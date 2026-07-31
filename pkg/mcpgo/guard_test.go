package mcpgo

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testWriteTools = map[string]bool{
	"create_link":       true,
	"delete_link_by_id": true,
}

func toolNames(tools []mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}

	return names
}

func TestWriteToolFilter(t *testing.T) {
	all := []mcp.Tool{
		{Name: "get_all_links"},
		{Name: "create_link"},
		{Name: "delete_link_by_id"},
		{Name: "search_links"},
	}

	filter := writeToolFilter(testWriteTools)

	t.Run("write permission sees everything", func(t *testing.T) {
		ctx := WithPermission(context.Background(), PermissionWrite)
		assert.Equal(t,
			[]string{"get_all_links", "create_link", "delete_link_by_id", "search_links"},
			toolNames(filter(ctx, all)))
	})

	t.Run("read permission sees only read tools", func(t *testing.T) {
		ctx := WithPermission(context.Background(), PermissionRead)
		assert.Equal(t,
			[]string{"get_all_links", "search_links"},
			toolNames(filter(ctx, all)))
	})

	t.Run("no permission in context sees everything", func(t *testing.T) {
		// No group policy applied; server-level settings still govern
		assert.Equal(t, 4, len(filter(context.Background(), all)))
	})
}

func TestWriteToolMiddleware(t *testing.T) {
	var reached bool
	next := func(
		context.Context, mcp.CallToolRequest,
	) (*mcp.CallToolResult, error) {
		reached = true
		return mcp.NewToolResultText("ok"), nil
	}

	guarded := writeToolMiddleware(testWriteTools)(next)

	call := func(name string) mcp.CallToolRequest {
		req := mcp.CallToolRequest{}
		req.Params.Name = name
		return req
	}

	t.Run("read permission cannot call a write tool", func(t *testing.T) {
		reached = false
		ctx := WithPermission(context.Background(), PermissionRead)

		result, err := guarded(ctx, call("create_link"))
		require.NoError(t, err)

		assert.False(t, reached, "handler must not run")
		assert.True(t, result.IsError)
	})

	t.Run("read permission can call a read tool", func(t *testing.T) {
		reached = false
		ctx := WithPermission(context.Background(), PermissionRead)

		result, err := guarded(ctx, call("get_all_links"))
		require.NoError(t, err)

		assert.True(t, reached)
		assert.False(t, result.IsError)
	})

	t.Run("write permission can call a write tool", func(t *testing.T) {
		reached = false
		ctx := WithPermission(context.Background(), PermissionWrite)

		result, err := guarded(ctx, call("delete_link_by_id"))
		require.NoError(t, err)

		assert.True(t, reached)
		assert.False(t, result.IsError)
	})

	// An unlisted tool is still invocable by a client, so the call gate has
	// to stand on its own rather than trusting tools/list to have hidden it
	t.Run("gate does not rely on the filter", func(t *testing.T) {
		reached = false
		ctx := WithPermission(context.Background(), PermissionRead)

		_, err := guarded(ctx, call("delete_link_by_id"))
		require.NoError(t, err)
		assert.False(t, reached)
	})
}

func TestWriteToolGuardNoOpWithoutWriteTools(t *testing.T) {
	// Globally read-only servers register no write tools at all
	assert.Nil(t, WithWriteToolGuard(nil))
	assert.Nil(t, WithWriteToolGuard(map[string]bool{}))
}
