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
  expires_at: string | Date;
  mode: ConnectionMode;
  admin_tools_enabled: number | boolean;
  status: ConnectionRecord["status"];
  created_at: string | Date;
  updated_at: string | Date;
}

interface PendingOperationRow {
  id: string;
  connection_id: string;
  operation_type: OperationType;
  method: string;
  params_json: string | Record<string, unknown>;
  human_summary: string;
  status: PendingOperationRecord["status"];
  result_json: string | null | unknown;
  error_message: string | null;
  created_at: string | Date;
  expires_at: string | Date;
  confirmed_at: string | Date | null;
  completed_at: string | Date | null;
}

function toDate(value: string | Date): Date {
  return value instanceof Date ? value : new Date(value);
}

function parseJson<T>(value: unknown): T {
  return typeof value === "string" ? (JSON.parse(value) as T) : (value as T);
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
    expiresAt: toDate(row.expires_at),
    mode: row.mode,
    adminToolsEnabled: Boolean(row.admin_tools_enabled),
    status: row.status,
    createdAt: toDate(row.created_at),
    updatedAt: toDate(row.updated_at),
  };
}

function mapOperation(row: PendingOperationRow): PendingOperationRecord {
  return {
    id: row.id,
    connectionId: row.connection_id,
    operationType: row.operation_type,
    method: row.method,
    params: parseJson<Record<string, unknown>>(row.params_json),
    humanSummary: row.human_summary,
    status: row.status,
    result: row.result_json === null ? null : parseJson<unknown>(row.result_json),
    errorMessage: row.error_message,
    createdAt: toDate(row.created_at),
    expiresAt: toDate(row.expires_at),
    confirmedAt: row.confirmed_at ? toDate(row.confirmed_at) : null,
    completedAt: row.completed_at ? toDate(row.completed_at) : null,
  };
}

export class ConnectionRepository {
  constructor(private readonly pool: DbPool) {}

  async list(): Promise<ConnectionRecord[]> {
    const result = await this.pool.query<ConnectionRow>("SELECT * FROM bitrix_connections ORDER BY created_at ASC");
    return result.rows.map(mapConnection);
  }

  async get(id: string, client: DbPool | DbClient = this.pool): Promise<ConnectionRecord> {
    const result = await client.query<ConnectionRow>("SELECT * FROM bitrix_connections WHERE id = ?1", [id]);
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
    const now = new Date().toISOString();
    const result = await this.pool.query<ConnectionRow>(`
      INSERT INTO bitrix_connections (
        id, domain, member_id, bitrix_user_id, client_endpoint, access_token_encrypted,
        refresh_token_encrypted, scope, expires_at, mode, admin_tools_enabled, status,
        created_at, updated_at
      ) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,'active',?12,?12)
      ON CONFLICT (member_id, bitrix_user_id) DO UPDATE SET
        domain = excluded.domain,
        bitrix_user_id = excluded.bitrix_user_id,
        client_endpoint = excluded.client_endpoint,
        access_token_encrypted = excluded.access_token_encrypted,
        refresh_token_encrypted = excluded.refresh_token_encrypted,
        scope = excluded.scope,
        expires_at = excluded.expires_at,
        mode = excluded.mode,
        admin_tools_enabled = excluded.admin_tools_enabled,
        status = 'active',
        updated_at = excluded.updated_at
      RETURNING *
    `, [
      randomUUID(),
      input.domain,
      input.memberId,
      input.bitrixUserId,
      input.clientEndpoint,
      input.accessTokenEncrypted,
      input.refreshTokenEncrypted,
      input.scope,
      input.expiresAt.toISOString(),
      input.mode,
      input.adminToolsEnabled ? 1 : 0,
      now,
    ]);
    return mapConnection(result.rows[0]!);
  }

  async withRefreshLock<T>(id: string, callback: (connection: ConnectionRecord, client: DbClient) => Promise<T>): Promise<T> {
    return this.pool.withLock(`bitrix-refresh:${id}`, async (client) => {
      const connection = await this.get(id, client);
      return callback(connection, client);
    });
  }

  async updateTokens(
    client: DbClient,
    id: string,
    input: Pick<ConnectionRecord,
      "accessTokenEncrypted" | "refreshTokenEncrypted" | "expiresAt" | "scope" | "domain" | "clientEndpoint" | "bitrixUserId"
    >,
  ): Promise<ConnectionRecord> {
    const now = new Date().toISOString();
    const result = await client.query<ConnectionRow>(`
      UPDATE bitrix_connections SET
        access_token_encrypted = ?2,
        refresh_token_encrypted = ?3,
        expires_at = ?4,
        scope = ?5,
        domain = ?6,
        client_endpoint = ?7,
        bitrix_user_id = ?8,
        status = 'active',
        updated_at = ?9
      WHERE id = ?1 RETURNING *
    `, [
      id,
      input.accessTokenEncrypted,
      input.refreshTokenEncrypted,
      input.expiresAt.toISOString(),
      input.scope,
      input.domain,
      input.clientEndpoint,
      input.bitrixUserId,
      now,
    ]);
    const row = result.rows[0];
    if (!row) throw new NotFoundError(`Bitrix connection '${id}' was not found`);
    return mapConnection(row);
  }

  async markRevoked(id: string): Promise<void> {
    const now = new Date().toISOString();
    await this.pool.query("UPDATE bitrix_connections SET status = 'revoked', updated_at = ?2 WHERE id = ?1", [id, now]);
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
    const now = new Date().toISOString();
    await this.pool.query("DELETE FROM oauth_states WHERE expires_at <= ?1", [now]);
    await this.pool.query(
      "INSERT INTO oauth_states (state_hash, domain, mode, expires_at, created_at, mcp_request_hash) VALUES (?1,?2,?3,?4,?5,?6)",
      [OAuthStateRepository.hash(rawState), domain, mode, expiresAt.toISOString(), now, mcpRequestHash ?? null],
    );
  }

  async consume(rawState: string): Promise<OAuthStateRecord | null> {
    const now = new Date().toISOString();
    const result = await this.pool.query<{ domain: string; mode: ConnectionMode; mcp_request_hash: string | null }>(`
      DELETE FROM oauth_states
      WHERE state_hash = ?1 AND expires_at > ?2
      RETURNING domain, mode, mcp_request_hash
    `, [OAuthStateRepository.hash(rawState), now]);
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
    const minFetchedAt = new Date(Date.now() - maxAgeSeconds * 1_000).toISOString();
    const result = await this.pool.query<{ payload: string }>(`
      SELECT payload FROM schema_cache
      WHERE connection_id = ?1 AND cache_key = ?2
        AND fetched_at > ?3
    `, [connectionId, key, minFetchedAt]);
    const raw = result.rows[0]?.payload;
    if (raw === undefined) return null;
    return parseJson<T>(raw);
  }

  async set(connectionId: string, key: string, payload: unknown): Promise<void> {
    const now = new Date().toISOString();
    await this.pool.query(`
      INSERT INTO schema_cache (connection_id, cache_key, payload, fetched_at)
      VALUES (?1,?2,?3,?4)
      ON CONFLICT (connection_id, cache_key) DO UPDATE SET payload = excluded.payload, fetched_at = excluded.fetched_at
    `, [connectionId, key, JSON.stringify(payload), now]);
  }

  async clear(connectionId: string): Promise<number> {
    const result = await this.pool.query("DELETE FROM schema_cache WHERE connection_id = ?1", [connectionId]);
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
    const now = new Date().toISOString();
    const result = await this.pool.query<PendingOperationRow>(`
      INSERT INTO pending_operations (
        id, connection_id, operation_type, method, params_json, human_summary, created_at, expires_at
      ) VALUES (?1,?2,?3,?4,?5,?6,?7,?8) RETURNING *
    `, [
      randomUUID(),
      input.connectionId,
      input.operationType,
      input.method,
      JSON.stringify(input.params),
      input.humanSummary,
      now,
      input.expiresAt.toISOString(),
    ]);
    return mapOperation(result.rows[0]!);
  }

  async list(connectionId?: string): Promise<PendingOperationRecord[]> {
    await this.expireStale();
    const result = connectionId
      ? await this.pool.query<PendingOperationRow>("SELECT * FROM pending_operations WHERE connection_id = ?1 ORDER BY created_at DESC LIMIT 100", [connectionId])
      : await this.pool.query<PendingOperationRow>("SELECT * FROM pending_operations ORDER BY created_at DESC LIMIT 100");
    return result.rows.map(mapOperation);
  }

  async get(id: string): Promise<PendingOperationRecord> {
    await this.expireStale();
    const result = await this.pool.query<PendingOperationRow>("SELECT * FROM pending_operations WHERE id = ?1", [id]);
    const row = result.rows[0];
    if (!row) throw new NotFoundError(`Pending operation '${id}' was not found`);
    return mapOperation(row);
  }

  async claim(id: string): Promise<PendingOperationRecord | null> {
    const now = new Date().toISOString();
    const result = await this.pool.query<PendingOperationRow>(`
      UPDATE pending_operations SET status = 'executing', confirmed_at = ?2
      WHERE id = ?1 AND status = 'pending' AND expires_at > ?2
      RETURNING *
    `, [id, now]);
    return result.rows[0] ? mapOperation(result.rows[0]) : null;
  }

  async cancel(id: string): Promise<PendingOperationRecord | null> {
    const now = new Date().toISOString();
    const result = await this.pool.query<PendingOperationRow>(`
      UPDATE pending_operations SET status = 'cancelled', completed_at = ?2
      WHERE id = ?1 AND status = 'pending' RETURNING *
    `, [id, now]);
    return result.rows[0] ? mapOperation(result.rows[0]) : null;
  }

  async complete(id: string, resultValue: unknown): Promise<void> {
    const now = new Date().toISOString();
    await this.pool.query(`
      UPDATE pending_operations SET status = 'completed', result_json = ?2, completed_at = ?3
      WHERE id = ?1 AND status = 'executing'
    `, [id, JSON.stringify(resultValue ?? null), now]);
  }

  async fail(id: string, message: string): Promise<void> {
    const now = new Date().toISOString();
    await this.pool.query(`
      UPDATE pending_operations SET status = 'failed', error_message = ?2, completed_at = ?3
      WHERE id = ?1 AND status = 'executing'
    `, [id, message.slice(0, 1_000), now]);
  }

  private async expireStale(): Promise<void> {
    const now = new Date().toISOString();
    await this.pool.query(`
      UPDATE pending_operations SET status = 'expired', completed_at = ?1
      WHERE status = 'pending' AND expires_at <= ?1
    `, [now]);
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
    const now = new Date().toISOString();
    await this.pool.query(`
      INSERT INTO audit_logs (id, connection_id, operation_id, action, outcome, details_json, created_at)
      VALUES (?1,?2,?3,?4,?5,?6,?7)
    `, [
      randomUUID(),
      input.connectionId ?? null,
      input.operationId ?? null,
      input.action,
      input.outcome,
      JSON.stringify(input.details ?? {}),
      now,
    ]);
  }
}
