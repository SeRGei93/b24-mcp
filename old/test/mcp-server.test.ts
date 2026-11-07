import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { describe, expect, it } from "vitest";
import type { AppContainer } from "../src/container.js";
import type { ConnectionRecord } from "../src/types.js";
import { createMcpServer } from "../src/mcp/server.js";

describe("MCP server", () => {
  it("advertises the intended tools and never returns connection secrets", async () => {
    const record: ConnectionRecord = {
      id: "00000000-0000-4000-8000-000000000001",
      domain: "example.bitrix24.com",
      memberId: "member",
      bitrixUserId: "1",
      clientEndpoint: "https://example.bitrix24.com/rest/",
      accessTokenEncrypted: "must-not-leak-access",
      refreshTokenEncrypted: "must-not-leak-refresh",
      scope: "crm",
      expiresAt: new Date("2030-01-01T00:00:00.000Z"),
      mode: "read_only",
      adminToolsEnabled: false,
      status: "active",
      createdAt: new Date("2026-01-01T00:00:00.000Z"),
      updatedAt: new Date("2026-01-01T00:00:00.000Z"),
    };
    const container = { connections: { list: async () => [record] } } as unknown as AppContainer;
    const server = createMcpServer(container);
    const client = new Client({ name: "test-client", version: "1.0.0" });
    const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
    await server.connect(serverTransport);
    await client.connect(clientTransport);
    try {
      const tools = await client.listTools();
      expect(tools.tools).toHaveLength(18);
      expect(tools.tools.map((tool) => tool.name)).toContain("bitrix_confirm_operation");
      expect(tools.tools.map((tool) => tool.name)).toContain("bitrix_list_product_rows");
      expect(tools.tools.map((tool) => tool.name)).not.toContain("bitrix_call_raw_method");

      const response = await client.callTool({ name: "bitrix_list_connections", arguments: {} });
      const serialized = JSON.stringify(response);
      expect(serialized).toContain("example.bitrix24.com");
      expect(serialized).not.toContain("must-not-leak-access");
      expect(serialized).not.toContain("must-not-leak-refresh");
    } finally {
      await client.close();
      await server.close();
    }
  });

  it("limits an OAuth client to the Bitrix24 connection bound to its token", async () => {
    const authorizedId = "00000000-0000-4000-8000-000000000001";
    const otherId = "00000000-0000-4000-8000-000000000002";
    const connection = (id: string, domain: string): ConnectionRecord => ({
      id,
      domain,
      memberId: `member-${id}`,
      bitrixUserId: "1",
      clientEndpoint: `https://${domain}/rest/`,
      accessTokenEncrypted: "encrypted-access",
      refreshTokenEncrypted: "encrypted-refresh",
      scope: "crm",
      expiresAt: new Date("2030-01-01T00:00:00.000Z"),
      mode: "read_only",
      adminToolsEnabled: false,
      status: "active",
      createdAt: new Date("2026-01-01T00:00:00.000Z"),
      updatedAt: new Date("2026-01-01T00:00:00.000Z"),
    });
    const records = [
      connection(authorizedId, "authorized.bitrix24.com"),
      connection(otherId, "other.bitrix24.com"),
    ];
    const container = {
      connections: {
        list: async () => records,
        get: async (id: string) => records.find((record) => record.id === id),
      },
    } as unknown as AppContainer;
    const server = createMcpServer(container, authorizedId);
    const client = new Client({ name: "scoped-client", version: "1.0.0" });
    const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
    await server.connect(serverTransport);
    await client.connect(clientTransport);
    try {
      const listed = await client.callTool({ name: "bitrix_list_connections", arguments: {} });
      const serialized = JSON.stringify(listed);
      expect(serialized).toContain("authorized.bitrix24.com");
      expect(serialized).not.toContain("other.bitrix24.com");

      const denied = await client.callTool({
        name: "bitrix_discover_portal",
        arguments: { connectionId: otherId, refresh: false },
      });
      expect(denied.isError).toBe(true);
      expect(JSON.stringify(denied)).toContain("CONNECTION_ACCESS_DENIED");
    } finally {
      await client.close();
      await server.close();
    }
  });
});
