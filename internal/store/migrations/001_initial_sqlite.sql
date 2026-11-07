-- +goose Up
CREATE TABLE IF NOT EXISTS schema_migrations (
  id TEXT PRIMARY KEY,
  applied_at TEXT NOT NULL
);

INSERT OR IGNORE INTO schema_migrations (id, applied_at)
VALUES ('001_initial_sqlite', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

CREATE TABLE IF NOT EXISTS bitrix_connections (
  id TEXT PRIMARY KEY,
  domain TEXT NOT NULL,
  member_id TEXT NOT NULL,
  bitrix_user_id TEXT NOT NULL,
  client_endpoint TEXT NOT NULL,
  access_token_encrypted TEXT NOT NULL,
  refresh_token_encrypted TEXT NOT NULL,
  scope TEXT NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL,
  mode TEXT NOT NULL DEFAULT 'read_only' CHECK (mode IN ('read_only', 'write_with_confirmation')),
  admin_tools_enabled INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'revoked')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS bitrix_connections_domain_idx ON bitrix_connections (domain);
CREATE UNIQUE INDEX IF NOT EXISTS bitrix_connections_member_user_idx
  ON bitrix_connections (member_id, bitrix_user_id);

CREATE TABLE IF NOT EXISTS mcp_oauth_clients (
  client_id TEXT PRIMARY KEY,
  redirect_uris TEXT NOT NULL,
  token_endpoint_auth_method TEXT NOT NULL CHECK (token_endpoint_auth_method = 'none'),
  grant_types TEXT NOT NULL,
  response_types TEXT NOT NULL,
  client_name TEXT,
  client_uri TEXT,
  logo_uri TEXT,
  scope TEXT NOT NULL DEFAULT 'mcp:tools',
  software_id TEXT,
  software_version TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS mcp_oauth_authorization_requests (
  request_hash TEXT PRIMARY KEY,
  client_id TEXT NOT NULL REFERENCES mcp_oauth_clients(client_id) ON DELETE CASCADE,
  redirect_uri TEXT NOT NULL,
  state TEXT,
  code_challenge TEXT NOT NULL,
  resource TEXT NOT NULL,
  scope TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0 AND attempts <= 5),
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS mcp_oauth_authorization_requests_expires_idx
  ON mcp_oauth_authorization_requests (expires_at);

CREATE TABLE IF NOT EXISTS oauth_states (
  state_hash TEXT PRIMARY KEY,
  domain TEXT NOT NULL,
  mode TEXT NOT NULL CHECK (mode IN ('read_only', 'write_with_confirmation')),
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  mcp_request_hash TEXT REFERENCES mcp_oauth_authorization_requests(request_hash) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS oauth_states_expires_idx ON oauth_states (expires_at);

CREATE TABLE IF NOT EXISTS schema_cache (
  connection_id TEXT NOT NULL REFERENCES bitrix_connections(id) ON DELETE CASCADE,
  cache_key TEXT NOT NULL,
  payload TEXT NOT NULL,
  fetched_at TEXT NOT NULL,
  PRIMARY KEY (connection_id, cache_key)
);

CREATE TABLE IF NOT EXISTS pending_operations (
  id TEXT PRIMARY KEY,
  connection_id TEXT NOT NULL REFERENCES bitrix_connections(id) ON DELETE CASCADE,
  operation_type TEXT NOT NULL CHECK (operation_type IN ('create_item', 'update_item', 'move_item_stage')),
  method TEXT NOT NULL,
  params_json TEXT NOT NULL,
  human_summary TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'executing', 'completed', 'cancelled', 'expired', 'failed')),
  result_json TEXT,
  error_message TEXT,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  confirmed_at TEXT,
  completed_at TEXT
);
CREATE INDEX IF NOT EXISTS pending_operations_status_idx ON pending_operations (status, expires_at);
CREATE INDEX IF NOT EXISTS pending_operations_connection_idx ON pending_operations (connection_id, created_at DESC);

CREATE TABLE IF NOT EXISTS audit_logs (
  id TEXT PRIMARY KEY,
  connection_id TEXT REFERENCES bitrix_connections(id) ON DELETE SET NULL,
  operation_id TEXT REFERENCES pending_operations(id) ON DELETE SET NULL,
  action TEXT NOT NULL,
  outcome TEXT NOT NULL,
  details_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_logs_connection_idx ON audit_logs (connection_id, created_at DESC);

CREATE TABLE IF NOT EXISTS mcp_oauth_authorization_codes (
  code_hash TEXT PRIMARY KEY,
  client_id TEXT NOT NULL REFERENCES mcp_oauth_clients(client_id) ON DELETE CASCADE,
  connection_id TEXT NOT NULL REFERENCES bitrix_connections(id) ON DELETE CASCADE,
  redirect_uri TEXT NOT NULL,
  code_challenge TEXT NOT NULL,
  resource TEXT NOT NULL,
  scope TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS mcp_oauth_authorization_codes_expires_idx
  ON mcp_oauth_authorization_codes (expires_at);
CREATE INDEX IF NOT EXISTS mcp_oauth_authorization_codes_connection_idx
  ON mcp_oauth_authorization_codes (connection_id);

CREATE TABLE IF NOT EXISTS mcp_oauth_tokens (
  id TEXT PRIMARY KEY,
  access_token_hash TEXT NOT NULL UNIQUE,
  refresh_token_hash TEXT NOT NULL UNIQUE,
  client_id TEXT NOT NULL REFERENCES mcp_oauth_clients(client_id) ON DELETE CASCADE,
  connection_id TEXT NOT NULL REFERENCES bitrix_connections(id) ON DELETE CASCADE,
  resource TEXT NOT NULL,
  scope TEXT NOT NULL,
  access_expires_at TEXT NOT NULL,
  refresh_expires_at TEXT NOT NULL,
  revoked_at TEXT,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS mcp_oauth_tokens_access_idx
  ON mcp_oauth_tokens (access_token_hash, access_expires_at) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS mcp_oauth_tokens_refresh_idx
  ON mcp_oauth_tokens (refresh_token_hash, refresh_expires_at) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS mcp_oauth_tokens_connection_idx
  ON mcp_oauth_tokens (connection_id, revoked_at);

-- +goose Down
DROP TABLE IF EXISTS mcp_oauth_tokens;
DROP TABLE IF EXISTS mcp_oauth_authorization_codes;
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS pending_operations;
DROP TABLE IF EXISTS schema_cache;
DROP TABLE IF EXISTS oauth_states;
DROP TABLE IF EXISTS mcp_oauth_authorization_requests;
DROP TABLE IF EXISTS mcp_oauth_clients;
DROP TABLE IF EXISTS bitrix_connections;
DROP TABLE IF EXISTS schema_migrations;
