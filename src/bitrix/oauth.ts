import { randomBytes } from "node:crypto";
import { isIP } from "node:net";
import { z } from "zod/v4";
import type { AppConfig } from "../config/env.js";
import type { ConnectionMode, ConnectionRecord } from "../types.js";
import type { ConnectionRepository, OAuthStateRepository } from "../db/repositories.js";
import { AppError, AuthenticationError, safeErrorMessage } from "../errors.js";
import { TokenCipher } from "../security/encryption.js";

const oauthTokenSchema = z.object({
  access_token: z.string().min(1),
  refresh_token: z.string().min(1),
  expires_in: z.coerce.number().int().positive().default(3_600),
  scope: z.union([z.string(), z.array(z.string())]).default(""),
  domain: z.string().min(1).optional(),
  member_id: z.string().min(1),
  user_id: z.union([z.string(), z.number()]).transform(String).optional(),
  client_endpoint: z.string().min(1),
});

const installationAuthSchema = z.object({
  access_token: z.string().min(1),
  refresh_token: z.string().min(1),
  expires_in: z.coerce.number().int().positive().default(3_600),
  scope: z.union([z.string(), z.array(z.string())]).default("").transform((val) => Array.isArray(val) ? val.join(",") : val),
  domain: z.string().min(1),
  member_id: z.string().min(1),
  user_id: z.union([z.string(), z.number()]).transform(String).optional(),
  client_endpoint: z.string().min(1),
});

const appInfoSchema = z.object({
  result: z.object({ CODE: z.string().min(1) }),
});

const profileSchema = z.object({
  result: z.object({ ID: z.union([z.string(), z.number()]).transform(String) }),
});

export type OAuthTokenResponse = z.infer<typeof oauthTokenSchema>;

export class BitrixOAuthError extends AppError {
  constructor(message: string, readonly oauthCode: string, options?: ErrorOptions) {
    super(message, "BITRIX_OAUTH_ERROR", 502, options);
  }
}

export class BitrixOAuthContinuationError extends AppError {
  constructor(
    message: string,
    readonly mcpRequestHash: string,
    readonly oauthErrorCode: "access_denied" | "server_error",
    options?: ErrorOptions,
  ) {
    super(message, "BITRIX_OAUTH_CONTINUATION_ERROR", oauthErrorCode === "access_denied" ? 401 : 502, options);
  }
}

export interface BitrixOAuthCallbackResult {
  connection: ConnectionRecord;
  mcpRequestHash?: string;
}

function parsePortalHost(input: string): string {
  const trimmed = input.trim().toLowerCase();
  let url: URL;
  try {
    url = new URL(trimmed.includes("://") ? trimmed : `https://${trimmed}`);
  } catch {
    throw new AppError("Invalid Bitrix24 portal domain", "INVALID_PORTAL_DOMAIN");
  }

  if (
    url.protocol !== "https:" || url.username || url.password || url.port ||
    (url.pathname !== "/" && url.pathname !== "") || url.search || url.hash
  ) {
    throw new AppError("Portal must be an HTTPS Bitrix24 domain without a path, port, query, or credentials", "INVALID_PORTAL_DOMAIN");
  }

  const host = url.hostname.replace(/\.$/, "");
  if (isIP(host) || host === "localhost") {
    throw new AppError("IP addresses and localhost are not valid Bitrix24 portal domains", "INVALID_PORTAL_DOMAIN");
  }
  return host;
}

export function normalizePortalDomain(input: string, allowedPortalDomains: readonly string[] = []): string {
  const host = parsePortalHost(input);
  const isHostedBitrix24 = /(^|\.)bitrix24\.[a-z]{2,}(?:\.[a-z]{2,})?$/i.test(host);
  const isExplicitlyAllowed = allowedPortalDomains.some((allowed) => parsePortalHost(allowed) === host);
  if (!isHostedBitrix24 && !isExplicitlyAllowed) {
    throw new AppError("Only hosted or explicitly configured Bitrix24 portal domains are allowed", "INVALID_PORTAL_DOMAIN");
  }
  return host;
}

export function validateClientEndpoint(
  value: string,
  expectedDomain?: string,
  allowedPortalDomains: readonly string[] = [],
): { domain: string; endpoint: string } {
  try {
    const url = new URL(value);
    const domain = normalizePortalDomain(url.hostname, allowedPortalDomains);
    const expected = expectedDomain ? normalizePortalDomain(expectedDomain, allowedPortalDomains) : undefined;
    if (
      url.protocol === "https:" && !url.username && !url.password && !url.port &&
      url.pathname.startsWith("/rest/") && !url.search && !url.hash &&
      (!expected || domain === expected)
    ) {
      return { domain, endpoint: url.toString().replace(/\/$/, "") + "/" };
    }
  } catch {
    // Fall through to the validation error.
  }
  throw new AuthenticationError("Bitrix24 returned a REST endpoint that does not match the authorized portal");
}

export class BitrixOAuthService {
  constructor(
    private readonly config: AppConfig,
    private readonly states: OAuthStateRepository,
    private readonly connections: ConnectionRepository,
    private readonly cipher: TokenCipher,
  ) {}

  async createAuthorizationUrl(
    domainInput: string,
    requestedMode?: ConnectionMode,
    mcpRequestHash?: string,
  ): Promise<string> {
    const domain = normalizePortalDomain(domainInput, this.allowedPortalDomains);
    const mode = requestedMode ?? this.config.defaultMode;
    const state = randomBytes(32).toString("base64url");
    const expiresAt = new Date(Date.now() + this.config.oauthStateTtlSeconds * 1_000);
    await this.states.create(state, domain, mode, expiresAt, mcpRequestHash);

    const url = new URL(`https://${domain}/oauth/authorize/`);
    url.searchParams.set("client_id", this.config.bitrixClientId);
    url.searchParams.set("state", state);
    return url.toString();
  }

  async createMcpAuthorizationUrl(mcpRequestHash: string): Promise<string> {
    const domain = await this.resolveAuthorizationDomain();
    return this.createAuthorizationUrl(domain, this.config.defaultMode, mcpRequestHash);
  }

  async handleCallback(input: { code?: string; state?: string; domain?: string; member_id?: string; error?: string }): Promise<ConnectionRecord> {
    return (await this.handleCallbackWithContext(input)).connection;
  }

  async handleCallbackWithContext(
    input: { code?: string; state?: string; domain?: string; member_id?: string; error?: string },
  ): Promise<BitrixOAuthCallbackResult> {
    if (!input.state) throw new AuthenticationError("OAuth callback is missing state");
    const state = await this.states.consume(input.state);
    if (!state) throw new AuthenticationError("OAuth state is invalid, expired, or already used");
    try {
      if (input.error) throw new AuthenticationError("Bitrix24 authorization was denied");
      if (!input.code) throw new AuthenticationError("OAuth callback is missing the authorization code");
      if (input.domain && normalizePortalDomain(input.domain, this.allowedPortalDomains) !== state.domain) {
        throw new AuthenticationError("OAuth callback portal does not match the portal that initiated authorization");
      }

      const token = await this.exchangeToken({ grant_type: "authorization_code", code: input.code });
      if (input.member_id && input.member_id !== token.member_id) {
        throw new AuthenticationError("OAuth token portal identifier does not match the callback portal identifier");
      }
      const portal = validateClientEndpoint(token.client_endpoint, state.domain, this.allowedPortalDomains);
      const bitrixUserId = await this.getProfileUserId(token.access_token, portal.endpoint);
      if (token.user_id && token.user_id !== bitrixUserId) {
        throw new AuthenticationError("Bitrix24 token profile does not match the token response user");
      }

      const connection = await this.connections.upsert({
        domain: portal.domain,
        memberId: token.member_id,
        bitrixUserId,
        clientEndpoint: portal.endpoint,
        accessTokenEncrypted: this.cipher.encrypt(token.access_token),
        refreshTokenEncrypted: this.cipher.encrypt(token.refresh_token),
        scope: Array.isArray(token.scope) ? token.scope.join(",") : token.scope,
        expiresAt: new Date(Date.now() + token.expires_in * 1_000),
        mode: state.mode,
        adminToolsEnabled: this.config.adminToolsEnabled,
      });
      return { connection, mcpRequestHash: state.mcpRequestHash };
    } catch (error) {
      if (state.mcpRequestHash) {
        throw new BitrixOAuthContinuationError(
          input.error ? "Bitrix24 authorization was denied" : safeErrorMessage(error),
          state.mcpRequestHash,
          input.error ? "access_denied" : "server_error",
          { cause: error },
        );
      }
      throw error;
    }
  }

  async refresh(refreshToken: string): Promise<OAuthTokenResponse> {
    return this.exchangeToken({ grant_type: "refresh_token", refresh_token: refreshToken });
  }

  async handleInstallation(input: unknown): Promise<ConnectionRecord> {
    const normalizedInput = this.normalizeInstallationPayload(input);
    const auth = installationAuthSchema.parse(normalizedInput);
    const expectedDomain = normalizePortalDomain(auth.domain, this.allowedPortalDomains);
    const portal = validateClientEndpoint(auth.client_endpoint, expectedDomain, this.allowedPortalDomains);
    const appInfoUrl = new URL("app.info.json", portal.endpoint);
    let response: Response;
    try {
      response = await fetch(appInfoUrl, {
        method: "POST",
        headers: { "content-type": "application/json", accept: "application/json" },
        body: JSON.stringify({ auth: auth.access_token }),
        signal: AbortSignal.timeout(this.config.bitrixRequestTimeoutMs),
      });
    } catch {
      throw new AuthenticationError("Could not validate the Bitrix24 installation callback");
    }
    const parsed = appInfoSchema.safeParse(await response.json().catch(() => null));
    if (!response.ok || !parsed.success) {
      throw new AuthenticationError("Bitrix24 installation token validation failed");
    }
    if (!this.config.bitrixClientId.startsWith("pending-") && parsed.data.result.CODE !== this.config.bitrixClientId) {
      throw new AuthenticationError("Installation token belongs to a different Bitrix24 application");
    }

    const bitrixUserId = await this.getProfileUserId(auth.access_token, portal.endpoint);
    if (auth.user_id && auth.user_id !== bitrixUserId) {
      throw new AuthenticationError("Installation token profile does not match the supplied user");
    }

    return this.connections.upsert({
      domain: portal.domain,
      memberId: auth.member_id,
      bitrixUserId,
      clientEndpoint: portal.endpoint,
      accessTokenEncrypted: this.cipher.encrypt(auth.access_token),
      refreshTokenEncrypted: this.cipher.encrypt(auth.refresh_token),
      scope: auth.scope,
      expiresAt: new Date(Date.now() + auth.expires_in * 1_000),
      mode: this.config.defaultMode,
      adminToolsEnabled: this.config.adminToolsEnabled,
    });
  }

  private normalizeInstallationPayload(input: unknown): Record<string, unknown> {
    const root = typeof input === "object" && input !== null ? (input as Record<string, unknown>) : {};
    const nestedAuth = typeof root.auth === "object" && root.auth !== null ? (root.auth as Record<string, unknown>) : {};
    const query = typeof root.query === "object" && root.query !== null ? (root.query as Record<string, unknown>) : {};

    const pick = (...keys: string[]): unknown => {
      for (const key of keys) {
        if (nestedAuth[key] !== undefined && nestedAuth[key] !== "") return nestedAuth[key];
        if (root[key] !== undefined && root[key] !== "") return root[key];
        if (query[key] !== undefined && query[key] !== "") return query[key];
      }
      return undefined;
    };

    const rawDomain = pick("domain", "DOMAIN") ?? this.config.bitrixPortalDomain;
    let normalizedDomain: string | undefined;
    if (typeof rawDomain === "string" && rawDomain.trim()) {
      try {
        normalizedDomain = parsePortalHost(rawDomain);
      } catch {
        normalizedDomain = rawDomain;
      }
    }
    const clientEndpoint = pick("client_endpoint", "CLIENT_ENDPOINT")
      ?? (normalizedDomain ? `https://${normalizedDomain}/rest/` : undefined);

    return {
      access_token: pick("access_token", "AUTH_ID"),
      refresh_token: pick("refresh_token", "REFRESH_ID"),
      expires_in: pick("expires_in", "AUTH_EXPIRES", "expires") ?? 3_600,
      scope: pick("scope", "SCOPE") ?? "",
      domain: normalizedDomain,
      member_id: pick("member_id", "MEMBER_ID") ?? normalizedDomain,
      user_id: pick("user_id", "USER_ID"),
      client_endpoint: clientEndpoint,
    };
  }

  private async resolveAuthorizationDomain(): Promise<string> {
    if (this.config.bitrixPortalDomain) {
      return normalizePortalDomain(this.config.bitrixPortalDomain, this.allowedPortalDomains);
    }
    const domains = [...new Set(
      (await this.connections.list())
        .filter((connection) => connection.status === "active")
        .map((connection) => connection.domain),
    )];
    if (domains.length === 1) return normalizePortalDomain(domains[0]!, this.allowedPortalDomains);
    throw new AppError(
      "Install the local Bitrix24 application first or configure BITRIX_PORTAL_DOMAIN",
      "BITRIX_PORTAL_NOT_CONFIGURED",
      503,
    );
  }

  private get allowedPortalDomains(): readonly string[] {
    return this.config.bitrixPortalDomain ? [this.config.bitrixPortalDomain] : [];
  }

  private async getProfileUserId(accessToken: string, clientEndpoint: string): Promise<string> {
    const url = new URL("profile.json", clientEndpoint);
    let response: Response;
    try {
      response = await fetch(url, {
        method: "POST",
        headers: { "content-type": "application/json", accept: "application/json" },
        body: JSON.stringify({ auth: accessToken }),
        signal: AbortSignal.timeout(this.config.bitrixRequestTimeoutMs),
      });
    } catch {
      throw new AuthenticationError("Could not verify the authorized Bitrix24 profile");
    }
    const parsed = profileSchema.safeParse(await response.json().catch(() => null));
    if (!response.ok || !parsed.success) {
      throw new AuthenticationError("Bitrix24 profile verification failed");
    }
    return parsed.data.result.ID;
  }

  private async exchangeToken(parameters: Record<string, string>): Promise<OAuthTokenResponse> {
    const url = new URL("https://oauth.bitrix.info/oauth/token/");
    const body = new URLSearchParams({
      ...parameters,
      client_id: this.config.bitrixClientId,
      client_secret: this.config.bitrixClientSecret,
    });
    url.search = body.toString();
    let response: Response;
    try {
      response = await fetch(url, {
        method: "GET",
        headers: { accept: "application/json" },
        signal: AbortSignal.timeout(this.config.bitrixRequestTimeoutMs),
      });
    } catch {
      throw new BitrixOAuthError("Could not reach the Bitrix24 OAuth service", "network_error");
    }
    const payload = await response.json().catch(() => null) as Record<string, unknown> | null;
    if (!response.ok || payload?.error) {
      const code = typeof payload?.error === "string" ? payload.error : `http_${response.status}`;
      throw new BitrixOAuthError(`Bitrix24 OAuth request failed (${code})`, code);
    }
    const parsed = oauthTokenSchema.safeParse(payload);
    if (!parsed.success) throw new BitrixOAuthError("Bitrix24 returned an invalid OAuth token response", "invalid_response");
    return parsed.data;
  }
}
