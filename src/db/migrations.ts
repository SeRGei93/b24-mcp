export const migrations = [
  {
    id: "001_initial",
    sql: `
      CREATE TABLE IF NOT EXISTS schema_migrations (
        id text PRIMARY KEY,
        applied_at timestamptz NOT NULL DEFAULT now()
      );

      CREATE TABLE IF NOT EXISTS bitrix_connections (
        id uuid PRIMARY KEY,
        domain text NOT NULL,
        member_id text NOT NULL UNIQUE,
        bitrix_user_id text NOT NULL,
        client_endpoint text NOT NULL,
        access_token_encrypted text NOT NULL,
        refresh_token_encrypted text NOT NULL,
        scope text NOT NULL DEFAULT '',
        expires_at timestamptz NOT NULL,
        mode text NOT NULL DEFAULT 'read_only' CHECK (mode IN ('read_only', 'write_with_confirmation')),
        admin_tools_enabled boolean NOT NULL DEFAULT false,
        status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'revoked')),
        created_at timestamptz NOT NULL DEFAULT now(),
        updated_at timestamptz NOT NULL DEFAULT now()
      );

      CREATE INDEX IF NOT EXISTS bitrix_connections_domain_idx ON bitrix_connections (domain);

      CREATE TABLE IF NOT EXISTS oauth_states (
        state_hash char(64) PRIMARY KEY,
        domain text NOT NULL,
        mode text NOT NULL CHECK (mode IN ('read_only', 'write_with_confirmation')),
        expires_at timestamptz NOT NULL,
        created_at timestamptz NOT NULL DEFAULT now()
      );
      CREATE INDEX IF NOT EXISTS oauth_states_expires_idx ON oauth_states (expires_at);

      CREATE TABLE IF NOT EXISTS schema_cache (
        connection_id uuid NOT NULL REFERENCES bitrix_connections(id) ON DELETE CASCADE,
        cache_key text NOT NULL,
        payload jsonb NOT NULL,
        fetched_at timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY (connection_id, cache_key)
      );

      CREATE TABLE IF NOT EXISTS pending_operations (
        id uuid PRIMARY KEY,
        connection_id uuid NOT NULL REFERENCES bitrix_connections(id) ON DELETE CASCADE,
        operation_type text NOT NULL CHECK (operation_type IN ('create_item', 'update_item', 'move_item_stage')),
        method text NOT NULL,
        params_json jsonb NOT NULL,
        human_summary text NOT NULL,
        status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'executing', 'completed', 'cancelled', 'expired', 'failed')),
        result_json jsonb,
        error_message text,
        created_at timestamptz NOT NULL DEFAULT now(),
        expires_at timestamptz NOT NULL,
        confirmed_at timestamptz,
        completed_at timestamptz
      );
      CREATE INDEX IF NOT EXISTS pending_operations_status_idx ON pending_operations (status, expires_at);
      CREATE INDEX IF NOT EXISTS pending_operations_connection_idx ON pending_operations (connection_id, created_at DESC);

      CREATE TABLE IF NOT EXISTS audit_logs (
        id uuid PRIMARY KEY,
        connection_id uuid REFERENCES bitrix_connections(id) ON DELETE SET NULL,
        operation_id uuid REFERENCES pending_operations(id) ON DELETE SET NULL,
        action text NOT NULL,
        outcome text NOT NULL,
        details_json jsonb NOT NULL DEFAULT '{}'::jsonb,
        created_at timestamptz NOT NULL DEFAULT now()
      );
      CREATE INDEX IF NOT EXISTS audit_logs_connection_idx ON audit_logs (connection_id, created_at DESC);
    `,
  },
  {
    id: "002_mcp_oauth",
    sql: `
      CREATE TABLE IF NOT EXISTS mcp_oauth_clients (
        client_id text PRIMARY KEY,
        redirect_uris jsonb NOT NULL,
        token_endpoint_auth_method text NOT NULL CHECK (token_endpoint_auth_method = 'none'),
        grant_types jsonb NOT NULL,
        response_types jsonb NOT NULL,
        client_name text,
        client_uri text,
        logo_uri text,
        scope text NOT NULL DEFAULT 'mcp:tools',
        software_id text,
        software_version text,
        created_at timestamptz NOT NULL DEFAULT now()
      );

      CREATE TABLE IF NOT EXISTS mcp_oauth_authorization_requests (
        request_hash char(64) PRIMARY KEY,
        client_id text NOT NULL REFERENCES mcp_oauth_clients(client_id) ON DELETE CASCADE,
        redirect_uri text NOT NULL,
        state text,
        code_challenge text NOT NULL,
        resource text NOT NULL,
        scope text NOT NULL,
        expires_at timestamptz NOT NULL,
        attempts smallint NOT NULL DEFAULT 0 CHECK (attempts >= 0 AND attempts <= 5),
        created_at timestamptz NOT NULL DEFAULT now()
      );
      CREATE INDEX IF NOT EXISTS mcp_oauth_authorization_requests_expires_idx
        ON mcp_oauth_authorization_requests (expires_at);

      CREATE TABLE IF NOT EXISTS mcp_oauth_authorization_codes (
        code_hash char(64) PRIMARY KEY,
        client_id text NOT NULL REFERENCES mcp_oauth_clients(client_id) ON DELETE CASCADE,
        redirect_uri text NOT NULL,
        code_challenge text NOT NULL,
        resource text NOT NULL,
        scope text NOT NULL,
        expires_at timestamptz NOT NULL,
        created_at timestamptz NOT NULL DEFAULT now()
      );
      CREATE INDEX IF NOT EXISTS mcp_oauth_authorization_codes_expires_idx
        ON mcp_oauth_authorization_codes (expires_at);

      CREATE TABLE IF NOT EXISTS mcp_oauth_tokens (
        id uuid PRIMARY KEY,
        access_token_hash char(64) NOT NULL UNIQUE,
        refresh_token_hash char(64) NOT NULL UNIQUE,
        client_id text NOT NULL REFERENCES mcp_oauth_clients(client_id) ON DELETE CASCADE,
        resource text NOT NULL,
        scope text NOT NULL,
        access_expires_at timestamptz NOT NULL,
        refresh_expires_at timestamptz NOT NULL,
        revoked_at timestamptz,
        created_at timestamptz NOT NULL DEFAULT now()
      );
      CREATE INDEX IF NOT EXISTS mcp_oauth_tokens_access_idx
        ON mcp_oauth_tokens (access_token_hash, access_expires_at) WHERE revoked_at IS NULL;
      CREATE INDEX IF NOT EXISTS mcp_oauth_tokens_refresh_idx
        ON mcp_oauth_tokens (refresh_token_hash, refresh_expires_at) WHERE revoked_at IS NULL;
    `,
  },
  {
    id: "003_federated_bitrix_oauth",
    sql: `
      -- Existing MCP sessions used the previous operator-token approval flow.
      -- They must authenticate again so every new MCP token is bound to the
      -- Bitrix24 profile that authorized it.
      DELETE FROM oauth_states;
      DELETE FROM mcp_oauth_tokens;
      DELETE FROM mcp_oauth_authorization_codes;
      DELETE FROM mcp_oauth_authorization_requests;

      ALTER TABLE bitrix_connections
        DROP CONSTRAINT IF EXISTS bitrix_connections_member_id_key;
      CREATE UNIQUE INDEX IF NOT EXISTS bitrix_connections_member_user_idx
        ON bitrix_connections (member_id, bitrix_user_id);

      ALTER TABLE oauth_states
        ADD COLUMN IF NOT EXISTS mcp_request_hash char(64)
        REFERENCES mcp_oauth_authorization_requests(request_hash) ON DELETE CASCADE;

      ALTER TABLE mcp_oauth_authorization_codes
        ADD COLUMN IF NOT EXISTS connection_id uuid NOT NULL
        REFERENCES bitrix_connections(id) ON DELETE CASCADE;
      CREATE INDEX IF NOT EXISTS mcp_oauth_authorization_codes_connection_idx
        ON mcp_oauth_authorization_codes (connection_id);

      ALTER TABLE mcp_oauth_tokens
        ADD COLUMN IF NOT EXISTS connection_id uuid NOT NULL
        REFERENCES bitrix_connections(id) ON DELETE CASCADE;
      CREATE INDEX IF NOT EXISTS mcp_oauth_tokens_connection_idx
        ON mcp_oauth_tokens (connection_id, revoked_at);
    `,
  },
] as const;
