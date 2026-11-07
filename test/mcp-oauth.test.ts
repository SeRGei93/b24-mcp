import { createHash, randomBytes, randomUUID } from "node:crypto";
import { describe, expect, it, vi } from "vitest";
import {
  OAuthMetadataSchema,
  OAuthProtectedResourceMetadataSchema,
} from "@modelcontextprotocol/sdk/shared/auth.js";
import type { AppConfig } from "../src/config/env.js";
import type { AppContainer } from "../src/container.js";
import type { ConnectionRecord } from "../src/types.js";
import { buildHttpApp } from "../src/api/app.js";
import { McpOAuthService, isSafeOAuthRedirectUri, redirectUriMatches } from "../src/mcp/oauth/service.js";
import type {
  McpOAuthAuthorizationCode,
  McpOAuthAuthorizationRequest,
  McpOAuthClient,
  McpOAuthStore,
  McpOAuthTokenRecord,
  NewMcpOAuthTokenRecord,
} from "../src/mcp/oauth/types.js";

const staticToken = "a".repeat(32);
const connectionId = "00000000-0000-4000-8000-000000000001";

function config(): AppConfig {
  return {
    appUrl: "https://mcp.example.com",
    mcpAuthToken: staticToken,
    mcpOAuthRequestTtlSeconds: 600,
    mcpOAuthCodeTtlSeconds: 300,
    mcpOAuthAccessTokenTtlSeconds: 3600,
    mcpOAuthRefreshTokenTtlSeconds: 2_592_000,
    logLevel: "silent",
    tokenEncryptionKey: randomBytes(32).toString("base64"),
  } as AppConfig;
}

function testContainer(service: McpOAuthService, oauth?: Partial<AppContainer["oauth"]>): AppContainer {
  return {
    config: config(),
    mcpOAuth: service,
    oauth,
    pool: { query: vi.fn(async () => ({ rows: [{ value: 1 }] })) },
  } as unknown as AppContainer;
}

describe("MCP OAuth 2.1", () => {
  it("allows native loopback callback ports without weakening other redirect matching", () => {
    expect(redirectUriMatches("http://127.0.0.1:49152/callback", "http://127.0.0.1:3000/callback")).toBe(true);
    expect(redirectUriMatches("http://localhost:49152/callback", "http://127.0.0.1:3000/callback")).toBe(false);
    expect(redirectUriMatches("https://client.example/callback", "https://client.example/other")).toBe(false);
    expect(isSafeOAuthRedirectUri("http://[::1]:43123/callback")).toBe(true);
  });

  it("publishes discovery metadata and a standards-based authentication challenge", async () => {
    const service = new McpOAuthService(config(), new MemoryMcpOAuthStore());
    const app = buildHttpApp(testContainer(service));
    try {
      const resource = await app.inject({ method: "GET", url: "/.well-known/oauth-protected-resource/mcp" });
      expect(resource.statusCode).toBe(200);
      expect(resource.json()).toMatchObject({
        resource: "https://mcp.example.com/mcp",
        authorization_servers: ["https://mcp.example.com"],
        scopes_supported: ["mcp:tools"],
      });
      expect(() => OAuthProtectedResourceMetadataSchema.parse(resource.json())).not.toThrow();

      const metadata = await app.inject({ method: "GET", url: "/.well-known/oauth-authorization-server" });
      expect(metadata.json()).toMatchObject({
        authorization_endpoint: "https://mcp.example.com/authorize",
        token_endpoint: "https://mcp.example.com/token",
        registration_endpoint: "https://mcp.example.com/register",
        code_challenge_methods_supported: ["S256"],
      });
      expect(() => OAuthMetadataSchema.parse(metadata.json())).not.toThrow();

      const unauthorized = await app.inject({ method: "POST", url: "/mcp", payload: {} });
      expect(unauthorized.statusCode).toBe(401);
      expect(unauthorized.headers["www-authenticate"]).toContain("resource_metadata=");
    } finally {
      await app.close();
    }
  });

  it("federates through Bitrix24 and completes PKCE, refresh rotation, and revocation", async () => {
    const store = new MemoryMcpOAuthStore();
    const service = new McpOAuthService(config(), store);
    let pendingRequestHash = "";
    const createMcpAuthorizationUrl = vi.fn(async (requestHash: string) => {
      pendingRequestHash = requestHash;
      return "https://example.bitrix24.com/oauth/authorize/?client_id=local.test&state=bitrix-state";
    });
    const handleCallbackWithContext = vi.fn(async () => ({
      connection: { id: connectionId } as ConnectionRecord,
      mcpRequestHash: pendingRequestHash,
    }));
    const app = buildHttpApp(testContainer(service, {
      createMcpAuthorizationUrl,
      handleCallbackWithContext,
    }));
    const redirectUri = "http://127.0.0.1:43123/callback";
    const verifier = "v".repeat(64);
    const challenge = createHash("sha256").update(verifier).digest("base64url");
    try {
      const registration = await app.inject({
        method: "POST",
        url: "/register",
        payload: {
          redirect_uris: [redirectUri],
          token_endpoint_auth_method: "none",
          grant_types: ["authorization_code", "refresh_token"],
          response_types: ["code"],
          client_name: "Codex test client",
        },
      });
      expect(registration.statusCode).toBe(201);
      const clientId = registration.json().client_id as string;

      const authorizeUrl = new URL("https://mcp.example.com/authorize");
      authorizeUrl.search = new URLSearchParams({
        response_type: "code",
        client_id: clientId,
        redirect_uri: redirectUri,
        code_challenge: challenge,
        code_challenge_method: "S256",
        state: "state-123",
        scope: "mcp:tools",
        resource: "https://mcp.example.com/mcp",
      }).toString();
      const consentPage = await app.inject({ method: "GET", url: `${authorizeUrl.pathname}${authorizeUrl.search}` });
      expect(consentPage.statusCode).toBe(200);
      expect(consentPage.headers["x-frame-options"]).toBe("DENY");
      expect(consentPage.body).toContain("Codex test client");
      expect(createMcpAuthorizationUrl).not.toHaveBeenCalled();

      const requestTokenMatch = /name="request_token" value="([^"]+)"/.exec(consentPage.body);
      const csrfTokenMatch = /name="csrf_token" value="([^"]+)"/.exec(consentPage.body);
      const setCookieHeader = String(consentPage.headers["set-cookie"] ?? "");
      const cookiePair = setCookieHeader.split(";")[0]!;
      expect(requestTokenMatch?.[1]).toBeTruthy();
      expect(csrfTokenMatch?.[1]).toBeTruthy();
      expect(cookiePair).toContain("mcp_oauth_csrf=");

      const csrfRejected = await app.inject({
        method: "POST",
        url: "/authorize",
        headers: { "content-type": "application/x-www-form-urlencoded" },
        payload: new URLSearchParams({
          request_token: requestTokenMatch![1]!,
          csrf_token: "wrong-csrf",
          decision: "approve",
        }).toString(),
      });
      expect(csrfRejected.statusCode).toBe(403);
      expect(createMcpAuthorizationUrl).not.toHaveBeenCalled();

      const authorization = await app.inject({
        method: "POST",
        url: "/authorize",
        headers: {
          "content-type": "application/x-www-form-urlencoded",
          cookie: cookiePair,
        },
        payload: new URLSearchParams({
          request_token: requestTokenMatch![1]!,
          csrf_token: csrfTokenMatch![1]!,
          decision: "approve",
        }).toString(),
      });
      expect(authorization.statusCode).toBe(302);
      expect(authorization.headers.location).toContain("example.bitrix24.com/oauth/authorize/");
      expect(createMcpAuthorizationUrl).toHaveBeenCalledOnce();

      const bitrixCallback = await app.inject({
        method: "GET",
        url: "/oauth/bitrix/callback?code=bitrix-code&state=bitrix-state&domain=example.bitrix24.com",
      });
      expect(bitrixCallback.statusCode).toBe(302);
      const callback = new URL(bitrixCallback.headers.location!);
      expect(callback.searchParams.get("state")).toBe("state-123");
      expect(handleCallbackWithContext).toHaveBeenCalledOnce();

      const token = await app.inject({
        method: "POST",
        url: "/token",
        headers: { "content-type": "application/x-www-form-urlencoded" },
        payload: new URLSearchParams({
          grant_type: "authorization_code",
          client_id: clientId,
          code: callback.searchParams.get("code")!,
          code_verifier: verifier,
          redirect_uri: redirectUri,
          resource: "https://mcp.example.com/mcp",
        }).toString(),
      });
      expect(token.statusCode).toBe(200);
      const firstTokens = token.json() as { access_token: string; refresh_token: string };
      expect((await service.verifyAuthorizationHeader(`Bearer ${firstTokens.access_token}`))?.clientId).toBe(clientId);
      expect((await service.verifyAuthorizationHeader(`Bearer ${firstTokens.access_token}`))?.connectionId).toBe(connectionId);

      const refresh = await app.inject({
        method: "POST",
        url: "/token",
        headers: { "content-type": "application/x-www-form-urlencoded" },
        payload: new URLSearchParams({
          grant_type: "refresh_token",
          client_id: clientId,
          refresh_token: firstTokens.refresh_token,
          resource: "https://mcp.example.com/mcp",
        }).toString(),
      });
      expect(refresh.statusCode).toBe(200);
      const secondTokens = refresh.json() as { access_token: string; refresh_token: string };
      expect(secondTokens.access_token).not.toBe(firstTokens.access_token);
      expect(await service.verifyAuthorizationHeader(`Bearer ${firstTokens.access_token}`)).toBeNull();
      expect(await service.verifyAuthorizationHeader(`Bearer ${secondTokens.access_token}`)).not.toBeNull();

      const reuse = await app.inject({
        method: "POST",
        url: "/token",
        headers: { "content-type": "application/x-www-form-urlencoded" },
        payload: new URLSearchParams({
          grant_type: "refresh_token",
          client_id: clientId,
          refresh_token: firstTokens.refresh_token,
        }).toString(),
      });
      expect(reuse.statusCode).toBe(400);
      expect(reuse.json().error).toBe("invalid_grant");

      const revoked = await app.inject({
        method: "POST",
        url: "/revoke",
        headers: { "content-type": "application/x-www-form-urlencoded" },
        payload: new URLSearchParams({ token: secondTokens.access_token }).toString(),
      });
      expect(revoked.statusCode).toBe(200);
      expect(await service.verifyAuthorizationHeader(`Bearer ${secondTokens.access_token}`)).toBeNull();
    } finally {
      await app.close();
    }
  });

  it("rejects unsafe redirects and a mismatched PKCE verifier", async () => {
    const store = new MemoryMcpOAuthStore();
    const service = new McpOAuthService(config(), store);
    await expect(service.registerClient({ redirectUris: ["http://public.example.com/callback"] })).rejects.toMatchObject({
      errorCode: "invalid_redirect_uri",
    });

    const client = await service.registerClient({
      redirectUris: ["https://client.example.com/callback"],
      clientName: "Security test",
    });
    const verifier = "z".repeat(64);
    const page = await service.beginAuthorization({
      responseType: "code",
      clientId: client.client_id,
      redirectUri: "https://client.example.com/callback",
      codeChallenge: createHash("sha256").update(verifier).digest("base64url"),
      codeChallengeMethod: "S256",
      resource: "https://mcp.example.com/mcp",
    });
    const callback = new URL(await service.completeBitrixAuthorization(page.requestHash, connectionId));
    await expect(service.exchangeAuthorizationCode({
      clientId: client.client_id,
      code: callback.searchParams.get("code")!,
      codeVerifier: "x".repeat(64),
      redirectUri: "https://client.example.com/callback",
    })).rejects.toMatchObject({ errorCode: "invalid_grant" });
  });
});

class MemoryMcpOAuthStore implements McpOAuthStore {
  private readonly clients = new Map<string, McpOAuthClient>();
  private readonly requests = new Map<string, McpOAuthAuthorizationRequest>();
  private readonly codes = new Map<string, McpOAuthAuthorizationCode>();
  private readonly tokens = new Map<string, McpOAuthTokenRecord>();

  async registerClient(client: McpOAuthClient): Promise<void> {
    this.clients.set(client.clientId, client);
  }

  async getClient(clientId: string): Promise<McpOAuthClient | null> {
    return this.clients.get(clientId) ?? null;
  }

  async createAuthorizationRequest(request: McpOAuthAuthorizationRequest): Promise<void> {
    this.requests.set(request.requestHash, request);
  }

  async getAuthorizationRequest(requestHash: string): Promise<McpOAuthAuthorizationRequest | null> {
    const request = this.requests.get(requestHash);
    return request && request.expiresAt > new Date() && request.attempts < 5 ? request : null;
  }

  async incrementAuthorizationAttempts(requestHash: string): Promise<number> {
    const request = this.requests.get(requestHash);
    if (!request) return 5;
    request.attempts += 1;
    return request.attempts;
  }

  async consumeAuthorizationRequestAndCreateCode(
    requestHash: string,
    codeHash: string,
    expiresAt: Date,
    authorizedConnectionId: string,
  ): Promise<McpOAuthAuthorizationRequest | null> {
    const request = await this.getAuthorizationRequest(requestHash);
    if (!request) return null;
    this.requests.delete(requestHash);
    this.codes.set(codeHash, {
      codeHash,
      clientId: request.clientId,
      connectionId: authorizedConnectionId,
      redirectUri: request.redirectUri,
      codeChallenge: request.codeChallenge,
      resource: request.resource,
      scope: request.scope,
      expiresAt,
    });
    return request;
  }

  async consumeAuthorizationRequest(requestHash: string): Promise<McpOAuthAuthorizationRequest | null> {
    const request = this.requests.get(requestHash) ?? null;
    this.requests.delete(requestHash);
    return request;
  }

  async consumeAuthorizationCode(codeHash: string): Promise<McpOAuthAuthorizationCode | null> {
    const code = this.codes.get(codeHash) ?? null;
    this.codes.delete(codeHash);
    return code && code.expiresAt > new Date() ? code : null;
  }

  async issueTokenPair(token: NewMcpOAuthTokenRecord): Promise<void> {
    this.addToken(token);
  }

  async rotateRefreshToken(
    refreshTokenHash: string,
    clientId: string,
    resource: string,
    replacement: NewMcpOAuthTokenRecord,
  ): Promise<McpOAuthTokenRecord | null> {
    const current = [...this.tokens.values()].find((token) => token.refreshTokenHash === refreshTokenHash
      && token.clientId === clientId
      && token.resource === resource
      && !token.revokedAt
      && token.refreshExpiresAt > new Date());
    if (!current) return null;
    current.revokedAt = new Date();
    this.addToken(replacement);
    return current;
  }

  async findAccessToken(accessTokenHash: string): Promise<McpOAuthTokenRecord | null> {
    const token = this.tokens.get(accessTokenHash);
    return token && !token.revokedAt && token.accessExpiresAt > new Date() ? token : null;
  }

  async findRefreshToken(
    refreshTokenHash: string,
    clientId: string,
    resource: string,
  ): Promise<McpOAuthTokenRecord | null> {
    return [...this.tokens.values()].find((token) => token.refreshTokenHash === refreshTokenHash
      && token.clientId === clientId
      && token.resource === resource
      && !token.revokedAt
      && token.refreshExpiresAt > new Date()) ?? null;
  }

  async revokeToken(tokenHash: string): Promise<void> {
    for (const token of this.tokens.values()) {
      if (token.accessTokenHash === tokenHash || token.refreshTokenHash === tokenHash) token.revokedAt = new Date();
    }
  }

  private addToken(token: NewMcpOAuthTokenRecord): void {
    this.tokens.set(token.accessTokenHash, {
      id: randomUUID(),
      ...token,
      revokedAt: null,
      createdAt: new Date(),
    });
  }
}
