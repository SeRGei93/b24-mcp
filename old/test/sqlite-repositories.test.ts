import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createPool, runMigrations, type DbPool } from "../src/db/client.js";
import {
  AuditRepository,
  ConnectionRepository,
  OAuthStateRepository,
  PendingOperationRepository,
  SchemaCacheRepository,
} from "../src/db/repositories.js";
import { SqliteMcpOAuthStore } from "../src/mcp/oauth/repository.js";

const logger = { debug: vi.fn(), info: vi.fn(), warn: vi.fn(), error: vi.fn() };

describe("SQLite repositories and migrations", () => {
  let pool: DbPool;

  beforeEach(async () => {
    pool = createPool({ databasePath: ":memory:" });
    await runMigrations(pool, logger);
  });

  afterEach(async () => {
    await pool.end();
  });

  it("applies migrations idempotently", async () => {
    await expect(runMigrations(pool, logger)).resolves.toBeUndefined();
    const result = await pool.query<{ id: string }>("SELECT id FROM schema_migrations");
    expect(result.rows).toHaveLength(1);
    expect(result.rows[0]?.id).toBe("001_initial_sqlite");
  });

  it("manages Bitrix connections, OAuth states, schema cache, pending operations, and audit logs", async () => {
    const connections = new ConnectionRepository(pool);
    const states = new OAuthStateRepository(pool);
    const cache = new SchemaCacheRepository(pool);
    const operations = new PendingOperationRepository(pool);
    const audit = new AuditRepository(pool);

    const created = await connections.upsert({
      domain: "example.bitrix24.com",
      memberId: "member-1",
      bitrixUserId: "42",
      clientEndpoint: "https://example.bitrix24.com/rest/",
      accessTokenEncrypted: "enc-access-1",
      refreshTokenEncrypted: "enc-refresh-1",
      scope: "crm",
      expiresAt: new Date(Date.now() + 3_600_000),
      mode: "read_only",
      adminToolsEnabled: false,
    });
    expect(created.id).toBeTruthy();
    expect(created.adminToolsEnabled).toBe(false);
    expect(created.status).toBe("active");

    const updated = await connections.upsert({
      domain: "example.bitrix24.com",
      memberId: "member-1",
      bitrixUserId: "42",
      clientEndpoint: "https://example.bitrix24.com/rest/",
      accessTokenEncrypted: "enc-access-2",
      refreshTokenEncrypted: "enc-refresh-2",
      scope: "crm,user",
      expiresAt: new Date(Date.now() + 7_200_000),
      mode: "write_with_confirmation",
      adminToolsEnabled: true,
    });
    expect(updated.id).toBe(created.id);
    expect(updated.accessTokenEncrypted).toBe("enc-access-2");
    expect(updated.mode).toBe("write_with_confirmation");
    expect(updated.adminToolsEnabled).toBe(true);

    await expect(connections.upsert({
      domain: "other.bitrix24.com",
      memberId: "member-1",
      bitrixUserId: "42",
      clientEndpoint: "https://other.bitrix24.com/rest/",
      accessTokenEncrypted: "enc-access-3",
      refreshTokenEncrypted: "enc-refresh-3",
      scope: "crm",
      expiresAt: new Date(Date.now() + 3_600_000),
      mode: "read_only",
      adminToolsEnabled: false,
    })).rejects.toThrow(/different portal domain/i);

    await states.create("raw-state-1", "example.bitrix24.com", "read_only", new Date(Date.now() + 60_000));
    const consumedState = await states.consume("raw-state-1");
    expect(consumedState).toEqual({ domain: "example.bitrix24.com", mode: "read_only", mcpRequestHash: undefined });
    expect(await states.consume("raw-state-1")).toBeNull();

    await cache.set(created.id, "entity-types", [{ entityTypeId: 2, title: "Deal" }]);
    expect(await cache.get(created.id, "entity-types", 60)).toEqual([{ entityTypeId: 2, title: "Deal" }]);
    expect(await cache.clear(created.id)).toBe(1);
    expect(await cache.get(created.id, "entity-types", 60)).toBeNull();

    const pending = await operations.create({
      connectionId: created.id,
      operationType: "create_item",
      method: "crm.item.add",
      params: { entityTypeId: 2, fields: { title: "New Deal" } },
      humanSummary: "Create an item in CRM entity type 2.",
      expiresAt: new Date(Date.now() + 60_000),
    });
    expect(pending.status).toBe("pending");
    expect(pending.params).toEqual({ entityTypeId: 2, fields: { title: "New Deal" } });

    const claimed = await operations.claim(pending.id);
    expect(claimed?.status).toBe("executing");
    expect(await operations.claim(pending.id)).toBeNull();

    await operations.complete(pending.id, { item: { id: 101 } });
    const completed = await operations.get(pending.id);
    expect(completed.status).toBe("completed");
    expect(completed.result).toEqual({ item: { id: 101 } });

    await audit.record({
      connectionId: created.id,
      operationId: pending.id,
      action: "create_item",
      outcome: "completed",
      details: { method: "crm.item.add" },
    });
    const logs = await pool.query<{ action: string; outcome: string }>("SELECT action, outcome FROM audit_logs");
    expect(logs.rows).toEqual([{ action: "create_item", outcome: "completed" }]);
  });

  it("persists MCP OAuth 2.1 clients, requests, codes, and rotating tokens in SQLite", async () => {
    const connections = new ConnectionRepository(pool);
    const connection = await connections.upsert({
      domain: "example.bitrix24.com",
      memberId: "member-oauth",
      bitrixUserId: "7",
      clientEndpoint: "https://example.bitrix24.com/rest/",
      accessTokenEncrypted: "enc-access",
      refreshTokenEncrypted: "enc-refresh",
      scope: "crm",
      expiresAt: new Date(Date.now() + 3_600_000),
      mode: "read_only",
      adminToolsEnabled: false,
    });

    const store = new SqliteMcpOAuthStore(pool);
    await store.registerClient({
      clientId: "client-1",
      redirectUris: ["http://127.0.0.1:3000/callback"],
      tokenEndpointAuthMethod: "none",
      grantTypes: ["authorization_code", "refresh_token"],
      responseTypes: ["code"],
      clientName: "SQLite Client",
      scope: "mcp:tools",
      createdAt: new Date(),
    });

    const client = await store.getClient("client-1");
    expect(client?.redirectUris).toEqual(["http://127.0.0.1:3000/callback"]);

    await store.createAuthorizationRequest({
      requestHash: "req-hash-1",
      clientId: "client-1",
      redirectUri: "http://127.0.0.1:3000/callback",
      state: "state-1",
      codeChallenge: "challenge-1",
      resource: "https://mcp.example.com/mcp",
      scope: "mcp:tools",
      expiresAt: new Date(Date.now() + 60_000),
      attempts: 0,
    });

    expect(await store.incrementAuthorizationAttempts("req-hash-1")).toBe(1);
    const consumedReq = await store.consumeAuthorizationRequestAndCreateCode(
      "req-hash-1",
      "code-hash-1",
      new Date(Date.now() + 60_000),
      connection.id,
    );
    expect(consumedReq?.clientId).toBe("client-1");

    const consumedCode = await store.consumeAuthorizationCode("code-hash-1");
    expect(consumedCode?.connectionId).toBe(connection.id);
    expect(await store.consumeAuthorizationCode("code-hash-1")).toBeNull();

    await store.issueTokenPair({
      accessTokenHash: "access-1",
      refreshTokenHash: "refresh-1",
      clientId: "client-1",
      connectionId: connection.id,
      resource: "https://mcp.example.com/mcp",
      scope: "mcp:tools",
      accessExpiresAt: new Date(Date.now() + 3_600_000),
      refreshExpiresAt: new Date(Date.now() + 86_400_000),
    });

    expect((await store.findAccessToken("access-1"))?.connectionId).toBe(connection.id);

    const rotated = await store.rotateRefreshToken("refresh-1", "client-1", "https://mcp.example.com/mcp", {
      accessTokenHash: "access-2",
      refreshTokenHash: "refresh-2",
      clientId: "client-1",
      connectionId: connection.id,
      resource: "https://mcp.example.com/mcp",
      scope: "mcp:tools",
      accessExpiresAt: new Date(Date.now() + 3_600_000),
      refreshExpiresAt: new Date(Date.now() + 86_400_000),
    });
    expect(rotated?.accessTokenHash).toBe("access-1");
    expect(await store.findAccessToken("access-1")).toBeNull();
    expect((await store.findAccessToken("access-2"))?.accessTokenHash).toBe("access-2");

    await store.revokeToken("access-2");
    expect(await store.findAccessToken("access-2")).toBeNull();
  });
});
