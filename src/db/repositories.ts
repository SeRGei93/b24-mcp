import { createHash, randomUUID } from "node:crypto";
import type { DbClient, DbPool } from "./client.js";
import type {
  ConnectionMode,
  ConnectionRecord,
  OAuthStateRecord,
  OperationType,
  PendingOperationRecord,
} from "../types.js";
import { NotFoundError } from "../errors.js";

interface ConnectionRow {
  id: string;
  domain: string;
  member_id: string;
  bitrix_user_id: string;
  client_endpoint: string;
  access_token_encrypted: string;
  refresh_token_encrypted: string;
  scope: string;
  expires_at: Date;
  mode: ConnectionMode;
  admin_tools_enabled: boolean;
  status: ConnectionRecord["status"];
  created_at: Date;
  updated_at: Date;
}

interface PendingOperationRow {
  id: string;
  connection_id: string;
  operation_type: OperationType;
  method: string;
  params_json: Record<string, unknown>;
  human_summary: string;
  status: PendingOperationRecord["status"];
  result_json: unknown;
  error_message: string | null;
  created_at: Date;
  expires_at: Date;
  confirmed_at: Date | null;
  completed_at: Date | null;
}

function mapConnection(row: ConnectionRow): ConnectionRecord {
  return {
    id: row.id,
    domain: row.domain,
    memberId: row.member_id,
    bitrixUserId: row.bitrix_user_id,
    clientEndpoint: row.client_endpoint,
    accessTokenEncrypted: row.access_token_encrypted,
    refreshTokenEncrypted: row.refresh_token_encrypted,
    scope: row.scope,
    expiresAt: row.expires_at,
    mode: row.mode,
    adminToolsEnabled: row.admin_tools_enabled,
    status: row.status,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

function mapOperation(row: PendingOperationRow): PendingOperationRecord {
  return {
    id: row.id,
    connectionId: row.connection_id,
    operationType: row.operation_type,
    method: row.method,
    params: row.params_json,
    humanSummary: row.human_summary,
    status: row.status,
    result: row.result_json,
    errorMessage: row.error_message,
    createdAt: row.created_at,
    expiresAt: row.expires_at,
    confirmedAt: row.confirmed_at,
    completedAt: row.completed_at,
  };
}

export class ConnectionRepository {
  constructor(private readonly pool: DbPool) {}

  async list(): Promise<ConnectionRecord[]> {
    const result = await this.pool.query<ConnectionRow>("SELECT * FROM bitrix_connections ORDER BY created_at ASC");
    return result.rows.map(mapConnection);
  }

  async get(id: string, client: DbPool | DbClient = this.pool): Promise<ConnectionRecord> {
    const result = await client.query<ConnectionRow>("SELECT * FROM bitrix_connections WHERE id = $1", [id]);
    const row = result.rows[0];
    if (!row) throw new NotFoundError(`Bitrix connection '${id}' was not found`);
    return mapConnection(row);
  }

  async upsert(input: {
    domain: string;
    memberId: string;
    bitrixUserId: string;
    clientEndpoint: string;
    accessTokenEncrypted: string;
    refreshTokenEncrypted: string;
    scope: string;
    expiresAt: Date;
    mode: ConnectionMode;
    adminToolsEnabled: boolean;
  }): Promise<ConnectionRecord> {
    const result = await this.pool.query<ConnectionRow>(`
      INSERT INTO bitrix_connections (
        id, domain, member_id, bitrix_user_id, client_endpoint, access_token_encrypted,
        refresh_token_encrypted, scope, expires_at, mode, admin_tools_enabled
      ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
      ON CONFLICT (member_id, bitrix_user_id) DO UPDATE SET
        domain = EXCLUDED.domain,
        bitrix_user_id = EXCLUDED.bitrix_user_id,
        client_endpoint = EXCLUDED.client_endpoint,
        access_token_encrypted = EXCLUDED.access_token_encrypted,
        refresh_token_encrypted = EXCLUDED.refresh_token_encrypted,
        scope = EXCLUDED.scope,
        expires_at = EXCLUDED.expires_at,
        mode = EXCLUDED.mode,
        admin_tools_enabled = EXCLUDED.admin_tools_enabled,
        status = 'active',
        updated_at = now()
      RETURNING *
    `, [
      randomUUID(), input.domain, input.memberId, input.bitrixUserId, input.clientEndpoint,
      input.accessTokenEncrypted, input.refreshTokenEncrypted, input.scope, input.expiresAt,
      input.mode, input.adminToolsEnabled,
    ]);
    return mapConnection(result.rows[0]!);
  }

  async withRefreshLock<T>(id: string, callback: (connection: ConnectionRecord, client: DbClient) => Promise<T>): Promise<T> {
    const client = await this.pool.connect();
    try {
      await client.query("BEGIN");
      await client.query("SELECT pg_advisory_xact_lock(hashtext($1))", [`bitrix-refresh:${id}`]);
      const connection = await this.get(id, client);
      const value = await callback(connection, client);
      await client.query("COMMIT");
      return value;
    } catch (error) {
      await client.query("ROLLBACK").catch(() => undefined);
      throw error;
    } finally {
      client.release();
    }
  }

  async updateTokens(
    client: DbClient,
    id: string,
    input: Pick<ConnectionRecord,
      "accessTokenEncrypted" | "refreshTokenEncrypted" | "expiresAt" | "scope" | "domain" | "clientEndpoint" | "bitrixUserId"
    >,
  ): Promise<ConnectionRecord> {
    const result = await client.query<ConnectionRow>(`
      UPDATE bitrix_connections SET
        access_token_encrypted = $2,
        refresh_token_encrypted = $3,
        expires_at = $4,
        scope = $5,
        domain = $6,
        client_endpoint = $7,
        bitrix_user_id = $8,
        status = 'active',
        updated_at = now()
      WHERE id = $1 RETURNING *
    `, [
      id, input.accessTokenEncrypted, input.refreshTokenEncrypted, input.expiresAt, input.scope,
      input.domain, input.clientEndpoint, input.bitrixUserId,
    ]);
    const row = result.rows[0];
    if (!row) throw new NotFoundError(`Bitrix connection '${id}' was not found`);
    return mapConnection(row);
  }

  async markRevoked(id: string): Promise<void> {
    await this.pool.query("UPDATE bitrix_connections SET status = 'revoked', updated_at = now() WHERE id = $1", [id]);
  }
}

export class OAuthStateRepository {
  constructor(private readonly pool: DbPool) {}

  static hash(rawState: string): string {
    return createHash("sha256").update(rawState).digest("hex");
  }

  async create(
    rawState: string,
    domain: string,
    mode: ConnectionMode,
    expiresAt: Date,
    mcpRequestHash?: string,
  ): Promise<void> {
    await this.pool.query("DELETE FROM oauth_states WHERE expires_at <= now()");
    await this.pool.query(
      "INSERT INTO oauth_states (state_hash, domain, mode, expires_at, mcp_request_hash) VALUES ($1,$2,$3,$4,$5)",
      [OAuthStateRepository.hash(rawState), domain, mode, expiresAt, mcpRequestHash ?? null],
    );
  }

  async consume(rawState: string): Promise<OAuthStateRecord | null> {
    const result = await this.pool.query<{ domain: string; mode: ConnectionMode; mcp_request_hash: string | null }>(`
      DELETE FROM oauth_states
      WHERE state_hash = $1 AND expires_at > now()
      RETURNING domain, mode, mcp_request_hash
    `, [OAuthStateRepository.hash(rawState)]);
    const row = result.rows[0];
    return row ? {
      domain: row.domain,
      mode: row.mode,
      mcpRequestHash: row.mcp_request_hash ?? undefined,
    } : null;
  }
}

export class SchemaCacheRepository {
  constructor(private readonly pool: DbPool) {}

  async get<T>(connectionId: string, key: string, maxAgeSeconds: number): Promise<T | null> {
    const result = await this.pool.query<{ payload: T }>(`
      SELECT payload FROM schema_cache
      WHERE connection_id = $1 AND cache_key = $2
        AND fetched_at > now() - ($3 * interval '1 second')
    `, [connectionId, key, maxAgeSeconds]);
    return result.rows[0]?.payload ?? null;
  }

  async set(connectionId: string, key: string, payload: unknown): Promise<void> {
    await this.pool.query(`
      INSERT INTO schema_cache (connection_id, cache_key, payload, fetched_at)
      VALUES ($1,$2,$3,now())
      ON CONFLICT (connection_id, cache_key) DO UPDATE SET payload = EXCLUDED.payload, fetched_at = now()
    `, [connectionId, key, JSON.stringify(payload)]);
  }

  async clear(connectionId: string): Promise<number> {
    const result = await this.pool.query("DELETE FROM schema_cache WHERE connection_id = $1", [connectionId]);
    return result.rowCount ?? 0;
  }
}

export class PendingOperationRepository {
  constructor(private readonly pool: DbPool) {}

  async create(input: {
    connectionId: string;
    operationType: OperationType;
    method: string;
    params: Record<string, unknown>;
    humanSummary: string;
    expiresAt: Date;
  }): Promise<PendingOperationRecord> {
    const result = await this.pool.query<PendingOperationRow>(`
      INSERT INTO pending_operations (
        id, connection_id, operation_type, method, params_json, human_summary, expires_at
      ) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING *
    `, [randomUUID(), input.connectionId, input.operationType, input.method, JSON.stringify(input.params), input.humanSummary, input.expiresAt]);
    return mapOperation(result.rows[0]!);
  }

  async list(connectionId?: string): Promise<PendingOperationRecord[]> {
    await this.expireStale();
    const result = connectionId
      ? await this.pool.query<PendingOperationRow>("SELECT * FROM pending_operations WHERE connection_id = $1 ORDER BY created_at DESC LIMIT 100", [connectionId])
      : await this.pool.query<PendingOperationRow>("SELECT * FROM pending_operations ORDER BY created_at DESC LIMIT 100");
    return result.rows.map(mapOperation);
  }

  async get(id: string): Promise<PendingOperationRecord> {
    await this.expireStale();
    const result = await this.pool.query<PendingOperationRow>("SELECT * FROM pending_operations WHERE id = $1", [id]);
    const row = result.rows[0];
    if (!row) throw new NotFoundError(`Pending operation '${id}' was not found`);
    return mapOperation(row);
  }

  async claim(id: string): Promise<PendingOperationRecord | null> {
    const result = await this.pool.query<PendingOperationRow>(`
      UPDATE pending_operations SET status = 'executing', confirmed_at = now()
      WHERE id = $1 AND status = 'pending' AND expires_at > now()
      RETURNING *
    `, [id]);
    return result.rows[0] ? mapOperation(result.rows[0]) : null;
  }

  async cancel(id: string): Promise<PendingOperationRecord | null> {
    const result = await this.pool.query<PendingOperationRow>(`
      UPDATE pending_operations SET status = 'cancelled', completed_at = now()
      WHERE id = $1 AND status = 'pending' RETURNING *
    `, [id]);
    return result.rows[0] ? mapOperation(result.rows[0]) : null;
  }

  async complete(id: string, resultValue: unknown): Promise<void> {
    await this.pool.query(`
      UPDATE pending_operations SET status = 'completed', result_json = $2, completed_at = now()
      WHERE id = $1 AND status = 'executing'
    `, [id, JSON.stringify(resultValue)]);
  }

  async fail(id: string, message: string): Promise<void> {
    await this.pool.query(`
      UPDATE pending_operations SET status = 'failed', error_message = $2, completed_at = now()
      WHERE id = $1 AND status = 'executing'
    `, [id, message.slice(0, 1_000)]);
  }

  private async expireStale(): Promise<void> {
    await this.pool.query(`
      UPDATE pending_operations SET status = 'expired', completed_at = now()
      WHERE status = 'pending' AND expires_at <= now()
    `);
  }
}

export class AuditRepository {
  constructor(private readonly pool: DbPool) {}

  async record(input: {
    connectionId?: string;
    operationId?: string;
    action: string;
    outcome: string;
    details?: Record<string, unknown>;
  }): Promise<void> {
    await this.pool.query(`
      INSERT INTO audit_logs (id, connection_id, operation_id, action, outcome, details_json)
      VALUES ($1,$2,$3,$4,$5,$6)
    `, [
      randomUUID(), input.connectionId ?? null, input.operationId ?? null, input.action,
      input.outcome, JSON.stringify(input.details ?? {}),
    ]);
  }
}
