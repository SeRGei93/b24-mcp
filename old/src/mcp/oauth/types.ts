export const MCP_OAUTH_SCOPE = "mcp:tools";

export interface McpOAuthClient {
  clientId: string;
  redirectUris: string[];
  tokenEndpointAuthMethod: "none";
  grantTypes: string[];
  responseTypes: string[];
  clientName?: string;
  clientUri?: string;
  logoUri?: string;
  scope: string;
  softwareId?: string;
  softwareVersion?: string;
  createdAt: Date;
}

export interface McpOAuthAuthorizationRequest {
  requestHash: string;
  clientId: string;
  redirectUri: string;
  state?: string;
  codeChallenge: string;
  resource: string;
  scope: string;
  expiresAt: Date;
  attempts: number;
}

export interface McpOAuthAuthorizationCode {
  codeHash: string;
  clientId: string;
  connectionId: string;
  redirectUri: string;
  codeChallenge: string;
  resource: string;
  scope: string;
  expiresAt: Date;
}

export interface McpOAuthTokenRecord {
  id: string;
  accessTokenHash: string;
  refreshTokenHash: string;
  clientId: string;
  connectionId: string;
  resource: string;
  scope: string;
  accessExpiresAt: Date;
  refreshExpiresAt: Date;
  revokedAt: Date | null;
  createdAt: Date;
}

export interface NewMcpOAuthTokenRecord {
  accessTokenHash: string;
  refreshTokenHash: string;
  clientId: string;
  connectionId: string;
  resource: string;
  scope: string;
  accessExpiresAt: Date;
  refreshExpiresAt: Date;
}

export interface McpOAuthStore {
  registerClient(client: McpOAuthClient): Promise<void>;
  getClient(clientId: string): Promise<McpOAuthClient | null>;
  createAuthorizationRequest(request: McpOAuthAuthorizationRequest): Promise<void>;
  getAuthorizationRequest(requestHash: string): Promise<McpOAuthAuthorizationRequest | null>;
  incrementAuthorizationAttempts(requestHash: string): Promise<number>;
  consumeAuthorizationRequestAndCreateCode(
    requestHash: string,
    codeHash: string,
    expiresAt: Date,
    connectionId: string,
  ): Promise<McpOAuthAuthorizationRequest | null>;
  consumeAuthorizationRequest(requestHash: string): Promise<McpOAuthAuthorizationRequest | null>;
  consumeAuthorizationCode(codeHash: string): Promise<McpOAuthAuthorizationCode | null>;
  issueTokenPair(token: NewMcpOAuthTokenRecord): Promise<void>;
  rotateRefreshToken(
    refreshTokenHash: string,
    clientId: string,
    resource: string,
    replacement: NewMcpOAuthTokenRecord,
  ): Promise<McpOAuthTokenRecord | null>;
  findAccessToken(accessTokenHash: string): Promise<McpOAuthTokenRecord | null>;
  findRefreshToken(
    refreshTokenHash: string,
    clientId: string,
    resource: string,
  ): Promise<McpOAuthTokenRecord | null>;
  revokeToken(tokenHash: string): Promise<void>;
}

export interface McpOAuthAccessInfo {
  clientId: string;
  connectionId?: string;
  scopes: string[];
  resource: string;
  expiresAt?: number;
  legacyStaticToken?: boolean;
}

export interface McpOAuthTokens {
  access_token: string;
  token_type: "Bearer";
  expires_in: number;
  refresh_token: string;
  scope: string;
}

export interface McpOAuthClientRegistrationResponse {
  client_id: string;
  client_id_issued_at: number;
  redirect_uris: string[];
  token_endpoint_auth_method: "none";
  grant_types: string[];
  response_types: string[];
  client_name?: string;
  client_uri?: string;
  logo_uri?: string;
  scope: string;
  software_id?: string;
  software_version?: string;
}
