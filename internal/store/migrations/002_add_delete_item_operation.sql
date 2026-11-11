CREATE TABLE pending_operations_new (
  id TEXT PRIMARY KEY,
  connection_id TEXT NOT NULL REFERENCES bitrix_connections(id) ON DELETE CASCADE,
  operation_type TEXT NOT NULL CHECK (operation_type IN ('create_item', 'update_item', 'move_item_stage', 'delete_item')),
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

INSERT INTO pending_operations_new (
  id, connection_id, operation_type, method, params_json, human_summary,
  status, result_json, error_message, created_at, expires_at, confirmed_at, completed_at
)
SELECT
  id, connection_id, operation_type, method, params_json, human_summary,
  status, result_json, error_message, created_at, expires_at, confirmed_at, completed_at
FROM pending_operations;

DROP TABLE pending_operations;

ALTER TABLE pending_operations_new RENAME TO pending_operations;

CREATE INDEX IF NOT EXISTS pending_operations_status_idx ON pending_operations (status, expires_at);
CREATE INDEX IF NOT EXISTS pending_operations_connection_idx ON pending_operations (connection_id, created_at DESC);
