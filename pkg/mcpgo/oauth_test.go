package mcpgo

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/irfansofyana/linkwarden-mcp-server/pkg/observability"
)

// noopLogger satisfies log.Logger without touching the filesystem
type noopLogger struct{}

func (noopLogger) Infof(context.Context, string, ...interface{})    {}
func (noopLogger) Errorf(context.Context, string, ...interface{})   {}
func (noopLogger) Fatalf(context.Context, string, ...interface{})   {}
func (noopLogger) Debugf(context.Context, string, ...interface{})   {}
func (noopLogger) Warningf(context.Context, string, ...interface{}) {}
func (noopLogger) Close() error                                     { return nil }

func testObs() *observability.Observability {
	return observability.New(observability.WithLogging(noopLogger{}))
}

// mockIDP is a minimal OIDC provider: discovery document, JWKS, and the
// ability to mint signed tokens against them.
type mockIDP struct {
	server *httptest.Server
	key    *rsa.PrivateKey
}

func newMockIDP(t *testing.T) *mockIDP {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	idp := &mockIDP{key: key}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                                idp.server.URL,
				"authorization_endpoint":                idp.server.URL + "/auth",
				"token_endpoint":                        idp.server.URL + "/token",
				"jwks_uri":                              idp.server.URL + "/jwks",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{
			Keys: []jose.JSONWebKey{{
				Key:       key.Public(),
				KeyID:     "test-key",
				Algorithm: "RS256",
				Use:       "sig",
			}},
		})
	})

	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)

	return idp
}

func (m *mockIDP) token(t *testing.T, audience string, expiry time.Time) string {
	t.Helper()

	return m.tokenWithType(t, "JWT", audience, expiry)
}

func (m *mockIDP) tokenWithType(
	t *testing.T, typ, audience string, expiry time.Time,
) string {
	t.Helper()

	return m.tokenWithClaims(t, typ, map[string]any{
		"iss": m.server.URL,
		"sub": "user-1",
		"aud": audience,
		"iat": time.Now().Unix(),
		"exp": expiry.Unix(),
	})
}

func (m *mockIDP) tokenWithClaims(
	t *testing.T, typ string, claims map[string]any,
) string {
	t.Helper()

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: m.key},
		(&jose.SignerOptions{}).WithType(jose.ContentType(typ)).
			WithHeader("kid", "test-key"),
	)
	require.NoError(t, err)

	payload, err := json.Marshal(claims)
	require.NoError(t, err)

	obj, err := signer.Sign(payload)
	require.NoError(t, err)

	raw, err := obj.CompactSerialize()
	require.NoError(t, err)

	return raw
}

func TestOAuthConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  OAuthConfig
		wantErr bool
	}{
		{
			name:   "disabled needs nothing",
			config: OAuthConfig{Enabled: false},
		},
		{
			name: "enabled without issuer",
			config: OAuthConfig{
				Enabled:   true,
				ServerURL: "https://mcp.example.com",
			},
			wantErr: true,
		},
		{
			name: "enabled without server url",
			config: OAuthConfig{
				Enabled: true,
				Issuer:  "https://auth.example.com",
			},
			wantErr: true,
		},
		{
			name: "server url without scheme",
			config: OAuthConfig{
				Enabled:   true,
				Issuer:    "https://auth.example.com",
				ServerURL: "mcp.example.com",
			},
			wantErr: true,
		},
		{
			name: "fully configured",
			config: OAuthConfig{
				Enabled:   true,
				Issuer:    "https://auth.example.com",
				ServerURL: "https://mcp.example.com",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrOAuthMisconfigured))
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		wantToken string
		wantOK    bool
	}{
		{name: "absent header"},
		{name: "wrong scheme", header: "Basic abc123"},
		{name: "scheme only", header: "Bearer"},
		{name: "empty token", header: "Bearer   "},
		{
			name:      "valid",
			header:    "Bearer abc123",
			wantToken: "abc123",
			wantOK:    true,
		},
		{
			name:      "scheme is case insensitive",
			header:    "bearer abc123",
			wantToken: "abc123",
			wantOK:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}

			token, ok := bearerToken(req)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantToken, token)
		})
	}
}

func TestResourceURLIncludesMCPPath(t *testing.T) {
	rs, err := NewResourceServer(OAuthConfig{
		Enabled: true,
		Issuer:  "https://auth.example.com",
		// trailing slash should not produce a doubled separator
		ServerURL: "https://mcp.example.com/",
		MCPPath:   "/mcp",
	}, testObs())
	require.NoError(t, err)

	assert.Equal(t, "https://mcp.example.com/mcp", rs.ResourceURL())
	assert.Equal(t,
		"https://mcp.example.com/.well-known/oauth-protected-resource",
		rs.metadataURL())
}

func TestProtectedResourceMetadata(t *testing.T) {
	rs, err := NewResourceServer(OAuthConfig{
		Enabled:   true,
		Issuer:    "https://auth.example.com/application/o/linkwarden/",
		ServerURL: "https://mcp.example.com",
		MCPPath:   "/mcp",
	}, testObs())
	require.NoError(t, err)

	handlers := rs.Handlers()
	// Claude probes the path-suffixed location first, then the bare one
	require.Contains(t, handlers, "/.well-known/oauth-protected-resource")
	require.Contains(t, handlers, "/.well-known/oauth-protected-resource/mcp")

	rec := httptest.NewRecorder()
	handlers["/.well-known/oauth-protected-resource"].ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet,
			"/.well-known/oauth-protected-resource", nil))

	require.Equal(t, http.StatusOK, rec.Code)

	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		BearerMethods        []string `json:"bearer_methods_supported"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))

	// Must match the URL entered in the client exactly, path included
	assert.Equal(t, "https://mcp.example.com/mcp", doc.Resource)
	assert.Equal(t,
		[]string{"https://auth.example.com/application/o/linkwarden/"},
		doc.AuthorizationServers)
	assert.Equal(t, []string{"header"}, doc.BearerMethods)
}

func TestMiddlewareRejectsMissingToken(t *testing.T) {
	rs, err := NewResourceServer(OAuthConfig{
		Enabled:   true,
		Issuer:    "https://auth.example.com",
		ServerURL: "https://mcp.example.com",
		MCPPath:   "/mcp",
	}, testObs())
	require.NoError(t, err)

	var called bool
	handler := rs.Middleware(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { called = true }))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	assert.False(t, called, "handler must not run without a token")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	// Claude only honours this header on a 401, and needs the pointer to
	// locate the authorization server
	assert.Equal(t,
		`Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`,
		rec.Header().Get("WWW-Authenticate"))
}

func TestMiddlewareWithMockIDP(t *testing.T) {
	idp := newMockIDP(t)

	newServer := func(t *testing.T, audience string) http.Handler {
		t.Helper()

		rs, err := NewResourceServer(OAuthConfig{
			Enabled:   true,
			Issuer:    idp.server.URL,
			ServerURL: "https://mcp.example.com",
			MCPPath:   "/mcp",
			Audience:  audience,
		}, testObs())
		require.NoError(t, err)

		return rs.Middleware(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("reached"))
			}))
	}

	t.Run("valid token is accepted", func(t *testing.T) {
		handler := newServer(t, "linkwarden-mcp")
		token := idp.token(t, "linkwarden-mcp", time.Now().Add(time.Hour))

		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "reached", rec.Body.String())
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		handler := newServer(t, "linkwarden-mcp")
		token := idp.token(t, "linkwarden-mcp", time.Now().Add(-time.Hour))

		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`)
	})

	t.Run("wrong audience is rejected", func(t *testing.T) {
		handler := newServer(t, "linkwarden-mcp")
		token := idp.token(t, "some-other-service", time.Now().Add(time.Hour))

		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("audience check skipped when unset", func(t *testing.T) {
		handler := newServer(t, "")
		token := idp.token(t, "anything-at-all", time.Now().Add(time.Hour))

		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	// Authelia issues RFC 9068 access tokens, whose header is typ: at+jwt
	// rather than typ: JWT. Verification must not care.
	t.Run("rfc9068 at+jwt token is accepted", func(t *testing.T) {
		handler := newServer(t, "linkwarden-mcp")
		token := idp.tokenWithType(
			t, "at+jwt", "linkwarden-mcp", time.Now().Add(time.Hour))

		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("garbage token is rejected", func(t *testing.T) {
		handler := newServer(t, "")

		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer not-a-jwt")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}

func TestMiddlewareGroupPolicy(t *testing.T) {
	idp := newMockIDP(t)

	// Captures the permission the middleware attached to the request
	newServer := func(t *testing.T) (http.Handler, *Permission) {
		t.Helper()

		rs, err := NewResourceServer(OAuthConfig{
			Enabled:   true,
			Issuer:    idp.server.URL,
			ServerURL: "https://mcp.example.com",
			MCPPath:   "/mcp",
			Groups: GroupPolicy{
				ReadGroups:  []string{"linkwarden-readers"},
				WriteGroups: []string{"linkwarden-admins"},
			},
		}, testObs())
		require.NoError(t, err)

		var granted Permission
		handler := rs.Middleware(http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				granted = PermissionFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			}))

		return handler, &granted
	}

	tokenWithGroups := func(groups any) string {
		return idp.tokenWithClaims(t, "at+jwt", map[string]any{
			"iss":    idp.server.URL,
			"sub":    "user-1",
			"aud":    "linkwarden-mcp",
			"iat":    time.Now().Unix(),
			"exp":    time.Now().Add(time.Hour).Unix(),
			"groups": groups,
		})
	}

	do := func(handler http.Handler, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		return rec
	}

	t.Run("admin group grants write", func(t *testing.T) {
		handler, granted := newServer(t)
		rec := do(handler, tokenWithGroups([]string{"linkwarden-admins"}))

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, PermissionWrite, *granted)
	})

	t.Run("reader group grants read", func(t *testing.T) {
		handler, granted := newServer(t)
		rec := do(handler, tokenWithGroups([]string{"linkwarden-readers"}))

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, PermissionRead, *granted)
	})

	// A valid token with no mapped group is an authorization failure, not
	// an authentication one — re-authenticating would not change anything
	t.Run("unmapped group is 403 not 401", func(t *testing.T) {
		handler, _ := newServer(t)
		rec := do(handler, tokenWithGroups([]string{"some-other-team"}))

		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), "insufficient_scope")
	})

	t.Run("missing groups claim is 403", func(t *testing.T) {
		handler, _ := newServer(t)
		token := idp.tokenWithType(
			t, "at+jwt", "linkwarden-mcp", time.Now().Add(time.Hour))

		assert.Equal(t, http.StatusForbidden, do(handler, token).Code)
	})
}

func TestAuthorizationServerMetadataMirrorsIssuer(t *testing.T) {
	idp := newMockIDP(t)

	rs, err := NewResourceServer(OAuthConfig{
		Enabled:   true,
		Issuer:    idp.server.URL,
		ServerURL: "https://mcp.example.com",
		MCPPath:   "/mcp",
	}, testObs())
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	rs.Handlers()["/.well-known/oauth-authorization-server"].ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet,
			"/.well-known/oauth-authorization-server", nil))

	require.Equal(t, http.StatusOK, rec.Code)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	assert.Equal(t, idp.server.URL, doc["issuer"])
	assert.Equal(t, idp.server.URL+"/jwks", doc["jwks_uri"])
}
