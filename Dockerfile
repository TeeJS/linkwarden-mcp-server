# syntax=docker/dockerfile:1

# Build stage
# Pinned to the build platform so cross-compilation is done by the Go
# toolchain rather than by emulating the target platform, which is both
# correct and far faster for the arm64 leg.
FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS builder

# Install build dependencies
RUN apk add --no-cache \
    make \
    bash \
    git \
    ca-certificates

# Set working directory
WORKDIR /build

# Copy go mod files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Install oapi-codegen. Pinned rather than @latest: an unpinned tool means a
# new upstream release can raise its Go floor and break this build with no
# change on our side, which is exactly what happened with v2.8.0.
ARG OAPI_CODEGEN_VERSION=v2.8.0
RUN go install \
    github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@${OAPI_CODEGEN_VERSION}

# Copy the rest of the source code
COPY . .

# Generate SDK from OpenAPI specification
RUN chmod +x ./scripts/generate-sdk.sh && \
    ./scripts/generate-sdk.sh

# Build metadata, supplied by the CI workflow. These must be declared for
# the corresponding --build-arg values to reach the build at all.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Provided automatically by buildx for the target platform
ARG TARGETOS
ARG TARGETARCH

# Build the binary with optimizations
# CGO_ENABLED=0 for static binary
# -ldflags for smaller binary size and embedded build metadata
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
    -ldflags="-w -s -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
    -o linkwarden-mcp-server \
    ./cmd/linkwarden-mcp-server

# Final stage - minimal Alpine image
FROM alpine:latest

# Install ca-certificates for HTTPS requests
RUN apk --no-cache add ca-certificates

# Create non-root user for security
RUN addgroup -g 1000 mcpserver && \
    adduser -D -u 1000 -G mcpserver mcpserver

# Set working directory
WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/linkwarden-mcp-server /app/linkwarden-mcp-server

# Change ownership to non-root user
RUN chown -R mcpserver:mcpserver /app

# Switch to non-root user
USER mcpserver

# Set default environment variables (can be overridden)
ENV LINKWARDEN_BASE_URL="" \
    LINKWARDEN_TOKEN="" \
    TOOLSETS="" \
    READ_ONLY="false" \
    LOG_FILE="" \
    MCP_HOST="0.0.0.0" \
    MCP_PORT="8080" \
    MCP_PATH="/mcp" \
    MCP_OAUTH_ENABLED="false" \
    MCP_OAUTH_ISSUER="" \
    MCP_SERVER_URL="" \
    MCP_OAUTH_AUDIENCE="" \
    MCP_OAUTH_JWKS_CACHE_TTL="3600"

# Streamable HTTP transport
EXPOSE 8080

# /healthz is served outside the auth gate so this works with OAuth enabled.
# wget comes from busybox; no need to add curl.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:${MCP_PORT:-8080}/healthz || exit 1

# The subcommand is part of CMD rather than ENTRYPOINT so it can be
# overridden: `docker run --rm -i <image> stdio` still speaks stdio.
ENTRYPOINT ["/app/linkwarden-mcp-server"]
CMD ["http"]

# Labels for metadata
LABEL org.opencontainers.image.title="Linkwarden MCP Server" \
      org.opencontainers.image.description="Model Context Protocol server for Linkwarden" \
      org.opencontainers.image.source="https://github.com/TeeJS/linkwarden-mcp-server" \
      org.opencontainers.image.vendor="TeeJS" \
      org.opencontainers.image.licenses="MIT"
