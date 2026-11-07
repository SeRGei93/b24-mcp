# Contributing

1. Use Node.js 22.5 or newer (uses built-in SQLite via `node:sqlite`) for local development.
2. Copy `.env.example` to `.env`, generate fresh local secrets, and never commit that file.
3. Install dependencies with `npm ci`.
4. Run `npm run check` before opening a pull request.
5. Add tests for policy, OAuth, token, or operation-state changes.

Keep tools narrow and schema-validated. New writes must use the pending-operation confirmation flow. Administrative or raw REST methods require a separate security design and must remain disabled by default. Never log authorization headers, OAuth responses, REST request bodies containing `auth`, or decrypted tokens.
