import { randomUUID } from "node:crypto";
import type { DbPool } from "../../db/client.js";
import type {
  McpOAuthAuthorizationCode,
  McpOAuthAuthorizationRequest,
  McpOAuthClient,
  McpOAuthStore,
  McpOAuthTokenRecord,
  NewMcpOAuthTokenRecord,
} from "./types.js";

interface ClientRow {
  client_id: string;
  redirect_uris: string | string[];
  token_endpoint_auth_method: "none";
  grant_types: string | string[];
  response_types: string | string[];
  client_name: string | null;
  client_uri: string | null;
  logo_uri: string | null;
  scope: string;
  software_id: string | null;
  software_version: string | null;
  created_at: string | Date;
}

interface AuthorizationRequestRow {
  request_hash: string;
  client_id: string;
  redirect_uri: string;
  state: string | null;
  code_challenge: string;
  resource: string;
  scope: string;
  expires_at: string | Date;
  attempts: number;
}

interface AuthorizationCodeRow {
  code_hash: string;
  client_id: string;
  connection_id: string;
  redirect_uri: string;
  code_challenge: string;
  resource: string;
  scope: string;
  expires_at: string | Date;
}

interface TokenRow {
  id: string;
  access_token_hash: string;
  refresh_token_hash: string;
  client_id: string;
  connection_id: string;
  resource: string;
  scope: string;
  access_expires_at: string | Date;
  refresh_expires_at: string | Date;
  revoked_at: string | Date | null;
  created_at: string | Date;
}

function toDate(value: string | Date): Date {
  return value instanceof Date ? value : new Date(value);
}

function parseStringArray(value: string | string[]): string[] {
  return typeof value === "string" ? (JSON.parse(value) as string[]) : value;
}

function mapClient(row: ClientRow): McpOAuthClient {
  return {
    clientId: row.client_id,
    redirectUris: parseStringArray(row.redirect_uris),
    tokenEndpointAuthMethod: row.token_endpoint_auth_method,
    grantTypes: parseStringArray(row.grant_types),
    responseTypes: parseStringArray(row.response_types),
    clientName: row.client_name ?? undefined,
    clientUri: row.client_uri ?? undefined,
    logoUri: row.logo_uri ?? undefined,
    scope: row.scope,
    softwareId: row.software_id ?? undefined,
    softwareVersion: row.software_version ?? undefined,
    createdAt: toDate(row.created_at),
  };
}

function mapAuthorizationRequest(row: AuthorizationRequestRow): McpOAuthAuthorizationRequest {
  return {
    requestHash: row.request_hash,
    clientId: row.client_id,
    redirectUri: row.redirect_uri,
    state: row.state ?? undefined,
    codeChallenge: row.code_challenge,
    resource: row.resource,
    scope: row.scope,
    expiresAt: toDate(row.expires_at),
    attempts: Number(row.attempts),
  };
}

function mapAuthorizationCode(row: AuthorizationCodeRow): McpOAuthAuthorizationCode {
  return {
    codeHash: row.code_hash,
    clientId: row.client_id,
    connectionId: row.connection_id,
    redirectUri: row.redirect_uri,
    codeChallenge: row.code_challenge,
    resource: row.resource,
    scope: row.scope,
    expiresAt: toDate(row.expires_at),
  };
}

function mapToken(row: TokenRow): McpOAuthTokenRecord {
  return {
    id: row.id,
    accessTokenHash: row.access_token_hash,
    refreshTokenHash: row.refresh_token_hash,
    clientId: row.client_id,
    connectionId: row.connection_id,
    resource: row.resource,
    scope: row.scope,
    accessExpiresAt: toDate(row.access_expires_at),
    refreshExpiresAt: toDate(row.refresh_expires_at),
    revokedAt: row.revoked_at ? toDate(row.revoked_at) : null,
    createdAt: toDate(row.created_at),
  };
}

export class SqliteMcpOAuthStore implements McpOAuthStore {
  constructor(private readonly pool: DbPool) {}

  async registerClient(client: McpOAuthClient): Promise<void> {
    await this.cleanupExpired();
    await this.pool.query(`
      INSERT INTO mcp_oauth_clients (
        client_id, redirect_uris, token_endpoint_auth_method, grant_types, response_types,
        client_name, client_uri, logo_uri, scope, software_id, software_version, created_at
      ) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12)
    `, [
      client.clientId,
      JSON.stringify(client.redirectUris),
      client.tokenEndpointAuthMethod,
      JSON.stringify(client.grantTypes),
      JSON.stringify(client.responseTypes),
      client.clientName ?? null,
      client.clientUri ?? null,
      client.logoUri ?? null,
      client.scope,
      client.softwareId ?? null,
      client.softwareVersion ?? null,
      client.createdAt.toISOString(),
    ]);
  }

  async getClient(clientId: string): Promise<McpOAuthClient | null> {
    const result = await this.pool.query<ClientRow>(
      "SELECT * FROM mcp_oauth_clients WHERE client_id = ?1",
      [clientId],
    );
    return result.rows[0] ? mapClient(result.rows[0]) : null;
  }

  async createAuthorizationRequest(request: McpOAuthAuthorizationRequest): Promise<void> {
    await this.cleanupExpired();
    const now = new Date().toISOString();
    await this.pool.query(`
      INSERT INTO mcp_oauth_authorization_requests (
        request_hash, client_id, redirect_uri, state, code_challenge, resource,
        scope, expires_at, attempts, created_at
      ) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)
    `, [
      request.requestHash,
      request.clientId,
      request.redirectUri,
      request.state ?? null,
      request.codeChallenge,
      request.resource,
      request.scope,
      request.expiresAt.toISOString(),
      request.attempts,
      now,
    ]);
  }

  async getAuthorizationRequest(requestHash: string): Promise<McpOAuthAuthorizationRequest | null> {
    const now = new Date().toISOString();
    const result = await this.pool.query<AuthorizationRequestRow>(`
      SELECT * FROM mcp_oauth_authorization_requests
      WHERE request_hash = ?1 AND expires_at > ?2 AND attempts < 5
    `, [requestHash, now]);
    return result.rows[0] ? mapAuthorizationRequest(result.rows[0]) : null;
  }

  async incrementAuthorizationAttempts(requestHash: string): Promise<number> {
    const now = new Date().toISOString();
    const result = await this.pool.query<{ attempts: number }>(`
      UPDATE mcp_oauth_authorization_requests
      SET attempts = MIN(attempts + 1, 5)
      WHERE request_hash = ?1 AND expires_at > ?2
      RETURNING attempts
    `, [requestHash, now]);
    return result.rows[0]?.attempts ?? 5;
  }

  async consumeAuthorizationRequestAndCreateCode(
    requestHash: string,
    codeHash: string,
    expiresAt: Date,
    connectionId: string,
  ): Promise<McpOAuthAuthorizationRequest | null> {
    return this.pool.withLock("mcp-oauth-tx", async (client) => {
      const now = new Date().toISOString();
      const consumed = await client.query<AuthorizationRequestRow>(`
        DELETE FROM mcp_oauth_authorization_requests
        WHERE request_hash = ?1 AND expires_at > ?2 AND attempts < 5
        RETURNING *
      `, [requestHash, now]);
      const row = consumed.rows[0];
      if (!row) {
        return null;
      }
      await client.query(`
        INSERT INTO mcp_oauth_authorization_codes (
          code_hash, client_id, connection_id, redirect_uri, code_challenge, resource, scope, expires_at, created_at
        ) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9)
      `, [
        codeHash,
        row.client_id,
        connectionId,
        row.redirect_uri,
        row.code_challenge,
        row.resource,
        row.scope,
        expiresAt.toISOString(),
        now,
      ]);
      return mapAuthorizationRequest(row);
    });
  }

  async consumeAuthorizationRequest(requestHash: string): Promise<McpOAuthAuthorizationRequest | null> {
    const now = new Date().toISOString();
    const result = await this.pool.query<AuthorizationRequestRow>(`
      DELETE FROM mcp_oauth_authorization_requests
      WHERE request_hash = ?1 AND expires_at > ?2
      RETURNING *
    `, [requestHash, now]);
    return result.rows[0] ? mapAuthorizationRequest(result.rows[0]) : null;
  }

  async consumeAuthorizationCode(codeHash: string): Promise<McpOAuthAuthorizationCode | null> {
    const now = new Date().toISOString();
    const result = await this.pool.query<AuthorizationCodeRow>(`
      DELETE FROM mcp_oauth_authorization_codes
      WHERE code_hash = ?1 AND expires_at > ?2
      RETURNING *
    `, [codeHash, now]);
    return result.rows[0] ? mapAuthorizationCode(result.rows[0]) : null;
  }

  async issueTokenPair(token: NewMcpOAuthTokenRecord): Promise<void> {
    const now = new Date().toISOString();
    await this.pool.query(`
      INSERT INTO mcp_oauth_tokens (
        id, access_token_hash, refresh_token_hash, client_id, connection_id, resource, scope,
        access_expires_at, refresh_expires_at, created_at
      ) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)
    `, [
      randomUUID(),
      token.accessTokenHash,
      token.refreshTokenHash,
      token.clientId,
      token.connectionId,
      token.resource,
      token.scope,
      token.accessExpiresAt.toISOString(),
      token.refreshExpiresAt.toISOString(),
      now,
    ]);
  }

  async rotateRefreshToken(
    refreshTokenHash: string,
    clientId: string,
    resource: string,
    replacement: NewMcpOAuthTokenRecord,
  ): Promise<McpOAuthTokenRecord | null> {
    return this.pool.withLock("mcp-oauth-tx", async (client) => {
      const now = new Date().toISOString();
      const result = await client.query<TokenRow>(`
        UPDATE mcp_oauth_tokens
        SET revoked_at = ?4
        WHERE refresh_token_hash = ?1
          AND client_id = ?2
          AND resource = ?3
          AND revoked_at IS NULL
          AND refresh_expires_at > ?4
        RETURNING *
      `, [refreshTokenHash, clientId, resource, now]);
      const current = result.rows[0];
      if (!current) {
        return null;
      }
      await client.query(`
        INSERT INTO mcp_oauth_tokens (
          id, access_token_hash, refresh_token_hash, client_id, connection_id, resource, scope,
          access_expires_at, refresh_expires_at, created_at
        ) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)
      `, [
        randomUUID(),
        replacement.accessTokenHash,
        replacement.refreshTokenHash,
        replacement.clientId,
        replacement.connectionId,
        replacement.resource,
        replacement.scope,
        replacement.accessExpiresAt.toISOString(),
        replacement.refreshExpiresAt.toISOString(),
        now,
      ]);
      return mapToken(current);
    });
  }

  async findAccessToken(accessTokenHash: string): Promise<McpOAuthTokenRecord | null> {
    const now = new Date().toISOString();
    const result = await this.pool.query<TokenRow>(`
      SELECT * FROM mcp_oauth_tokens
      WHERE access_token_hash = ?1
        AND revoked_at IS NULL
        AND access_expires_at > ?2
    `, [accessTokenHash, now]);
    return result.rows[0] ? mapToken(result.rows[0]) : null;
  }

  async findRefreshToken(
    refreshTokenHash: string,
    clientId: string,
    resource: string,
  ): Promise<McpOAuthTokenRecord | null> {
    const now = new Date().toISOString();
    const result = await this.pool.query<TokenRow>(`
      SELECT * FROM mcp_oauth_tokens
      WHERE refresh_token_hash = ?1
        AND client_id = ?2
        AND resource = ?3
        AND revoked_at IS NULL
        AND refresh_expires_at > ?4
    `, [refreshTokenHash, clientId, resource, now]);
    return result.rows[0] ? mapToken(result.rows[0]) : null;
  }

  async revokeToken(tokenHash: string): Promise<void> {
    const now = new Date().toISOString();
    await this.pool.query(`
      UPDATE mcp_oauth_tokens
      SET revoked_at = COALESCE(revoked_at, ?2)
      WHERE access_token_hash = ?1 OR refresh_token_hash = ?1
    `, [tokenHash, now]);
  }

  private async cleanupExpired(): Promise<void> {
    const now = new Date().toISOString();
    const sevenDaysAgo = new Date(Date.now() - 7 * 24 * 60 * 60 * 1_000).toISOString();
    const thirtyDaysAgo = new Date(Date.now() - 30 * 24 * 60 * 60 * 1_000).toISOString();
    await this.pool.query("DELETE FROM mcp_oauth_authorization_requests WHERE expires_at <= ?1", [now]);
    await this.pool.query("DELETE FROM mcp_oauth_authorization_codes WHERE expires_at <= ?1", [now]);
    await this.pool.query(`
      DELETE FROM mcp_oauth_tokens
      WHERE refresh_expires_at <= ?1 OR (revoked_at IS NOT NULL AND revoked_at < ?2)
    `, [now, sevenDaysAgo]);
    await this.pool.query(`
      DELETE FROM mcp_oauth_clients
      WHERE created_at < ?1
        AND NOT EXISTS (SELECT 1 FROM mcp_oauth_tokens WHERE mcp_oauth_tokens.client_id = mcp_oauth_clients.client_id)
        AND NOT EXISTS (SELECT 1 FROM mcp_oauth_authorization_codes WHERE mcp_oauth_authorization_codes.client_id = mcp_oauth_clients.client_id)
        AND NOT EXISTS (SELECT 1 FROM mcp_oauth_authorization_requests WHERE mcp_oauth_authorization_requests.client_id = mcp_oauth_clients.client_id)
    `, [thirtyDaysAgo]);
  }
}

export { SqliteMcpOAuthStore as PostgresMcpOAuthStore };
