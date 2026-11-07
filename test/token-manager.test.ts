import { randomBytes } from "node:crypto";
import { describe, expect, it, vi } from "vitest";
import type { ConnectionRecord } from "../src/types.js";
import type { ConnectionRepository } from "../src/db/repositories.js";
import type { BitrixOAuthService } from "../src/bitrix/oauth.js";
import { TokenCipher } from "../src/security/encryption.js";
import { TokenManager } from "../src/bitrix/token-manager.js";

const logger = { debug: vi.fn(), info: vi.fn(), warn: vi.fn(), error: vi.fn() };

describe("TokenManager", () => {
  it("deduplicates cross-instance refresh after the stale token has been replaced", async () => {
    const cipher = new TokenCipher(randomBytes(32));
    let current: ConnectionRecord = {
      id: "00000000-0000-4000-8000-000000000001",
      domain: "crm.hub-bnk.com",
      memberId: "member",
      bitrixUserId: "1",
      clientEndpoint: "https://crm.hub-bnk.com/rest/",
      accessTokenEncrypted: cipher.encrypt("old-access"),
      refreshTokenEncrypted: cipher.encrypt("old-refresh"),
      scope: "crm",
      expiresAt: new Date(Date.now() + 3_600_000),
      mode: "read_only",
      adminToolsEnabled: false,
      status: "active",
      createdAt: new Date(),
      updatedAt: new Date(),
    };

    let lock = Promise.resolve();
    const repository = {
      get: vi.fn(async () => current),
      withRefreshLock: vi.fn(async (_id: string, callback: (value: ConnectionRecord, client: object) => Promise<unknown>) => {
        const previous = lock;
        let release!: () => void;
        lock = new Promise<void>((resolve) => { release = resolve; });
        await previous;
        try {
          return await callback(current, {});
        } finally {
          release();
        }
      }),
      updateTokens: vi.fn(async (_client: object, _id: string, tokens: Partial<ConnectionRecord>) => {
        current = { ...current, ...tokens, updatedAt: new Date() };
        return current;
      }),
      markRevoked: vi.fn(),
    } as unknown as ConnectionRepository;
    const oauth = {
      refresh: vi.fn(async () => ({
        access_token: "new-access",
        refresh_token: "new-refresh",
        expires_in: 3_600,
        scope: "crm",
        domain: "oauth.bitrix.info",
        member_id: current.memberId,
        user_id: current.bitrixUserId,
        client_endpoint: current.clientEndpoint,
      })),
    } as unknown as BitrixOAuthService;

    const first = new TokenManager(repository, oauth, cipher, logger);
    const second = new TokenManager(repository, oauth, cipher, logger);
    const [a, b] = await Promise.all([
      first.get(current.id, true, "old-access"),
      second.get(current.id, true, "old-access"),
    ]);

    expect(a.accessToken).toBe("new-access");
    expect(b.accessToken).toBe("new-access");
    expect(oauth.refresh).toHaveBeenCalledTimes(1);
  });
});
