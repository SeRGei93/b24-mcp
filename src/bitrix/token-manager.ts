import type { AppLogger, ConnectionRecord } from "../types.js";
import type { ConnectionRepository } from "../db/repositories.js";
import { PolicyError } from "../errors.js";
import { TokenCipher } from "../security/encryption.js";
import { BitrixOAuthError, BitrixOAuthService, validateClientEndpoint } from "./oauth.js";

export interface ConnectionWithToken {
  connection: ConnectionRecord;
  accessToken: string;
}

export class TokenManager {
  readonly #inflight = new Map<string, Promise<ConnectionWithToken>>();

  constructor(
    private readonly connections: ConnectionRepository,
    private readonly oauth: BitrixOAuthService,
    private readonly cipher: TokenCipher,
    private readonly logger: AppLogger,
  ) {}

  async get(connectionId: string, forceRefresh = false, staleAccessToken?: string): Promise<ConnectionWithToken> {
    const connection = await this.connections.get(connectionId);
    if (connection.status !== "active") throw new PolicyError(`Connection '${connectionId}' is ${connection.status}`);
    if (forceRefresh && staleAccessToken && this.cipher.decrypt(connection.accessTokenEncrypted) !== staleAccessToken) {
      return { connection, accessToken: this.cipher.decrypt(connection.accessTokenEncrypted) };
    }
    if (!forceRefresh && connection.expiresAt.getTime() > Date.now() + 60_000) {
      return { connection, accessToken: this.cipher.decrypt(connection.accessTokenEncrypted) };
    }

    const existing = this.#inflight.get(connectionId);
    if (existing) return existing;
    const refresh = this.refreshLocked(connectionId, forceRefresh, staleAccessToken).finally(() => this.#inflight.delete(connectionId));
    this.#inflight.set(connectionId, refresh);
    return refresh;
  }

  private async refreshLocked(connectionId: string, forceRefresh: boolean, staleAccessToken?: string): Promise<ConnectionWithToken> {
    try {
      return await this.connections.withRefreshLock(connectionId, async (current, client) => {
        if (forceRefresh && staleAccessToken && this.cipher.decrypt(current.accessTokenEncrypted) !== staleAccessToken) {
          return { connection: current, accessToken: this.cipher.decrypt(current.accessTokenEncrypted) };
        }
        if (!forceRefresh && current.expiresAt.getTime() > Date.now() + 60_000) {
          return { connection: current, accessToken: this.cipher.decrypt(current.accessTokenEncrypted) };
        }
        const response = await this.oauth.refresh(this.cipher.decrypt(current.refreshTokenEncrypted));
        if (response.member_id !== current.memberId) {
          throw new BitrixOAuthError("Refreshed token belongs to a different Bitrix24 portal", "portal_mismatch");
        }
        const portal = validateClientEndpoint(response.client_endpoint, undefined, [current.domain]);
        const updated = await this.connections.updateTokens(client, connectionId, {
          accessTokenEncrypted: this.cipher.encrypt(response.access_token),
          refreshTokenEncrypted: this.cipher.encrypt(response.refresh_token),
          expiresAt: new Date(Date.now() + response.expires_in * 1_000),
          scope: Array.isArray(response.scope) ? response.scope.join(",") : response.scope,
          domain: portal.domain,
          clientEndpoint: portal.endpoint,
          bitrixUserId: response.user_id ?? current.bitrixUserId,
        });
        this.logger.info({ connectionId }, "Bitrix24 access token refreshed");
        return { connection: updated, accessToken: response.access_token };
      });
    } catch (error) {
      if (error instanceof BitrixOAuthError && ["invalid_grant", "invalid_token", "expired_token"].includes(error.oauthCode)) {
        await this.connections.markRevoked(connectionId);
        this.logger.warn({ connectionId, oauthCode: error.oauthCode }, "Bitrix24 connection marked revoked");
      }
      throw error;
    }
  }
}
