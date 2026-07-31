# Linkwarden MCP — Docker / Unraid

Containerized fork of [`irfansofyana/linkwarden-mcp-server`](https://github.com/irfansofyana/linkwarden-mcp-server)
that additionally exposes the server over **Streamable HTTP**, so it can run as a
long-lived service on Unraid and be reached by remote MCP clients — including
Claude on the web, which connects from Anthropic's servers rather than from your
browser.

Both transports ship in the same image:

| Subcommand | Use                                                                 |
| ---------- | ------------------------------------------------------------------- |
| `http`     | Long-lived service. **This is the container default.**               |
| `stdio`    | Spawned per session by a local client: `docker run --rm -i <image> stdio` |

## Environment variables

| Variable                   | Required | Default   | Notes                                                                        |
| -------------------------- | -------- | --------- | ---------------------------------------------------------------------------- |
| `LINKWARDEN_BASE_URL`      | yes      | —         | Linkwarden instance URL, no trailing slash.                                  |
| `LINKWARDEN_TOKEN`         | yes      | —         | Linkwarden API token (Settings → Access Tokens).                             |
| `TOOLSETS`                 | no       | all       | Comma-separated: `search`, `collection`, `link`, `tags`.                     |
| `READ_ONLY`                | no       | `false`   | Disables every write tool.                                                   |
| `MCP_HOST`                 | no       | `0.0.0.0` | Bind address inside the container.                                           |
| `MCP_PATH`                 | no       | `/mcp`    | Path the MCP endpoint is served on.                                          |
| `MCP_OAUTH_ENABLED`        | no       | `false`   | Require OAuth 2.1 bearer tokens.                                             |
| `MCP_OAUTH_ISSUER`         | if OAuth | —         | OIDC provider URL. Must match the reported issuer exactly, trailing slash included. |
| `MCP_SERVER_URL`           | if OAuth | —         | Public base URL, no path. Connector URL is this + `MCP_PATH`.                |
| `MCP_OAUTH_AUDIENCE`       | no       | —         | Expected `aud` claim. Empty skips the audience check.                        |
| `MCP_OAUTH_JWKS_CACHE_TTL` | no       | `3600`    | Seconds to cache provider discovery.                                         |
| `MCP_GROUPS_CLAIM`         | no       | `groups`  | Token claim holding the caller's groups.                                     |
| `MCP_READ_GROUPS`          | no       | —         | Comma-separated groups granted read tools only.                              |
| `MCP_WRITE_GROUPS`         | no       | —         | Comma-separated groups granted every tool.                                   |

## Run with docker-compose (local test)

```bash
cp .env.example .env       # fill in LINKWARDEN_BASE_URL + LINKWARDEN_TOKEN
docker compose up --build
```

The endpoint is then `http://localhost:8080/mcp`, and `http://localhost:8080/healthz`
should return `{"status":"ok"}`.

## Image builds (GitHub Actions → GHCR)

`.github/workflows/docker-publish.yml` builds on every push to `main` and every
`v*.*.*` tag, for `linux/amd64` (the Unraid target; arm64 is not built):

```
ghcr.io/teejs/linkwarden-mcp-server:latest
ghcr.io/teejs/linkwarden-mcp-server:sha-<short>
ghcr.io/teejs/linkwarden-mcp-server:<semver>   # on tags
```

Two one-time setup steps:

1. **Enable Actions on the fork.** GitHub suppresses workflows on forked repos
   until the owner confirms once in the Actions tab. Until then nothing builds.
2. **Check the package is public**, so Unraid can pull without credentials.
   Verify with an unauthenticated pull from a machine that has never logged in to
   ghcr.io. If it fails, go to **GitHub → Packages → linkwarden-mcp-server →
   Package settings → Change visibility → Public** (or keep it private and
   `docker login ghcr.io` on the Unraid host).

Build metadata is embedded via ldflags, so `docker run --rm <image> --version`
reports the real version, commit, and build date.

## Run on Unraid

1. Make sure the image has been built at least once.
2. Copy `my-linkwarden-mcp.xml` to `/boot/config/plugins/dockerMan/templates-user/`
   on the Unraid host.
3. Docker tab → **Add Container** → pick the `linkwarden-mcp` template → fill in
   `LINKWARDEN_BASE_URL` and `LINKWARDEN_TOKEN` → **Apply**.
4. Check the host port. The template defaults to `8080`; change it if something
   else on the box already has it.
5. LAN endpoint: `http://<unraid-ip>:8095/mcp`.

### Updates

The Docker tab shows "update ready" when the `:latest` digest changes. Click
**Force Update** (or Apply on the container) to pull and recreate. The workflow
publishes without provenance/SBOM attestations specifically so the manifest stays
clean — attestation entries show up as `unknown/unknown` platforms and interfere
with Unraid's digest comparison.

## Remote access from Claude

Claude connects from Anthropic's egress range (`160.79.104.0/21`), not from your
machine, so the endpoint has to be reachable from the public internet. Put it
behind the reverse proxy the same way as the other MCP services, then add it at
**claude.ai → Settings → Connectors → Add custom connector** with the URL
`https://<public-host>/mcp`.

### Turning OAuth on

Anything internet-reachable should have `MCP_OAUTH_ENABLED=true`. Without it, the
endpoint hands anyone who finds it full use of your Linkwarden token, including
the delete tools.

1. In your identity provider, create an OAuth application for this server.
   Register the redirect URI `https://claude.ai/api/mcp/auth_callback`.
2. Set on the container:
   - `MCP_OAUTH_ENABLED=true`
   - `MCP_OAUTH_ISSUER=<the issuer URL your provider reports>`
   - `MCP_SERVER_URL=https://<public-host>`
3. Add the connector in Claude and supply the OAuth Client ID and Secret under
   **Advanced settings**.

When OAuth is enabled the server publishes:

- `/.well-known/oauth-protected-resource` (and the path-suffixed variant Claude
  probes first)
- `/.well-known/oauth-authorization-server`, mirrored from the issuer's own
  discovery document

and answers unauthenticated requests with `401` plus
`WWW-Authenticate: Bearer resource_metadata="…"`, which is how Claude locates the
authorization server.

`/healthz` stays outside the gate so the container healthcheck keeps working.

### Group-based tool access

Groups from the identity provider map onto the read/write split the toolsets
already have:

```bash
MCP_READ_GROUPS=linkwarden-readers
MCP_WRITE_GROUPS=linkwarden-admins
```

- A caller in a **write** group gets every enabled tool.
- A caller in a **read** group gets the read tools; write tools are hidden from
  `tools/list` *and* refused at `tools/call`. Both halves are enforced, because a
  client can invoke a tool that was never listed.
- A caller in neither gets `403 insufficient_scope`. The token is valid — they
  just have no mapped group — so it is deliberately not a `401`.
- Leaving both empty disables group checking entirely.

Group policy only narrows access. With `READ_ONLY=true` the server registers no
write tools at all, so a write group grants nothing extra.

**Authelia note.** Authelia issues **opaque access tokens by default**, which this
server cannot validate. Set `access_token_signed_response_alg: 'RS256'` on the
client to get RFC 9068 JWTs. Getting `groups` into the access token additionally
needs a `claims_policies` entry listing it under `access_token` — Authelia's docs
steer you toward `/userinfo` instead, on the grounds that *clients* must not
inspect access tokens. That rule is about clients; this is a resource server, which
is exactly what RFC 9068 JWT access tokens are for.

### Troubleshooting

- **Every request 401s.** The issuer string must match what the provider reports
  in its discovery document byte for byte. Authentik reports its issuer *with* a
  trailing slash (`…/application/o/<slug>/`); dropping it fails discovery. The
  container logs `OAUTH_DISCOVERY_FAILED` with the underlying mismatch.
- **Claude says it can't reach the server.** Check the endpoint from outside your
  network, not from the LAN. Also confirm `MCP_SERVER_URL` matches the URL you
  entered in the connector — the `resource` field in the metadata document has to
  agree with it exactly, path included.
- **Opaque tokens.** This server validates JWT access tokens. If your provider
  issues opaque tokens instead, configure it to return JWTs for this application.

## Using stdio instead

Unchanged from upstream, for a local client that spawns the process itself:

```json
{
  "mcpServers": {
    "linkwarden": {
      "command": "docker",
      "args": [
        "run", "--rm", "-i", "--init",
        "-e", "LINKWARDEN_BASE_URL=https://your-linkwarden-instance.com",
        "-e", "LINKWARDEN_TOKEN=your-api-token-here",
        "ghcr.io/teejs/linkwarden-mcp-server:latest",
        "stdio"
      ]
    }
  }
}
```

Note the trailing `stdio`. Without it the container starts the HTTP server, since
that is now the default command.
