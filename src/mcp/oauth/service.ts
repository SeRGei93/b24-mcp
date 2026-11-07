import { createHash, randomBytes } from "node:crypto";
import type { AppConfig } from "../../config/env.js";
import { isValidSecret } from "../../security/auth.js";
import {
  MCP_OAUTH_SCOPE,
  type McpOAuthAccessInfo,
  type McpOAuthAuthorizationRequest,
  type McpOAuthClient,
  type McpOAuthClientRegistrationResponse,
  type McpOAuthStore,
  type McpOAuthTokens,
  type NewMcpOAuthTokenRecord,
} from "./types.js";

const LOOPBACK_HOSTS = new Set(["localhost", "127.0.0.1", "[::1]"]);
const PKCE_VERIFIER_PATTERN = /^[A-Za-z0-9._~-]{43,128}$/;
const PKCE_CHALLENGE_PATTERN = /^[A-Za-z0-9_-]{43}$/;

export class McpOAuthProtocolError extends Error {
  constructor(
    readonly errorCode: string,
    message: string,
    readonly statusCode = 400,
    readonly safeRedirectUri?: string,
    readonly state?: string,
  ) {
    super(message);
    this.name = "McpOAuthProtocolError";
  }
}

export interface ClientRegistrationInput {
  redirectUris: string[];
  tokenEndpointAuthMethod?: string;
  grantTypes?: string[];
  responseTypes?: string[];
  clientName?: string;
  clientUri?: string;
  logoUri?: string;
  scope?: string;
  softwareId?: string;
  softwareVersion?: string;
}

export interface AuthorizationInput {
  responseType: string;
  clientId: string;
  redirectUri?: string;
  codeChallenge: string;
  codeChallengeMethod: string;
  state?: string;
  scope?: string;
  resource?: string;
}

export interface PendingAuthorizationInfo {
  requestHash: string;
}

export interface AuthorizationCodeExchangeInput {
  clientId: string;
  code: string;
  codeVerifier: string;
  redirectUri?: string;
  resource?: string;
}

export interface RefreshTokenExchangeInput {
  clientId: string;
  refreshToken: string;
  scope?: string;
  resource?: string;
}

export function hashMcpOAuthSecret(value: string): string {
  return createHash("sha256").update(value).digest("hex");
}

export function redirectUriMatches(requested: string, registered: string): boolean {
  if (requested === registered) return true;
  try {
    const requestedUrl = new URL(requested);
    const registeredUrl = new URL(registered);
    if (!LOOPBACK_HOSTS.has(requestedUrl.hostname) || !LOOPBACK_HOSTS.has(registeredUrl.hostname)) return false;
    return requestedUrl.protocol === registeredUrl.protocol
      && requestedUrl.hostname === registeredUrl.hostname
      && requestedUrl.pathname === registeredUrl.pathname
      && requestedUrl.search === registeredUrl.search;
  } catch {
    return false;
  }
}

export function isSafeOAuthRedirectUri(value: string): boolean {
  try {
    const url = new URL(value);
    if (url.hash || url.username || url.password) return false;
    if (url.protocol === "https:") return true;
    return url.protocol === "http:" && LOOPBACK_HOSTS.has(url.hostname);
  } catch {
    return false;
  }
}

export class McpOAuthService {
  readonly issuerUrl: string;
  readonly resourceUrl: string;
  readonly protectedResourceMetadataUrl: string;

  constructor(
    private readonly config: AppConfig,
    private readonly store: McpOAuthStore,
  ) {
    this.issuerUrl = config.appUrl;
    this.resourceUrl = new URL("/mcp", config.appUrl).toString();
    this.protectedResourceMetadataUrl = new URL(
      "/.well-known/oauth-protected-resource/mcp",
      config.appUrl,
    ).toString();
  }

  protectedResourceMetadata() {
    return {
      resource: this.resourceUrl,
      authorization_servers: [this.issuerUrl],
      scopes_supported: [MCP_OAUTH_SCOPE],
      bearer_methods_supported: ["header"],
      resource_name: "Bitrix24 MCP Server",
      resource_documentation: new URL("/", this.config.appUrl).toString(),
    };
  }

  authorizationServerMetadata() {
    return {
      issuer: this.issuerUrl,
      authorization_endpoint: new URL("/authorize", this.config.appUrl).toString(),
      token_endpoint: new URL("/token", this.config.appUrl).toString(),
      registration_endpoint: new URL("/register", this.config.appUrl).toString(),
      revocation_endpoint: new URL("/revoke", this.config.appUrl).toString(),
      response_types_supported: ["code"],
      grant_types_supported: ["authorization_code", "refresh_token"],
      token_endpoint_auth_methods_supported: ["none"],
      revocation_endpoint_auth_methods_supported: ["none"],
      code_challenge_methods_supported: ["S256"],
      scopes_supported: [MCP_OAUTH_SCOPE],
      service_documentation: new URL("/", this.config.appUrl).toString(),
    };
  }

  challenge(error?: "invalid_token" | "insufficient_scope"): string {
    const parts = [
      `Bearer resource_metadata="${this.protectedResourceMetadataUrl}"`,
      `scope="${MCP_OAUTH_SCOPE}"`,
    ];
    if (error) parts.push(`error="${error}"`);
    return parts.join(", ");
  }

  async registerClient(input: ClientRegistrationInput): Promise<McpOAuthClientRegistrationResponse> {
    if (!input.redirectUris.length || input.redirectUris.length > 10) {
      throw new McpOAuthProtocolError("invalid_client_metadata", "Provide between one and ten redirect URIs");
    }
    if (input.redirectUris.some((uri) => !isSafeOAuthRedirectUri(uri))) {
      throw new McpOAuthProtocolError(
        "invalid_redirect_uri",
        "Redirect URIs must use HTTPS, except for HTTP loopback callbacks",
      );
    }
    const tokenMethod = input.tokenEndpointAuthMethod ?? "none";
    if (tokenMethod !== "none") {
      throw new McpOAuthProtocolError("invalid_client_metadata", "Only public PKCE clients are supported");
    }
    const grantTypes = input.grantTypes ?? ["authorization_code", "refresh_token"];
    if (!grantTypes.includes("authorization_code") || grantTypes.some((grant) => !["authorization_code", "refresh_token"].includes(grant))) {
      throw new McpOAuthProtocolError("invalid_client_metadata", "Unsupported OAuth grant type");
    }
    const responseTypes = input.responseTypes ?? ["code"];
    if (!responseTypes.includes("code") || responseTypes.some((response) => response !== "code")) {
      throw new McpOAuthProtocolError("invalid_client_metadata", "Unsupported OAuth response type");
    }
    if (input.scope && !parseScope(input.scope).includes(MCP_OAUTH_SCOPE)) {
      throw new McpOAuthProtocolError("invalid_client_metadata", `Client scope must include ${MCP_OAUTH_SCOPE}`);
    }

    const createdAt = new Date();
    const client: McpOAuthClient = {
      clientId: `mcp_${randomToken(24)}`,
      redirectUris: [...new Set(input.redirectUris)],
      tokenEndpointAuthMethod: "none",
      grantTypes,
      responseTypes,
      clientName: input.clientName?.slice(0, 200),
      clientUri: input.clientUri,
      logoUri: input.logoUri,
      scope: MCP_OAUTH_SCOPE,
      softwareId: input.softwareId?.slice(0, 200),
      softwareVersion: input.softwareVersion?.slice(0, 100),
      createdAt,
    };
    await this.store.registerClient(client);
    return toRegistrationResponse(client);
  }

  async beginAuthorization(input: AuthorizationInput): Promise<PendingAuthorizationInfo> {
    const client = await this.store.getClient(input.clientId);
    if (!client) throw new McpOAuthProtocolError("invalid_client", "Unknown OAuth client");

    const redirectUri = input.redirectUri ?? (client.redirectUris.length === 1 ? client.redirectUris[0] : undefined);
    if (!redirectUri || !client.redirectUris.some((registered) => redirectUriMatches(redirectUri, registered))) {
      throw new McpOAuthProtocolError("invalid_request", "Unregistered redirect_uri");
    }
    const fail = (code: string, message: string): never => {
      throw new McpOAuthProtocolError(code, message, 400, redirectUri, input.state);
    };
    if (input.responseType !== "code") fail("unsupported_response_type", "Only response_type=code is supported");
    if (input.codeChallengeMethod !== "S256" || !PKCE_CHALLENGE_PATTERN.test(input.codeChallenge)) {
      fail("invalid_request", "A valid S256 PKCE code challenge is required");
    }
    const resource = input.resource ?? this.resourceUrl;
    if (resource !== this.resourceUrl) fail("invalid_target", "The OAuth resource does not match this MCP server");
    const scopes = parseScope(input.scope ?? MCP_OAUTH_SCOPE);
    if (!scopes.length || scopes.some((scope) => scope !== MCP_OAUTH_SCOPE)) {
      fail("invalid_scope", `Only ${MCP_OAUTH_SCOPE} is supported`);
    }

    const requestId = randomToken(32);
    const request: McpOAuthAuthorizationRequest = {
      requestHash: hashMcpOAuthSecret(requestId),
      clientId: client.clientId,
      redirectUri,
      state: input.state,
      codeChallenge: input.codeChallenge,
      resource,
      scope: MCP_OAUTH_SCOPE,
      expiresAt: new Date(Date.now() + this.config.mcpOAuthRequestTtlSeconds * 1_000),
      attempts: 0,
    };
    await this.store.createAuthorizationRequest(request);
    return { requestHash: request.requestHash };
  }

  async completeBitrixAuthorization(requestHash: string, connectionId: string): Promise<string> {
    const rawCode = randomToken(32);
    const consumed = await this.store.consumeAuthorizationRequestAndCreateCode(
      requestHash,
      hashMcpOAuthSecret(rawCode),
      new Date(Date.now() + this.config.mcpOAuthCodeTtlSeconds * 1_000),
      connectionId,
    );
    if (!consumed) throw new McpOAuthProtocolError("invalid_request", "Authorization request was already used", 400);
    const redirect = new URL(consumed.redirectUri);
    redirect.searchParams.set("code", rawCode);
    if (consumed.state) redirect.searchParams.set("state", consumed.state);
    return redirect.toString();
  }

  async failBitrixAuthorization(
    requestHash: string,
    errorCode = "access_denied",
    description = "Bitrix24 authorization was not completed",
  ): Promise<string> {
    const request = await this.store.consumeAuthorizationRequest(requestHash);
    if (!request) throw new McpOAuthProtocolError("invalid_request", "Authorization request expired or invalid", 400);
    const redirect = new URL(request.redirectUri);
    redirect.searchParams.set("error", errorCode);
    redirect.searchParams.set("error_description", description);
    if (request.state) redirect.searchParams.set("state", request.state);
    return redirect.toString();
  }

  async exchangeAuthorizationCode(input: AuthorizationCodeExchangeInput): Promise<McpOAuthTokens> {
    const client = await this.store.getClient(input.clientId);
    if (!client) throw new McpOAuthProtocolError("invalid_client", "Unknown OAuth client", 401);
    const code = await this.store.consumeAuthorizationCode(hashMcpOAuthSecret(input.code));
    if (!code || code.clientId !== client.clientId) {
      throw new McpOAuthProtocolError("invalid_grant", "Authorization code is invalid or expired");
    }
    if (input.redirectUri && !redirectUriMatches(input.redirectUri, code.redirectUri)) {
      throw new McpOAuthProtocolError("invalid_grant", "redirect_uri does not match the authorization request");
    }
    if (input.resource && input.resource !== code.resource) {
      throw new McpOAuthProtocolError("invalid_target", "The resource does not match the authorization request");
    }
    if (!verifyPkce(input.codeVerifier, code.codeChallenge)) {
      throw new McpOAuthProtocolError("invalid_grant", "PKCE code_verifier does not match");
    }
    return this.issueTokens(client.clientId, code.connectionId, code.resource, code.scope);
  }

  async exchangeRefreshToken(input: RefreshTokenExchangeInput): Promise<McpOAuthTokens> {
    const client = await this.store.getClient(input.clientId);
    if (!client) throw new McpOAuthProtocolError("invalid_client", "Unknown OAuth client", 401);
    const resource = input.resource ?? this.resourceUrl;
    if (resource !== this.resourceUrl) throw new McpOAuthProtocolError("invalid_target", "Invalid MCP resource");
    if (input.scope && parseScope(input.scope).some((scope) => scope !== MCP_OAUTH_SCOPE)) {
      throw new McpOAuthProtocolError("invalid_scope", `Only ${MCP_OAUTH_SCOPE} is supported`);
    }

    const refreshTokenHash = hashMcpOAuthSecret(input.refreshToken);
    const current = await this.store.findRefreshToken(refreshTokenHash, client.clientId, resource);
    if (!current) throw new McpOAuthProtocolError("invalid_grant", "Refresh token is invalid, expired, or already used");

    const accessToken = randomToken(32);
    const refreshToken = randomToken(48);
    const replacement = this.newTokenRecord(
      accessToken,
      refreshToken,
      client.clientId,
      current.connectionId,
      resource,
      MCP_OAUTH_SCOPE,
    );
    const previous = await this.store.rotateRefreshToken(
      refreshTokenHash,
      client.clientId,
      resource,
      replacement,
    );
    if (!previous) throw new McpOAuthProtocolError("invalid_grant", "Refresh token is invalid, expired, or already used");
    return this.tokenResponse(accessToken, refreshToken, replacement.scope);
  }

  async verifyAuthorizationHeader(authorization: string | undefined): Promise<McpOAuthAccessInfo | null> {
    if (!authorization?.startsWith("Bearer ")) return null;
    const token = authorization.slice(7);
    if (!token) return null;
    if (isValidSecret(token, this.config.mcpAuthToken)) {
      return {
        clientId: "legacy-static-token",
        scopes: [MCP_OAUTH_SCOPE],
        resource: this.resourceUrl,
        legacyStaticToken: true,
      };
    }
    const record = await this.store.findAccessToken(hashMcpOAuthSecret(token));
    if (!record || record.resource !== this.resourceUrl) return null;
    const scopes = parseScope(record.scope);
    if (!scopes.includes(MCP_OAUTH_SCOPE)) return null;
    return {
      clientId: record.clientId,
      connectionId: record.connectionId,
      scopes,
      resource: record.resource,
      expiresAt: Math.floor(record.accessExpiresAt.getTime() / 1_000),
    };
  }

  async revokeToken(token: string): Promise<void> {
    await this.store.revokeToken(hashMcpOAuthSecret(token));
  }

  private async issueTokens(
    clientId: string,
    connectionId: string,
    resource: string,
    scope: string,
  ): Promise<McpOAuthTokens> {
    const accessToken = randomToken(32);
    const refreshToken = randomToken(48);
    const record = this.newTokenRecord(accessToken, refreshToken, clientId, connectionId, resource, scope);
    await this.store.issueTokenPair(record);
    return this.tokenResponse(accessToken, refreshToken, scope);
  }

  private newTokenRecord(
    accessToken: string,
    refreshToken: string,
    clientId: string,
    connectionId: string,
    resource: string,
    scope: string,
  ): NewMcpOAuthTokenRecord {
    return {
      accessTokenHash: hashMcpOAuthSecret(accessToken),
      refreshTokenHash: hashMcpOAuthSecret(refreshToken),
      clientId,
      connectionId,
      resource,
      scope,
      accessExpiresAt: new Date(Date.now() + this.config.mcpOAuthAccessTokenTtlSeconds * 1_000),
      refreshExpiresAt: new Date(Date.now() + this.config.mcpOAuthRefreshTokenTtlSeconds * 1_000),
    };
  }

  private tokenResponse(accessToken: string, refreshToken: string, scope: string): McpOAuthTokens {
    return {
      access_token: accessToken,
      token_type: "Bearer",
      expires_in: this.config.mcpOAuthAccessTokenTtlSeconds,
      refresh_token: refreshToken,
      scope,
    };
  }
}

function parseScope(scope: string): string[] {
  return [...new Set(scope.split(/\s+/).filter(Boolean))];
}

function randomToken(bytes: number): string {
  return randomBytes(bytes).toString("base64url");
}

function verifyPkce(verifier: string, expectedChallenge: string): boolean {
  if (!PKCE_VERIFIER_PATTERN.test(verifier)) return false;
  const actual = createHash("sha256").update(verifier).digest("base64url");
  return isValidSecret(actual, expectedChallenge);
}

function toRegistrationResponse(client: McpOAuthClient): McpOAuthClientRegistrationResponse {
  return {
    client_id: client.clientId,
    client_id_issued_at: Math.floor(client.createdAt.getTime() / 1_000),
    redirect_uris: client.redirectUris,
    token_endpoint_auth_method: client.tokenEndpointAuthMethod,
    grant_types: client.grantTypes,
    response_types: client.responseTypes,
    client_name: client.clientName,
    client_uri: client.clientUri,
    logo_uri: client.logoUri,
    scope: client.scope,
    software_id: client.softwareId,
    software_version: client.softwareVersion,
  };
}
