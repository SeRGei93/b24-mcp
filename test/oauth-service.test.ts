import { randomBytes } from "node:crypto";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AppConfig } from "../src/config/env.js";
import type { ConnectionRepository, OAuthStateRepository } from "../src/db/repositories.js";
import type { ConnectionRecord } from "../src/types.js";
import { BitrixOAuthService } from "../src/bitrix/oauth.js";
import { TokenCipher } from "../src/security/encryption.js";

const config = {
  bitrixClientId: "client-id",
  bitrixClientSecret: "client-secret",
  bitrixRequestTimeoutMs: 1_000,
  oauthStateTtlSeconds: 600,
  defaultMode: "read_only",
  adminToolsEnabled: false,
} as AppConfig;

afterEach(() => vi.unstubAllGlobals());

describe("BitrixOAuthService", () => {
  it("redirects MCP authorization directly to the configured Bitrix24 portal", async () => {
    const create = vi.fn(async () => undefined);
    const oauth = new BitrixOAuthService(
      { ...config, bitrixPortalDomain: "example.bitrix24.com" },
      { create } as unknown as OAuthStateRepository,
      {} as ConnectionRepository,
      new TokenCipher(randomBytes(32)),
    );

    const location = new URL(await oauth.createMcpAuthorizationUrl("mcp-request-hash"));

    expect(location.origin).toBe("https://example.bitrix24.com");
    expect(location.pathname).toBe("/oauth/authorize/");
    expect(location.searchParams.get("client_id")).toBe("client-id");
    expect(location.searchParams.get("state")).toBeTruthy();
    expect(location.searchParams.has("redirect_uri")).toBe(false);
    expect(create).toHaveBeenCalledWith(
      expect.any(String),
      "example.bitrix24.com",
      "read_only",
      expect.any(Date),
      "mcp-request-hash",
    );
  });

  it("redirects MCP authorization to an explicitly configured custom portal domain", async () => {
    const create = vi.fn(async () => undefined);
    const oauth = new BitrixOAuthService(
      { ...config, bitrixPortalDomain: "https://crm.hub-bnk.com/" },
      { create } as unknown as OAuthStateRepository,
      {} as ConnectionRepository,
      new TokenCipher(randomBytes(32)),
    );

    const location = new URL(await oauth.createMcpAuthorizationUrl("mcp-request-hash"));

    expect(location.origin).toBe("https://crm.hub-bnk.com");
    expect(location.pathname).toBe("/oauth/authorize/");
    expect(create).toHaveBeenCalledWith(
      expect.any(String),
      "crm.hub-bnk.com",
      "read_only",
      expect.any(Date),
      "mcp-request-hash",
    );
  });

  it("learns the OAuth portal from a prior local-app installation", async () => {
    const create = vi.fn(async () => undefined);
    const connections = {
      list: vi.fn(async () => [{ domain: "installed.bitrix24.com", status: "active" }]),
    } as unknown as ConnectionRepository;
    const oauth = new BitrixOAuthService(
      config,
      { create } as unknown as OAuthStateRepository,
      connections,
      new TokenCipher(randomBytes(32)),
    );

    const location = new URL(await oauth.createMcpAuthorizationUrl("mcp-request-hash"));

    expect(location.hostname).toBe("installed.bitrix24.com");
    expect(connections.list).toHaveBeenCalledOnce();
  });

  it("accepts a custom portal callback when the OAuth token domain identifies the OAuth server", async () => {
    const cipher = new TokenCipher(randomBytes(32));
    const states = {
      consume: vi.fn(async () => ({
        domain: "crm.hub-bnk.com",
        mode: "read_only",
        mcpRequestHash: "mcp-request-hash",
      })),
    } as unknown as OAuthStateRepository;
    const upsert = vi.fn(async (input: Record<string, unknown>) => ({ ...input, id: "connection" }) as unknown as ConnectionRecord);
    const connections = { upsert } as unknown as ConnectionRepository;
    const fetchMock = vi.fn(async (input: string | URL | Request, _init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.hostname === "oauth.bitrix.info") {
        return new Response(JSON.stringify({
          access_token: "access-secret",
          client_endpoint: "https://crm.hub-bnk.com/rest/",
          domain: "oauth.bitrix.info",
          expires_in: 3_600,
          member_id: "portal-member",
          refresh_token: "refresh-secret",
          scope: "crm",
          status: "L",
        }), { status: 200 });
      }
      return new Response(JSON.stringify({ result: { ID: "7" } }), { status: 200 });
    });
    vi.stubGlobal("fetch", fetchMock);

    const oauth = new BitrixOAuthService(
      { ...config, bitrixPortalDomain: "https://crm.hub-bnk.com/" },
      states,
      connections,
      cipher,
    );
    const callback = await oauth.handleCallbackWithContext({
      code: "authorization-code",
      state: "state",
      domain: "crm.hub-bnk.com",
      member_id: "portal-member",
    });

    expect(upsert).toHaveBeenCalledOnce();
    expect(callback.mcpRequestHash).toBe("mcp-request-hash");
    const saved = upsert.mock.calls[0]![0];
    expect(saved.domain).toBe("crm.hub-bnk.com");
    expect(saved.bitrixUserId).toBe("7");
    expect(saved.accessTokenEncrypted).not.toContain("access-secret");
    expect(saved.refreshTokenEncrypted).not.toContain("refresh-secret");
    const [requestedUrl, init] = fetchMock.mock.calls[0]!;
    const url = new URL(String(requestedUrl));
    expect(url.origin).toBe("https://oauth.bitrix.info");
    expect(url.searchParams.get("client_secret")).toBe("client-secret");
    expect(url.searchParams.has("redirect_uri")).toBe(false);
    expect(init?.method).toBe("GET");
    expect(String(fetchMock.mock.calls[1]![0])).toBe("https://crm.hub-bnk.com/rest/profile.json");
  });

  it("rejects a token whose REST endpoint belongs to another portal", async () => {
    const states = {
      consume: vi.fn(async () => ({ domain: "example.bitrix24.com", mode: "read_only" })),
    } as unknown as OAuthStateRepository;
    const upsert = vi.fn();
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({
      access_token: "access",
      client_endpoint: "https://other.bitrix24.com/rest/",
      domain: "oauth.bitrix.info",
      expires_in: 3_600,
      member_id: "member",
      refresh_token: "refresh",
      scope: "crm",
    }), { status: 200 })));
    const oauth = new BitrixOAuthService(
      config,
      states,
      { upsert } as unknown as ConnectionRepository,
      new TokenCipher(randomBytes(32)),
    );

    await expect(oauth.handleCallback({ code: "code", state: "state" })).rejects.toThrow(/does not match/i);
    expect(upsert).not.toHaveBeenCalled();
  });

  it("validates and stores an API-only installation callback", async () => {
    const pendingConfig = { ...config, bitrixClientId: "pending-bitrix-app" };
    const upsert = vi.fn(async (input: Record<string, unknown>) => ({ ...input, id: "connection" }) as unknown as ConnectionRecord);
    const fetchMock = vi.fn(async (input: string | URL | Request, _init?: RequestInit) => {
      const url = String(input);
      return new Response(JSON.stringify(url.endsWith("app.info.json")
        ? { result: { CODE: "local.abc123", INSTALLED: true } }
        : { result: { ID: "7" } }), { status: 200 });
    });
    vi.stubGlobal("fetch", fetchMock);
    const oauth = new BitrixOAuthService(
      pendingConfig,
      {} as OAuthStateRepository,
      { upsert } as unknown as ConnectionRepository,
      new TokenCipher(randomBytes(32)),
    );

    await oauth.handleInstallation({
      access_token: "install-access",
      refresh_token: "install-refresh",
      expires_in: "3600",
      scope: "crm,user",
      domain: "example.bitrix24.com",
      member_id: "member",
      user_id: "7",
      client_endpoint: "https://example.bitrix24.com/rest/",
    });

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(String(fetchMock.mock.calls[0]![0])).toBe("https://example.bitrix24.com/rest/app.info.json");
    expect(upsert).toHaveBeenCalledOnce();
    expect(upsert.mock.calls[0]![0].bitrixUserId).toBe("7");
    expect(upsert.mock.calls[0]![0].accessTokenEncrypted).not.toContain("install-access");
  });

  it("validates and stores a Bitrix24 placement installation payload (AUTH_ID / REFRESH_ID + DOMAIN)", async () => {
    const pendingConfig = { ...config, bitrixClientId: "pending-bitrix-app" };
    const upsert = vi.fn(async (input: Record<string, unknown>) => ({ ...input, id: "connection" }) as unknown as ConnectionRecord);
    const fetchMock = vi.fn(async (input: string | URL | Request) => {
      const url = String(input);
      return new Response(JSON.stringify(url.endsWith("app.info.json")
        ? { result: { CODE: "local.abc123", INSTALLED: false } }
        : { result: { ID: "7" } }), { status: 200 });
    });
    vi.stubGlobal("fetch", fetchMock);
    const oauth = new BitrixOAuthService(
      pendingConfig,
      {} as OAuthStateRepository,
      { upsert } as unknown as ConnectionRepository,
      new TokenCipher(randomBytes(32)),
    );

    await oauth.handleInstallation({
      DOMAIN: "example.bitrix24.by",
      PROTOCOL: "1",
      LANG: "ru",
      APP_SID: "sid123",
      AUTH_ID: "placement-access",
      AUTH_EXPIRES: "3600",
      REFRESH_ID: "placement-refresh",
      member_id: "member-by",
      status: "L",
      PLACEMENT: "DEFAULT",
    });

    expect(upsert).toHaveBeenCalledOnce();
    expect(upsert.mock.calls[0]![0].domain).toBe("example.bitrix24.by");
    expect(upsert.mock.calls[0]![0].memberId).toBe("member-by");
    expect(upsert.mock.calls[0]![0].clientEndpoint).toBe("https://example.bitrix24.by/rest/");
  });

  it("rejects installation tokens issued to another configured app", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({
      result: { CODE: "local.other", INSTALLED: true },
    }), { status: 200 })));
    const upsert = vi.fn();
    const oauth = new BitrixOAuthService(
      { ...config, bitrixClientId: "local.expected" },
      {} as OAuthStateRepository,
      { upsert } as unknown as ConnectionRepository,
      new TokenCipher(randomBytes(32)),
    );
    await expect(oauth.handleInstallation({
      access_token: "access",
      refresh_token: "refresh",
      expires_in: 3_600,
      scope: "crm",
      domain: "example.bitrix24.com",
      member_id: "member",
      client_endpoint: "https://example.bitrix24.com/rest/",
    })).rejects.toThrow(/different/i);
    expect(upsert).not.toHaveBeenCalled();
  });
});
