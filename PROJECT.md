# PROJECT.md — Remote MCP server for Linkwarden: Cowork-reachable, Unraid-hosted

Status: **awaiting go/no-go (v3 — pattern identified, no open questions)**
Created: 2026-07-31
Revised: 2026-07-31 — Cowork-on-the-web requirement supersedes the earlier "stdio only" decision.
Revised: 2026-07-31 — reference pattern corrected to `TeeJS/plex-mcp-server-docker`.

## Scope

Bring this fork in line with `TeeJS/plex-mcp-server-docker`: a long-lived containerized MCP
service on Unraid, published to GHCR by Actions, with an Unraid template, fronted by the
existing reverse proxy, and reachable by Claude Cowork on the web.

Correction to v2: v2 claimed Cowork could not reach a LAN-hosted MCP server. That was wrong
in practice — a reverse proxy already fronts these services, and Cowork reaches the Joplin
MCP today (confirmed by screenshot). The public-reachability requirement is real, but it is
already satisfied by existing infrastructure rather than being an unsolved problem.

## 1. What is the one thing this must do?

Expose Linkwarden to Claude Cowork on the web as a remote MCP connector, running as a
container on Unraid, updatable through the Unraid Docker tab.

## 2. What would be wrong if we shipped "working" software without it?

- A container that works from Claude Code on the LAN but that Cowork cannot reach. Anthropic
  connects from `160.79.104.0/21`, not from the user's browser, so a LAN address fails no
  matter how well it works locally. This is exactly the state joplin-mcp-server-docker is in.
- An internet-reachable endpoint with no authentication, handing anyone who finds it a
  Linkwarden token that can delete collections and links.
- Losing stdio. Claude Code locally should keep working unchanged.

## 3. What is explicitly off-limits as a workaround?

- Shipping the endpoint authless and relying on the URL being unguessable.
- Putting the token in the connector URL as a query parameter. The MCP authorization spec
  prohibits access tokens in the query string, and Anthropic's docs call it out specifically.
- Removing or breaking the `stdio` subcommand.
- Hand-building and hand-pushing images. Publishing stays automatic from CI.
- Telling the user to run `docker pull` by hand as the answer to "updates".
- Leaving the GHCR package private and papering over it with a `docker login` on Unraid.

## 4. Deployment target and backup location

- Image: `ghcr.io/teejs/linkwarden-mcp-server`
- Runtime: container on Unraid (SchmitzMegaplex), fronted by a public HTTPS hostname.
- Consumers: Claude Cowork / Claude.ai web (remote connector), Claude Code (either transport).
- Backup: this git repo. Clean at d69093b; all edits revertible. No separate file backups.

## 5. How will we verify it is done?

- [ ] Actions run completes green and publishes to GHCR.
- [ ] `docker run --rm ghcr.io/teejs/linkwarden-mcp-server:latest --version` prints real
      semver + commit + date, not the literal strings `version / commit / date`.
- [ ] `docker manifest inspect` shows only real platforms — no `unknown/unknown` entries.
- [ ] GHCR package is public; unauthenticated `docker pull` succeeds.
- [ ] Container runs on Unraid, stays up, and the Docker tab offers an update after a push.
- [ ] With OAuth off: `curl https://<public-host>/mcp` reaches the server through the proxy.
- [ ] With OAuth on: an unauthenticated request gets `401` carrying
      `WWW-Authenticate: Bearer resource_metadata="…"`, and both `.well-known` documents
      return valid JSON with `resource` matching the connector URL exactly.
- [ ] Cowork on the web lists Linkwarden tools and completes a real tool call.
- [ ] `docker run --rm -i <image> stdio` still speaks stdio for Claude Code.

## Architecture

    Cowork (web)  ──HTTPS──▶  reverse proxy  ──▶  Unraid container :8080/mcp
                              (already exists)      │  optional OAuth 2.1 resource server
                                                    ▼
                                               Linkwarden API

Auth mirrors `plex-mcp-server-docker`: off by default (`MCP_OAUTH_ENABLED=false`), which
behaves like the Joplin server — reachable through the proxy with no gate. Setting
`MCP_OAUTH_ENABLED=true` plus an issuer turns the server into an OAuth 2.1 resource server
that validates bearer JWTs against the existing OIDC provider. Nothing to decide up front;
the same image does both.

## Planned changes

### New — HTTP transport
- `pkg/mcpgo/streamable_http.go`: wrap `server.NewStreamableHTTPServer` from
  mark3labs/mcp-go v0.40.0 (already a dependency; default endpoint path is `/mcp`).
  The existing `TransportServer` interface is stdio-shaped (`Listen(ctx, in, out)`), so this
  gets its own interface with `Start(addr)` / `Shutdown(ctx)`.
- `pkg/mcpgo/oauth.go`: OAuth 2.1 resource-server middleware. Fetches the issuer's OIDC
  discovery document, caches JWKS, validates bearer JWT signature / `iss` / `aud` / `exp`.
  On failure returns `401` with
  `WWW-Authenticate: Bearer resource_metadata="<server>/.well-known/oauth-protected-resource"`,
  which is the handshake Claude requires — it does not honour the header on a `200`.
  Serves `/.well-known/oauth-protected-resource` and `/.well-known/oauth-authorization-server`.
  `resource` must equal `MCP_SERVER_URL` exactly as entered in the connector, path included.
- Unauthenticated `/healthz` for the container healthcheck, outside the gate.
- `httpCmd` in `main.go`, alongside the untouched `stdioCmd`. Flags mirror plex:
  `--address`, `--mcp-path`, `--oauth-enabled`, `--oauth-issuer`, `--server-url`,
  `--oauth-jwks-cache-ttl`. Refuses to start if OAuth is enabled without an issuer and
  server URL.

### Dockerfile
- Declare `ARG VERSION` / `COMMIT` / `BUILD_DATE` in the builder stage. Without these the
  workflow's `build-args` are silently discarded and the ldflags can only produce defaults.
  **This is the version-metadata fix.**
- `FROM --platform=$BUILDPLATFORM` + `ARG TARGETARCH` + `GOARCH=${TARGETARCH}`, replacing the
  hardcoded `GOARCH=amd64` on line 33. Fixes the broken arm64 image; also converts an
  emulated arm64 build into a native cross-compile, which is free since CGO is already off.
- `EXPOSE 8080`, `HEALTHCHECK` against `/healthz` using busybox wget (no curl needed on Alpine).
- `ENTRYPOINT ["/app/linkwarden-mcp-server"]` + `CMD ["http"]`.
  **Breaking change:** the container's default subcommand becomes `http` instead of `stdio`.
  stdio remains available as `docker run --rm -i <image> stdio`. Documented in the README.

### .github/workflows/docker-publish.yml
- `provenance: false`, `sbom: false` — keeps `unknown/unknown` attestation entries out of the
  manifest list, which is what confuses Unraid's update check and GHCR's package UI.
- Add `type=sha` tagging to match the joplin repo's `sha-<short>` convention.
- Reconcile tags with DOCKER.md, which currently promises a `main-<sha>` tag the workflow
  does not produce.

### New files, mirroring the sibling MCP repos
- `my-linkwarden-mcp.xml` — Unraid dockerMan template. `Mask="true"` on `LINKWARDEN_TOKEN`.
- `docker-compose.yml` + rewritten `.env.example` for local testing. The current
  `.env.example` is also wrong: it names `LINKWARDEN_API_TOKEN`, but `main.go` binds
  `LINKWARDEN_TOKEN`, so copying it as-is produces an unauthenticated client.
- `README-DOCKER.md` — Unraid install, updates, reverse-proxy route, Cowork connector setup.

## Group-based tool access

Identity provider is **Authelia**, not Authentik.

Groups map onto the read/write split the toolsets already have, via
`MCP_READ_GROUPS` / `MCP_WRITE_GROUPS`. Both empty disables group checking, so the
zero-config case still works. Enforced in two places: `tools/list` filtering, so a
read-only caller never sees write tools, and `tools/call`, because a client can
invoke a tool that was never listed. A valid token with no mapped group gets `403
insufficient_scope`, not `401` — re-authenticating would not help.

Group policy only narrows. `READ_ONLY=true` registers no write tools at all, so no
group can grant them.

### Split of responsibility

Only JWKS/issuer/audience/expiry validation and the group check live in this
server. PKCE (`enforce_pkce: always`), `authorization_policy: two_factor`, token
lifespans, and refresh tokens are all Authelia client config — a resource server
never sees any of them.

Two Authelia prerequisites that are **not defaults**:

- `access_token_signed_response_alg: 'RS256'` — Authelia issues opaque access
  tokens by default, which cannot be validated statelessly.
- a `claims_policies` entry listing `groups` under `access_token`.

## Decisions taken without asking (all reversible)

- **stdio kept, `http` added.** Additive; `runStdioServer` is untouched.
- **OAuth optional and off by default**, matching plex. Avoids a blocking decision and lets
  the same image serve the authless-behind-proxy case and the Cowork-with-consent case.
- **`GOARCH` fix included.** Two lines in a file already being edited; also converts the
  emulated arm64 build into a native cross-compile.
- **Container default subcommand becomes `http`.** stdio still available explicitly.

## Manual steps required from the user (cannot be automated)

1. **Enable Actions on the fork.** Zero workflow runs exist; GitHub suppresses workflows on
   forked repos until the owner confirms once in the Actions tab. Nothing publishes until
   this is done.
2. **Make the GHCR package public** after the first successful run. Packages default to
   private even from a public repo.
3. **Create the public hostname / tunnel route** per question 1.
4. **Add the connector in Cowork** and paste the bearer token.
