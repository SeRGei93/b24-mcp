# Security

- **Token Encryption**: Bitrix24 OAuth tokens are encrypted at rest in SQLite with AES-256-GCM (`TOKEN_ENCRYPTION_KEY`).
- **OAuth 2.1 & PKCE**: MCP OAuth uses S256 PKCE, hashed tokens/codes, and binds each issued token to the authorizing Bitrix24 connection.
- **Portal Allowlist**: Outbound requests are restricted to `*.bitrix24.*` cloud domains or the exact `BITRIX_PORTAL_DOMAIN`.
- **Safe Writes**: Connections default to `read_only`. Write operations require `write_with_confirmation` mode and an explicit `bitrix_confirm_operation` call.
