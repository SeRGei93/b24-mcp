import { describe, expect, it, vi } from "vitest";
import type { AppContainer } from "../src/container.js";
import { buildHttpApp } from "../src/api/app.js";

function container(): AppContainer {
  return {
    config: {
      logLevel: "silent",
      appUrl: "https://mcp.example.com",
      mcpAuthToken: "a".repeat(32),
    },
    pool: { query: vi.fn(async () => ({ rows: [{ value: 1 }] })) },
    mcpOAuth: {
      verifyAuthorizationHeader: vi.fn(async (authorization?: string) => authorization === `Bearer ${"a".repeat(32)}`
        ? { clientId: "test", scopes: ["mcp:tools"], resource: "https://mcp.example.com/mcp" }
        : null),
      challenge: vi.fn(() => "Bearer resource_metadata=\"https://mcp.example.com/.well-known/oauth-protected-resource/mcp\", scope=\"mcp:tools\", error=\"invalid_token\""),
      protectedResourceMetadata: vi.fn(() => ({ resource: "https://mcp.example.com/mcp" })),
      authorizationServerMetadata: vi.fn(() => ({ issuer: "https://mcp.example.com" })),
    },
  } as unknown as AppContainer;
}

describe("HTTP app security", () => {
  it("requires bearer authentication on OAuth initiation", async () => {
    const app = buildHttpApp(container());
    try {
      const response = await app.inject({ method: "GET", url: "/oauth/bitrix/start?domain=example.bitrix24.com" });
      expect(response.statusCode).toBe(401);
      expect(response.headers["www-authenticate"]).toContain("/.well-known/oauth-protected-resource/mcp");
    } finally {
      await app.close();
    }
  });

  it("rejects a cross-origin authenticated request", async () => {
    const app = buildHttpApp(container());
    try {
      const response = await app.inject({
        method: "GET",
        url: "/oauth/bitrix/start?domain=example.bitrix24.com",
        headers: { authorization: `Bearer ${"a".repeat(32)}`, origin: "https://evil.example" },
      });
      expect(response.statusCode).toBe(403);
      expect(response.json().error).toBe("ORIGIN_NOT_ALLOWED");
    } finally {
      await app.close();
    }
  });

  it("reports database health without authentication", async () => {
    const app = buildHttpApp(container());
    try {
      const response = await app.inject({ method: "GET", url: "/health" });
      expect(response.statusCode).toBe(200);
      expect(response.json()).toEqual({ status: "ok", database: "connected" });
    } finally {
      await app.close();
    }
  });

  it("parses Bitrix24's form-encoded installation auth object", async () => {
    const current = container();
    const handleInstallation = vi.fn(async () => ({
      id: "00000000-0000-4000-8000-000000000001",
      domain: "example.bitrix24.com",
      memberId: "member",
      bitrixUserId: "7",
      scope: "crm",
      expiresAt: new Date("2030-01-01T00:00:00.000Z"),
      mode: "read_only",
      adminToolsEnabled: false,
      status: "active",
      createdAt: new Date("2026-01-01T00:00:00.000Z"),
      updatedAt: new Date("2026-01-01T00:00:00.000Z"),
    }));
    current.oauth = { handleInstallation } as unknown as AppContainer["oauth"];
    const app = buildHttpApp(current);
    try {
      const body = new URLSearchParams({
        "auth[access_token]": "access",
        "auth[refresh_token]": "refresh",
        "auth[domain]": "example.bitrix24.com",
      });
      const response = await app.inject({
        method: "POST",
        url: "/oauth/bitrix/install",
        headers: { "content-type": "application/x-www-form-urlencoded" },
        body: body.toString(),
      });
      expect(response.statusCode).toBe(200);
      expect(handleInstallation).toHaveBeenCalledWith({
        access_token: "access",
        refresh_token: "refresh",
        domain: "example.bitrix24.com",
      });
    } finally {
      await app.close();
    }
  });

  it("prevents a connection-scoped OAuth token from refreshing another connection or starting standalone OAuth", async () => {
    const current = container();
    const scopedToken = "b".repeat(32);
    const getTokens = vi.fn();
    current.tokens = { get: getTokens } as unknown as AppContainer["tokens"];
    current.mcpOAuth.verifyAuthorizationHeader = vi.fn(async (authorization?: string) =>
      authorization === `Bearer ${scopedToken}`
        ? {
            clientId: "scoped-client",
            connectionId: "00000000-0000-4000-8000-000000000001",
            scopes: ["mcp:tools"],
            resource: "https://mcp.example.com/mcp",
          }
        : null);

    const app = buildHttpApp(current);
    try {
      const refreshOther = await app.inject({
        method: "POST",
        url: "/oauth/bitrix/refresh",
        headers: { authorization: `Bearer ${scopedToken}` },
        payload: { connection_id: "00000000-0000-4000-8000-000000000002" },
      });
      expect(refreshOther.statusCode).toBe(403);
      expect(refreshOther.json().error).toBe("CONNECTION_ACCESS_DENIED");
      expect(getTokens).not.toHaveBeenCalled();

      const startStandalone = await app.inject({
        method: "GET",
        url: "/oauth/bitrix/start?domain=example.bitrix24.com&mode=write_with_confirmation",
        headers: { authorization: `Bearer ${scopedToken}` },
      });
      expect(startStandalone.statusCode).toBe(403);
      expect(startStandalone.json().error).toBe("CONNECTION_ACCESS_DENIED");
    } finally {
      await app.close();
    }
  });
});
