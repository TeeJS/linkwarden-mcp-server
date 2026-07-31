package mcpgo

import (
	"context"
	"strings"
)

// Permission is the level of access a caller has been granted for a request
type Permission int

const (
	// PermissionNone denies every tool
	PermissionNone Permission = iota
	// PermissionRead allows read tools only
	PermissionRead
	// PermissionWrite allows every tool
	PermissionWrite
)

func (p Permission) String() string {
	switch p {
	case PermissionRead:
		return "read"
	case PermissionWrite:
		return "write"
	default:
		return "none"
	}
}

// permissionContextKey is unexported so only this package can set it,
// which keeps a tool handler from forging its own permission.
type permissionContextKey struct{}

// WithPermission attaches a permission to the context
func WithPermission(ctx context.Context, p Permission) context.Context {
	return context.WithValue(ctx, permissionContextKey{}, p)
}

// PermissionFromContext reads the permission attached to the context.
//
// When nothing was attached, the caller was never subject to a group policy
// — either OAuth is off, or no groups were configured — so it reports
// PermissionWrite and the server-level READ_ONLY / TOOLSETS settings remain
// the only limits. Group policy narrows access; it never widens it.
func PermissionFromContext(ctx context.Context) Permission {
	if p, ok := ctx.Value(permissionContextKey{}).(Permission); ok {
		return p
	}

	return PermissionWrite
}

// GroupPolicy maps identity provider groups onto access levels.
//
// An empty policy disables group checking entirely, which is what makes the
// zero-config case work: any valid token gets whatever the server-level
// settings already allow.
type GroupPolicy struct {
	// Claim is the token claim holding the caller's groups
	Claim string
	// ReadGroups grant read tools only
	ReadGroups []string
	// WriteGroups grant every tool
	WriteGroups []string
}

// DefaultGroupsClaim is the claim Authelia and most OIDC providers use
const DefaultGroupsClaim = "groups"

// Configured reports whether any group mapping was supplied
func (g GroupPolicy) Configured() bool {
	return len(g.ReadGroups) > 0 || len(g.WriteGroups) > 0
}

// ClaimName returns the configured claim, defaulting to "groups"
func (g GroupPolicy) ClaimName() string {
	if g.Claim == "" {
		return DefaultGroupsClaim
	}

	return g.Claim
}

// Evaluate resolves the caller's groups to a permission. Write wins over
// read when a caller is in both.
func (g GroupPolicy) Evaluate(groups []string) Permission {
	if !g.Configured() {
		return PermissionWrite
	}

	held := make(map[string]bool, len(groups))
	for _, group := range groups {
		held[group] = true
	}

	for _, group := range g.WriteGroups {
		if held[group] {
			return PermissionWrite
		}
	}

	for _, group := range g.ReadGroups {
		if held[group] {
			return PermissionRead
		}
	}

	return PermissionNone
}

// ParseGroupList splits a comma-separated group list, trimming blanks
func ParseGroupList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	groups := make([]string, 0, len(parts))

	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			groups = append(groups, trimmed)
		}
	}

	return groups
}

// groupsFromClaims extracts a string list from an arbitrary claim value.
// Providers are inconsistent here: some emit a JSON array, some a single
// string, some a space-delimited string.
func groupsFromClaims(claims map[string]any, claim string) []string {
	raw, ok := claims[claim]
	if !ok {
		return nil
	}

	switch value := raw.(type) {
	case []string:
		return value
	case []any:
		groups := make([]string, 0, len(value))
		for _, item := range value {
			if s, ok := item.(string); ok {
				groups = append(groups, s)
			}
		}
		return groups
	case string:
		return strings.Fields(value)
	default:
		return nil
	}
}
