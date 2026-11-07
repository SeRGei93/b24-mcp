# Contributing

1. Use Node.js 20.11 or newer and PostgreSQL 16 for local development.
2. Copy `.env.example` to `.env`, generate fresh local secrets, and never commit that file.
3. Install dependencies with `npm ci`.
4. Run `npm run check` before opening a pull request.
5. Add tests for policy, OAuth, token, or operation-state changes.

Keep tools narrow and schema-validated. New writes must use the pending-operation confirmation flow. Administrative or raw REST methods require a separate security design and must remain disabled by default. Never log authorization headers, OAuth responses, REST request bodies containing `auth`, or decrypted tokens.
