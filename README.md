# bitrix24-mcp-server

Self-hosted [Model Context Protocol](https://modelcontextprotocol.io/) server for Bitrix24 CRM and smart-process automations (SPAs). Uses built-in Node.js SQLite (`node:sqlite`) for zero external database setup.

## Features

- **Bitrix24 OAuth 2.0**: Automatic authorization flow, token refresh, and AES-256-GCM encryption at rest in SQLite.
- **MCP OAuth 2.1**: Dynamic client registration (DCR), S256 PKCE, and static bearer token (`MCP_AUTH_TOKEN`) fallback.
- **CRM & SPA Discovery**: Built-in entities (Lead, Deal, Contact, Company, Invoice) and custom SPAs (`crm.type.list`), fields, categories/funnels, and stages with schema caching.
- **Two-Step Writes**: Create, update, and stage-move operations are prepared first and executed only after explicit confirmation (`bitrix_confirm_operation`).

## Quick Start

Requirements: **Node.js 22.5+** and a Bitrix24 OAuth local/public application with `crm` scope.

```bash
cp .env.example .env
npm ci
npm run build
npm start
```

Configure your Bitrix24 local application paths:

```text
Handler path:              https://your-server.example.com/oauth/bitrix/callback
Initial installation path: https://your-server.example.com/oauth/bitrix/install
```

## Connect an MCP Client

Streamable HTTP endpoint:

```text
https://your-server.example.com/mcp
```

- **Claude / Claude Code / Codex (OAuth 2.1)**:
  ```bash
  claude mcp add --transport http bitrix24 https://your-server.example.com/mcp
  # or
  codex mcp add bitrix24 --url https://your-server.example.com/mcp && codex mcp login bitrix24
  ```
- **Static Bearer Token**:
  ```json
  {
    "mcpServers": {
      "bitrix24": {
        "url": "https://your-server.example.com/mcp",
        "headers": {
          "Authorization": "Bearer <MCP_AUTH_TOKEN>"
        }
      }
    }
  }
  ```
- **Stdio**: Set `MCP_TRANSPORT=stdio` and run `node dist/index.js`.

## Available Tools

**Read-only:**
- `bitrix_list_connections`, `bitrix_discover_portal`, `bitrix_list_crm_entities`, `bitrix_describe_entity`
- `bitrix_list_fields`, `bitrix_list_categories`, `bitrix_list_stages`, `bitrix_refresh_schema_cache`
- `bitrix_search_items`, `bitrix_get_item`, `bitrix_list_users`

**Write (requires `write_with_confirmation` mode):**
- `bitrix_prepare_create_item`, `bitrix_prepare_update_item`, `bitrix_prepare_move_item_stage`
- `bitrix_confirm_operation`, `bitrix_cancel_operation`, `bitrix_list_pending_operations`

## Configuration

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `APP_URL` | yes | — | Public server URL (HTTPS, or `http://localhost` for dev) |
| `DATABASE_PATH` | no | `./data/bitrix24-mcp.sqlite` | SQLite database file path (or `:memory:`) |
| `BITRIX_CLIENT_ID` | yes | — | Bitrix24 OAuth app client ID |
| `BITRIX_CLIENT_SECRET` | yes | — | Bitrix24 OAuth app client secret |
| `BITRIX_PORTAL_DOMAIN` | no | — | Portal domain for OAuth redirect and custom domain allowlist |
| `MCP_AUTH_TOKEN` | yes | — | Static bearer token (min 32 chars) |
| `TOKEN_ENCRYPTION_KEY` | yes | — | 32-byte base64 or 64-char hex key for AES-256-GCM |
| `BITRIX_MCP_DEFAULT_MODE` | no | `read_only` | `read_only` or `write_with_confirmation` |

## Docker

```bash
docker compose up --build
```

SQLite data is persisted in the `sqlite_data` volume mounted at `/app/data`.
