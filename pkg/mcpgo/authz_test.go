package mcpgo

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseGroupList(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty"},
		{name: "whitespace only", raw: "   "},
		{name: "single", raw: "admins", want: []string{"admins"}},
		{
			name: "trims and drops blanks",
			raw:  " admins , , readers ,",
			want: []string{"admins", "readers"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ParseGroupList(tt.raw))
		})
	}
}

func TestGroupPolicyEvaluate(t *testing.T) {
	policy := GroupPolicy{
		ReadGroups:  []string{"linkwarden-readers"},
		WriteGroups: []string{"linkwarden-admins"},
	}

	tests := []struct {
		name   string
		policy GroupPolicy
		groups []string
		want   Permission
	}{
		{
			name:   "unconfigured policy grants write",
			policy: GroupPolicy{},
			groups: nil,
			want:   PermissionWrite,
		},
		{
			name:   "write group",
			policy: policy,
			groups: []string{"linkwarden-admins"},
			want:   PermissionWrite,
		},
		{
			name:   "read group",
			policy: policy,
			groups: []string{"linkwarden-readers"},
			want:   PermissionRead,
		},
		{
			name:   "write wins when in both",
			policy: policy,
			groups: []string{"linkwarden-readers", "linkwarden-admins"},
			want:   PermissionWrite,
		},
		{
			name:   "unmapped group denied",
			policy: policy,
			groups: []string{"some-other-team"},
			want:   PermissionNone,
		},
		{
			name:   "no groups denied",
			policy: policy,
			groups: nil,
			want:   PermissionNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.policy.Evaluate(tt.groups))
		})
	}
}

func TestPermissionFromContextDefaultsToWrite(t *testing.T) {
	// Nothing attached means no group policy applied, so the server-level
	// settings stay the only limit. Group policy narrows, never widens.
	assert.Equal(t, PermissionWrite, PermissionFromContext(context.Background()))

	ctx := WithPermission(context.Background(), PermissionRead)
	assert.Equal(t, PermissionRead, PermissionFromContext(ctx))
}

func TestGroupsFromClaims(t *testing.T) {
	claim := "groups"

	tests := []struct {
		name   string
		claims map[string]any
		want   []string
	}{
		{
			name:   "absent claim",
			claims: map[string]any{},
		},
		{
			name:   "json array decodes to []any",
			claims: map[string]any{claim: []any{"a", "b"}},
			want:   []string{"a", "b"},
		},
		{
			name:   "already a string slice",
			claims: map[string]any{claim: []string{"a", "b"}},
			want:   []string{"a", "b"},
		},
		{
			name:   "space delimited string",
			claims: map[string]any{claim: "a b"},
			want:   []string{"a", "b"},
		},
		{
			name:   "non-string entries are skipped",
			claims: map[string]any{claim: []any{"a", 42, "b"}},
			want:   []string{"a", "b"},
		},
		{
			name:   "unexpected type yields nothing",
			claims: map[string]any{claim: 42},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, groupsFromClaims(tt.claims, claim))
		})
	}
}

func TestGroupPolicyClaimName(t *testing.T) {
	assert.Equal(t, "groups", GroupPolicy{}.ClaimName())
	assert.Equal(t, "roles", GroupPolicy{Claim: "roles"}.ClaimName())
}
