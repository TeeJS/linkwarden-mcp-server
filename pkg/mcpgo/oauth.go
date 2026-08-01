package mcpgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/irfansofyana/linkwarden-mcp-server/pkg/observability"
)

// ErrOAuthMisconfigured indicates the OAuth configuration is incomplete
var ErrOAuthMisconfigured = errors.New("oauth misconfigured")

// Well-known paths served when OAuth is enabled.
const (
	protectedResourcePath   = "/.well-known/oauth-protected-resource"
	authorizationServerPath = "/.well-known/oauth-authorization-server"
	defaultJWKSCacheTTL     = time.Hour
	discoveryRequestTimeout = 10 * time.Second
)

// OAuthConfig configures the OAuth 2.1 resource server
type OAuthConfig struct {
	// Enabled turns the resource server on. When false the MCP endpoint
	// is served without any authentication gate.
	Enabled bool
	// Issuer is the OIDC provider URL. It must match the issuer the
	// provider reports in its discovery document exactly, including any
	// trailing slash.
	Issuer string
	// ServerURL is the public base URL this server is reached on,
	// without a path (e.g. https://linkwarden-mcp.example.com)
	ServerURL string
	// MCPPath is the path the MCP endpoint is served on
	MCPPath string
	// Audience is the expected "aud" claim. When empty the audience
	// check is skipped.
	Audience string
	// JWKSCacheTTL is how long a resolved provider is reused before
	// discovery runs again
	JWKSCacheTTL time.Duration
	// Groups maps identity provider groups onto access levels. An empty
	// policy disables group checking.
	Groups GroupPolicy
}

// normalize trims surrounding whitespace from every string field.
//
// These arrive as environment variables, typically pasted by hand into a
// container UI, so a stray tab or trailing space is routine. Left alone it
// produces an unusable discovery URL and an error that points nowhere near
// the actual mistake.
func (c OAuthConfig) normalize() OAuthConfig {
	c.Issuer = strings.TrimSpace(c.Issuer)
	c.ServerURL = strings.TrimSuffix(strings.TrimSpace(c.ServerURL), "/")
	c.MCPPath = strings.TrimSpace(c.MCPPath)
	c.Audience = strings.TrimSpace(c.Audience)
	c.Groups.Claim = strings.TrimSpace(c.Groups.Claim)

	return c
}

// Validate checks that the configuration is usable
func (c OAuthConfig) Validate() error {
	c = c.normalize()

	if !c.Enabled {
		return nil
	}

	if c.Issuer == "" {
		return fmt.Errorf(
			"%w: oauth is enabled but no issuer was provided", ErrOAuthMisconfigured)
	}

	if c.ServerURL == "" {
		return fmt.Errorf(
			"%w: oauth is enabled but no public server url was provided",
			ErrOAuthMisconfigured)
	}

	if !strings.HasPrefix(c.ServerURL, "http://") &&
		!strings.HasPrefix(c.ServerURL, "https://") {
		return fmt.Errorf("%w: server url must include a scheme, got %q",
			ErrOAuthMisconfigured, c.ServerURL)
	}

	return nil
}

// ResourceServer implements the OAuth 2.1 protected resource half of the
// MCP authorization spec: it validates bearer tokens issued by an external
// OIDC provider and publishes the discovery documents clients use to find
// that provider.
type ResourceServer struct {
	cfg OAuthConfig
	obs *observability.Observability

	mu        sync.RWMutex
	verifier  *oidc.IDTokenVerifier
	issuerDoc json.RawMessage
	resolved  time.Time
}

// NewResourceServer creates an OAuth resource server. Provider discovery is
// deferred to the first request so that this server can start before the
// identity provider is reachable, which matters when both are containers
// coming up in an arbitrary order.
func NewResourceServer(
	cfg OAuthConfig, obs *observability.Observability,
) (*ResourceServer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	cfg = cfg.normalize()
	if cfg.MCPPath == "" {
		cfg.MCPPath = "/mcp"
	}
	if cfg.JWKSCacheTTL <= 0 {
		cfg.JWKSCacheTTL = defaultJWKSCacheTTL
	}

	return &ResourceServer{cfg: cfg, obs: obs}, nil
}

// ResourceURL is the canonical identifier for this protected resource. It
// must match the URL entered in the client exactly, path included.
func (rs *ResourceServer) ResourceURL() string {
	return rs.cfg.ServerURL + rs.cfg.MCPPath
}

// Issuer returns the normalized issuer, which is what discovery actually
// uses. Logging the raw config value instead is actively misleading when
// diagnosing a discovery failure.
func (rs *ResourceServer) Issuer() string {
	return rs.cfg.Issuer
}

// metadataURL is the absolute URL of the protected resource metadata
// document, advertised in WWW-Authenticate on a 401.
func (rs *ResourceServer) metadataURL() string {
	return rs.cfg.ServerURL + protectedResourcePath
}

// verifierFor returns a token verifier, running OIDC discovery if the
// cached one is missing or older than the configured TTL.
func (rs *ResourceServer) verifierFor(
	ctx context.Context,
) (*oidc.IDTokenVerifier, error) {
	rs.mu.RLock()
	if rs.verifier != nil && time.Since(rs.resolved) < rs.cfg.JWKSCacheTTL {
		v := rs.verifier
		rs.mu.RUnlock()
		return v, nil
	}
	rs.mu.RUnlock()

	rs.mu.Lock()
	defer rs.mu.Unlock()

	// Another goroutine may have resolved while we waited for the lock.
	if rs.verifier != nil && time.Since(rs.resolved) < rs.cfg.JWKSCacheTTL {
		return rs.verifier, nil
	}

	discoveryCtx, cancel := context.WithTimeout(ctx, discoveryRequestTimeout)
	defer cancel()

	provider, err := oidc.NewProvider(discoveryCtx, rs.cfg.Issuer)
	if err != nil {
		// A stale verifier is better than refusing every request while the
		// provider is briefly unreachable.
		if rs.verifier != nil {
			rs.obs.Logger.Warningf(ctx, "OAUTH_DISCOVERY_REFRESH_FAILED",
				"issuer", rs.cfg.Issuer,
				"error", err)
			return rs.verifier, nil
		}
		return nil, fmt.Errorf("oidc discovery against %q failed: %w",
			rs.cfg.Issuer, err)
	}

	var doc json.RawMessage
	if err := provider.Claims(&doc); err == nil {
		rs.issuerDoc = doc
	}

	rs.verifier = provider.Verifier(&oidc.Config{
		ClientID:          rs.cfg.Audience,
		SkipClientIDCheck: rs.cfg.Audience == "",
	})
	rs.resolved = time.Now()

	rs.obs.Logger.Infof(ctx, "OAUTH_DISCOVERY_SUCCEEDED",
		"issuer", rs.cfg.Issuer,
		"resource", rs.ResourceURL())

	return rs.verifier, nil
}

// challenge writes a 401 carrying the resource_metadata pointer Claude uses
// to locate the authorization server. The spec requires this on a 401
// specifically — the header is ignored on a 200.
func (rs *ResourceServer) challenge(
	w http.ResponseWriter, oauthErr, description string,
) {
	// The scope parameter is how a client is told which scopes to request.
	// Without it the client asks for everything the authorization server
	// advertises, which strict providers reject outright.
	header := fmt.Sprintf("Bearer resource_metadata=%q, scope=%q",
		rs.metadataURL(), strings.Join(requiredScopes, " "))
	if oauthErr != "" {
		header += fmt.Sprintf(", error=%q, error_description=%q",
			oauthErr, description)
	}

	w.Header().Set("WWW-Authenticate", header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)

	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             cmpOr(oauthErr, "unauthorized"),
		"error_description": cmpOr(description, "authentication required"),
	})
}

// Middleware gates a handler behind bearer token validation
func (rs *ResourceServer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		token, ok := bearerToken(r)
		if !ok {
			rs.challenge(w, "", "")
			return
		}

		verifier, err := rs.verifierFor(ctx)
		if err != nil {
			rs.obs.Logger.Errorf(ctx, "OAUTH_DISCOVERY_FAILED",
				"issuer", rs.cfg.Issuer,
				"error", err)
			rs.challenge(w, "temporarily_unavailable",
				"identity provider discovery failed")
			return
		}

		verified, err := verifier.Verify(ctx, token)
		if err != nil {
			rs.obs.Logger.Warningf(ctx, "OAUTH_TOKEN_REJECTED",
				"error", err)
			rs.challenge(w, "invalid_token", "the access token is not valid")
			return
		}

		if !rs.cfg.Groups.Configured() {
			next.ServeHTTP(w, r)
			return
		}

		var claims map[string]any
		if err := verified.Claims(&claims); err != nil {
			rs.obs.Logger.Warningf(ctx, "OAUTH_CLAIMS_UNREADABLE",
				"error", err)
			rs.challenge(w, "invalid_token", "token claims could not be read")
			return
		}

		groups := groupsFromClaims(claims, rs.cfg.Groups.ClaimName())
		permission := rs.cfg.Groups.Evaluate(groups)

		if permission == PermissionNone {
			// The token is valid; the caller simply lacks a mapped group.
			// That is 403, not 401 — re-authenticating would not help.
			rs.obs.Logger.Warningf(ctx, "AUTHZ_DENIED",
				"subject", claims["sub"],
				"claim", rs.cfg.Groups.ClaimName(),
				"groups", groups)
			rs.forbidden(w)
			return
		}

		// Logged at info, not debug: who was granted what access is an audit
		// record, and it is useless if it only appears when someone thinks to
		// raise the log level after the fact.
		rs.obs.Logger.Infof(ctx, "AUTHZ_GRANTED",
			"subject", claims["sub"],
			"permission", permission.String())

		next.ServeHTTP(w, r.WithContext(WithPermission(ctx, permission)))
	})
}

// forbidden rejects a caller holding a valid token but no mapped group
func (rs *ResourceServer) forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)

	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "insufficient_scope",
		"error_description": "your account is not in a group permitted to " +
			"use this server",
	})
}

// Handlers returns the unauthenticated discovery routes to register
// alongside the MCP endpoint.
func (rs *ResourceServer) Handlers() map[string]http.Handler {
	protected := http.HandlerFunc(rs.handleProtectedResource)

	handlers := map[string]http.Handler{
		protectedResourcePath:   protected,
		authorizationServerPath: http.HandlerFunc(rs.handleAuthorizationServer),
	}

	// Claude probes the path-suffixed location first, so serve the same
	// document there as well (e.g. /.well-known/oauth-protected-resource/mcp).
	if suffixed := protectedResourcePath + rs.cfg.MCPPath; suffixed !=
		protectedResourcePath {
		handlers[suffixed] = protected
	}

	return handlers
}

// requiredScopes is what this server actually needs: an identity, the group
// membership tool access is derived from, and a refresh token.
//
// Advertising these matters. A client that is not told which scopes to ask for
// falls back to requesting everything the authorization server advertises, and
// a provider that rejects — rather than ignores — a scope its client is not
// allowed to request will fail the whole authorization request. That surfaces
// as an opaque "authorization failed" with nothing useful on the client side.
var requiredScopes = []string{
	"openid",
	"profile",
	"groups",
	"offline_access",
}

// handleProtectedResource serves the RFC 9728 protected resource metadata
func (rs *ResourceServer) handleProtectedResource(
	w http.ResponseWriter, r *http.Request,
) {
	writeJSON(w, map[string]any{
		"resource":                 rs.ResourceURL(),
		"authorization_servers":    []string{rs.cfg.Issuer},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         requiredScopes,
	})
}

// handleAuthorizationServer mirrors the issuer's own discovery document.
// Serving it from this origin keeps discovery working for providers whose
// issuer lives on a nonstandard path, such as Authentik's
// /application/o/<slug>/.
func (rs *ResourceServer) handleAuthorizationServer(
	w http.ResponseWriter, r *http.Request,
) {
	if _, err := rs.verifierFor(r.Context()); err != nil {
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}

	rs.mu.RLock()
	doc := rs.issuerDoc
	rs.mu.RUnlock()

	if len(doc) == 0 {
		http.Error(w, "issuer metadata unavailable", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(doc)
}

// bearerToken extracts a bearer token from the Authorization header
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}

	const prefix = "bearer "
	if len(header) <= len(prefix) ||
		!strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}

	token := strings.TrimSpace(header[len(prefix):])

	return token, token != ""
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func cmpOr(value, fallback string) string {
	if value != "" {
		return value
	}

	return fallback
}
