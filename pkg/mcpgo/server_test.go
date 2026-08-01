package mcpgo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Over HTTP transport the mcp-go request structs embed the full header map,
// so logging one verbatim writes a replayable access token to disk
func TestRedactStripsCredentialHeaders(t *testing.T) {
	raw := "map[Accept:[application/json] " +
		"Authorization:[Bearer eyJhbGciOiJSUzI1NiJ9.payload.signature] " +
		"Cookie:[session=abc123] Content-Type:[application/json]]"

	got := redact(raw)

	assert.NotContains(t, got, "eyJhbGciOiJSUzI1NiJ9")
	assert.NotContains(t, got, "session=abc123")
	assert.Contains(t, got, "Authorization:[REDACTED]")
	assert.Contains(t, got, "Cookie:[REDACTED]")
	// non-sensitive headers must survive, or the log loses its value
	assert.Contains(t, got, "Accept:[application/json]")
	assert.Contains(t, got, "Content-Type:[application/json]")
}

func TestRedactIsCaseInsensitive(t *testing.T) {
	assert.Contains(t, redact("map[authorization:[Bearer secret]]"),
		"authorization:[REDACTED]")
}
