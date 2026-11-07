# Security policy

## Reporting a vulnerability

Please do not open a public issue for a suspected vulnerability. Use GitHub's private vulnerability reporting for this repository. Include the affected version, impact, reproduction steps, and any proposed remediation. Maintainers should acknowledge a report within seven days.

## Security model

- Every deployment supplies its own Bitrix24 OAuth application credentials.
- OAuth access and refresh tokens are encrypted at rest with AES-256-GCM. The encryption key is never stored in PostgreSQL.
- OAuth `state` values are random, stored only as SHA-256 hashes, expire after ten minutes by default, and are consumed once.
- API-only installation callbacks are validated against Bitrix24's `app.info` method before any supplied token is stored.
- Outbound portal requests are restricted to hosted HTTPS Bitrix24 domains or the exact custom domain configured in `BITRIX_PORTAL_DOMAIN` to reduce SSRF exposure.
- `/mcp`, OAuth initiation, and manual token refresh accept OAuth 2.1 access tokens or the legacy static bearer token.
- MCP OAuth requires S256 PKCE, exact resource binding, registered redirect URIs, short-lived authorization codes, and rotating refresh tokens. Authorization is delegated to the configured Bitrix24 portal; no operator token is entered in the browser.
- Every MCP OAuth token is bound to the Bitrix24 connection and user profile that completed authorization. Cross-connection tool and pending-operation access is denied server-side.
- Dynamic client registration accepts only public clients and HTTPS redirect URIs, with the standard HTTP loopback exception for local clients. Registration and token endpoints are rate-limited per source address.
- MCP authorization requests, codes, access tokens, and refresh tokens are stored only as SHA-256 hashes. A database disclosure does not reveal usable MCP credentials.
- Connections are read-only by default. Write-capable connections can only prepare operations; a distinct confirmation call executes them.
- Raw and administrative methods are not exposed in version 0.3.1. The global admin flag remains off by default.
- Tokens and credentials are redacted from logs. Do not add request-body logging around OAuth or REST calls.

## Operator responsibilities

Use HTTPS, restrict network access to PostgreSQL, rotate `MCP_AUTH_TOKEN` when exposure is suspected, back up `TOKEN_ENCRYPTION_KEY` separately, and test with a non-production Bitrix24 portal first. Losing the encryption key makes stored Bitrix24 OAuth tokens unrecoverable. Changing it requires reconnecting every portal.

Grant the Bitrix24 OAuth app and OAuth user only the scopes and CRM permissions actually required. Application policy cannot grant access that Bitrix24 denies, and it cannot compensate for an over-privileged OAuth user.
