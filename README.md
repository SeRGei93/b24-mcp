# bitrix24-mcp-server

A secure-by-default, self-hosted [Model Context Protocol](https://modelcontextprotocol.io/) server for Bitrix24 CRM. It discovers portal-specific schemas and smart-process automations (SPAs), reads CRM data, and optionally performs confirmation-gated writes.

This is an independent open-source project and is not an official Bitrix24 product. Test it against a non-production portal before using it in production. Effective access always depends on the Bitrix24 OAuth user's permissions and the OAuth application's scopes.

## What is included

- Bring-your-own Bitrix24 OAuth application with an automatic MCP-to-Bitrix24 authorization-code flow and refresh tokens.
- AES-256-GCM token encryption in PostgreSQL and concurrency-safe automatic refresh.
- Read-only tools for connections, entity types, fields, funnels, stages, users, and CRM items.
- Dynamic discovery for SPAs through `crm.type.list` and universal `crm.item.*` methods.
- Schema metadata cache with explicit invalidation.
- Prepared create/update/stage-move operations that require a separate confirmation call.
- Audit records for prepared, completed, failed, and cancelled writes.
- Stateless Streamable HTTP MCP transport with OAuth 2.1, PKCE, dynamic client registration, refresh tokens, and legacy static-bearer compatibility.
- Docker Compose and Coolify-ready deployment.

Raw REST calls, deletion, schema administration, robots, Bizproc, and event binding are deliberately not exposed in this release.

## Quick start

Requirements: Node.js 20.11+, PostgreSQL 14+ (16 recommended), and a local/public Bitrix24 OAuth application with CRM scope.

Version 0.3.1 accepts hosted `*.bitrix24.*` cloud portal domains and the exact custom portal configured through `BITRIX_PORTAL_DOMAIN`. Other custom and on-premise domains remain rejected.

```bash
cp .env.example .env
npm ci
npm run build
npm start
```

Create the two secrets shown in `.env.example`; keep `TOKEN_ENCRYPTION_KEY` stable for the lifetime of the database. Configure the Bitrix24 local application handler as:

```text
https://your-server.example.com/oauth/bitrix/callback
```

Set `BITRIX_PORTAL_DOMAIN` to the hosted or custom portal URL, such as `your-portal.bitrix24.com` or `https://crm.example.com/`. A custom domain is accepted only when it matches this operator-managed value exactly. The variable can be omitted when the installation callback has already stored exactly one active hosted portal in the database.

The server applies its database migration automatically at startup. Start authorization in a browser while sending the MCP bearer token:

```bash
curl -i \
  -H "Authorization: Bearer $MCP_AUTH_TOKEN" \
  "https://your-server.example.com/oauth/bitrix/start?domain=your-portal.bitrix24.com"
```

Follow the `Location` redirect. The callback returns a safe connection object containing the `id` used by MCP tools; it never returns OAuth tokens.

### API-only local application installation

For a Bitrix24 local server-side application with **Uses API only** enabled, configure:

```text
Handler path:               https://your-server.example.com/oauth/bitrix/callback
Initial installation path: https://your-server.example.com/oauth/bitrix/install
```

Bitrix24 sends the initial token pair as a form-encoded `auth` object. The installation endpoint validates that token against the portal's `app.info` method before encrypting and storing it. A deployment may initially use a `pending-*` client ID while Bitrix24 creates the local app; replace the placeholder client ID and secret immediately after Bitrix24 displays them so refresh-token renewal can work.

## Connect an MCP client

The recommended remote flow is federated OAuth. The server publishes OAuth 2.1 protected-resource and authorization-server metadata, supports dynamic client registration (DCR), and requires S256 PKCE. Its `/authorize` endpoint redirects directly to the configured Bitrix24 portal. After the user logs in and grants access, Bitrix24 returns to the server, the server stores the encrypted Bitrix24 token pair, and the browser returns automatically to the MCP client's callback. No MCP token is typed into a browser.

Every OAuth access token issued to an MCP client is bound to the Bitrix24 connection and user profile that completed authorization. That client can list and operate only on its bound connection. The static `MCP_AUTH_TOKEN` remains an operator-only compatibility and recovery mechanism.

Use this Streamable HTTP URL:

```text
https://your-server.example.com/mcp
```

### Claude custom connector

Add the MCP URL and leave the advanced OAuth Client ID and Client Secret fields empty. Claude registers itself through DCR. Select **Connect**; the browser goes to Bitrix24 for login/consent and then returns automatically to Claude.

### Claude Code

Claude Code can use OAuth:

```bash
claude mcp add --transport http bitrix24 https://your-server.example.com/mcp
```

Open `/mcp` inside Claude Code and complete the Bitrix24 login/consent. For an operator-only static-token setup, add `--header "Authorization: Bearer <MCP_AUTH_TOKEN>"` to the command.

### Codex

Add the remote server, then start the OAuth login:

```bash
codex mcp add bitrix24 --url https://your-server.example.com/mcp
codex mcp login bitrix24
```

Codex can also retain static-bearer compatibility through `~/.codex/config.toml`:

```toml
[mcp_servers.bitrix24]
url = "https://your-server.example.com/mcp"
bearer_token_env_var = "BITRIX24_MCP_TOKEN"
```

### Generic static-bearer clients

Clients that can set arbitrary headers may continue to send:

```text
Authorization: Bearer <MCP_AUTH_TOKEN>
```

Example client configuration (exact keys vary by MCP client):

```json
{
  "mcpServers": {
    "bitrix24": {
      "url": "https://your-server.example.com/mcp",
      "headers": {
        "Authorization": "Bearer replace-with-your-token"
      }
    }
  }
}
```

For local stdio use, set `MCP_TRANSPORT=stdio` and run `node dist/index.js`. OAuth still needs the HTTP transport temporarily or an existing connection in the same database. Diagnostic logs go to stderr so they do not corrupt MCP stdio messages.

## Tools

Read-only:

- `bitrix_list_connections`
- `bitrix_discover_portal`
- `bitrix_list_crm_entities`
- `bitrix_describe_entity`
- `bitrix_list_fields`
- `bitrix_list_categories`
- `bitrix_list_stages`
- `bitrix_refresh_schema_cache`
- `bitrix_search_items`
- `bitrix_get_item`
- `bitrix_list_users`

Confirmation-gated writes:

- `bitrix_prepare_create_item`
- `bitrix_prepare_update_item`
- `bitrix_prepare_move_item_stage`
- `bitrix_confirm_operation`
- `bitrix_cancel_operation`
- `bitrix_list_pending_operations`

The write workflow is intentionally two-step:

```text
prepare request -> pending operation and human summary -> explicit confirm -> Bitrix24 call -> audit record
```

To allow preparation for a portal, initiate OAuth with `?mode=write_with_confirmation`. The default `read_only` mode denies preparation as well as execution. Confirmation is atomic: concurrent confirmation requests cannot execute one operation twice. Pending operations expire after ten minutes by default.

## Configuration

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `APP_URL` | yes | — | Public server URL; HTTPS except localhost |
| `DATABASE_URL` | yes | — | PostgreSQL connection string |
| `DATABASE_SSL` | no | `false` | Require a verified TLS PostgreSQL connection |
| `BITRIX_CLIENT_ID` | yes | — | Deployment-owned Bitrix24 OAuth app ID |
| `BITRIX_CLIENT_SECRET` | yes | — | Deployment-owned Bitrix24 OAuth app secret |
| `BITRIX_PORTAL_DOMAIN` | conditional | stored portal | Portal used for the automatic OAuth redirect and exact allowlist entry for a custom domain |
| `MCP_AUTH_TOKEN` | yes | — | Operator-only static bearer fallback, minimum 32 characters |
| `TOKEN_ENCRYPTION_KEY` | yes | — | Stable 32-byte base64 or 64-character hex key |
| `BITRIX_MCP_DEFAULT_MODE` | no | `read_only` | Mode assigned during OAuth unless overridden |
| `BITRIX_MCP_ADMIN_TOOLS` | no | `false` | Stored policy flag; no admin tools are exposed yet |
| `SCHEMA_CACHE_TTL_SECONDS` | no | `900` | Metadata cache lifetime |
| `PENDING_OPERATION_TTL_SECONDS` | no | `600` | Confirmation window |
| `MCP_OAUTH_ACCESS_TOKEN_TTL_SECONDS` | no | `3600` | MCP OAuth access-token lifetime |
| `MCP_OAUTH_REFRESH_TOKEN_TTL_SECONDS` | no | `2592000` | MCP OAuth refresh-token lifetime |
| `BITRIX_MAX_PAGES` | no | `20` | Pagination safety cap per tool call |

See `.env.example` for all runtime and retry settings.

## Docker and Coolify

For local Compose, set `DATABASE_URL` to use hostname `postgres` and make it agree with the PostgreSQL variables:

```text
DATABASE_URL=postgresql://bitrix_mcp:<POSTGRES_PASSWORD>@postgres:5432/bitrix_mcp
```

Then run `docker compose up --build`; Compose automatically loads `docker-compose.override.yml` to publish port 3000 for local use. In Coolify, deploy only `docker-compose.yml`, attach a public HTTPS domain to the `app` service on port 3000, and set every variable marked required by Compose. The production stack exposes the app only to Coolify's proxy network. Persistent storage is the `postgres_data` volume, and PostgreSQL is never published on the host.

## Architecture

```text
MCP client
  -> OAuth 2.1 / PKCE authorization request
  -> Bitrix24 login and consent
  -> Bitrix24 callback and encrypted profile token storage
  -> MCP token bound to that Bitrix24 connection
  -> MCP tools
  -> policy guard / confirmation workflow
  -> CRM and schema services
  -> token manager and encrypted PostgreSQL store
  -> Bitrix24 OAuth / REST API
```

All REST calls pass through one client for token refresh, timeouts, bounded retries, pagination at the service layer, and sanitized logging. Metadata resolution never guesses when an exact entity name is unavailable.

See [SECURITY.md](SECURITY.md) before production deployment and [CONTRIBUTING.md](CONTRIBUTING.md) before adding new tools.
