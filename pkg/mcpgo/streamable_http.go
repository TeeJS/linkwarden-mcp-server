package mcpgo

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// HTTPTransportServer defines a server that listens for MCP connections
// over HTTP. It is deliberately separate from TransportServer, whose
// Listen(ctx, in, out) signature only makes sense for stdio.
type HTTPTransportServer interface {
	// Start begins serving on addr and blocks until the server stops
	Start(addr string) error
	// Shutdown gracefully stops the server
	Shutdown(ctx context.Context) error
}

// StreamableHTTPConfig configures the streamable HTTP transport
type StreamableHTTPConfig struct {
	// EndpointPath is the path the MCP endpoint is served on
	EndpointPath string
	// Middleware wraps the MCP endpoint only. Health and discovery
	// routes are registered outside of it so they stay reachable.
	Middleware func(http.Handler) http.Handler
	// ExtraHandlers are additional unauthenticated routes, keyed by path
	ExtraHandlers map[string]http.Handler
}

// NewStreamableHTTPServer creates a new streamable HTTP transport server.
//
// The underlying mcp-go server is used as a plain http.Handler rather than
// through its own Start method, because we need to register additional
// routes (health, OAuth discovery) alongside the MCP endpoint.
func NewStreamableHTTPServer(
	mcpServer Server, cfg StreamableHTTPConfig,
) (*mark3labsStreamableHTTPImpl, error) {
	sImpl, ok := mcpServer.(*Mark3labsImpl)
	if !ok {
		return nil, fmt.Errorf("%w: expected *Mark3labsImpl, got %T",
			ErrInvalidServerImplementation, mcpServer)
	}

	if cfg.EndpointPath == "" {
		cfg.EndpointPath = "/mcp"
	}

	streamableSrv := server.NewStreamableHTTPServer(
		sImpl.McpServer,
		server.WithEndpointPath(cfg.EndpointPath),
	)

	mux := http.NewServeMux()

	var mcpHandler http.Handler = streamableSrv
	if cfg.Middleware != nil {
		mcpHandler = cfg.Middleware(mcpHandler)
	}
	mux.Handle(cfg.EndpointPath, mcpHandler)

	for path, handler := range cfg.ExtraHandlers {
		mux.Handle(path, handler)
	}

	return &mark3labsStreamableHTTPImpl{
		mcpStreamableServer: streamableSrv,
		httpServer: &http.Server{
			Handler: mux,
			// Streamable HTTP holds long-lived SSE responses open, so a
			// write timeout would sever active streams. Guard the request
			// header read instead.
			ReadHeaderTimeout: 10 * time.Second,
		},
	}, nil
}

// mark3labsStreamableHTTPImpl implements the HTTPTransportServer
// interface for streamable HTTP transport
type mark3labsStreamableHTTPImpl struct {
	mcpStreamableServer *server.StreamableHTTPServer
	httpServer          *http.Server
}

// Start implements the HTTPTransportServer interface
func (s *mark3labsStreamableHTTPImpl) Start(addr string) error {
	s.httpServer.Addr = addr

	if err := s.httpServer.ListenAndServe(); err != nil &&
		err != http.ErrServerClosed {
		return err
	}

	return nil
}

// Shutdown implements the HTTPTransportServer interface
func (s *mark3labsStreamableHTTPImpl) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
