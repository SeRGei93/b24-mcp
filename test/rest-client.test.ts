import { afterEach, describe, expect, it, vi } from "vitest";
import type { AppConfig } from "../src/config/env.js";
import type { TokenManager } from "../src/bitrix/token-manager.js";
import { BitrixRestClient } from "../src/bitrix/rest-client.js";

const config = {
  bitrixMaxRetries: 2,
  bitrixRequestTimeoutMs: 1_000,
} as AppConfig;
const logger = { debug: vi.fn(), info: vi.fn(), warn: vi.fn(), error: vi.fn() };

afterEach(() => vi.unstubAllGlobals());

describe("BitrixRestClient", () => {
  it("refreshes once after an expired token and does not expose it in the URL", async () => {
    const get = vi.fn()
      .mockResolvedValueOnce({
        connection: { clientEndpoint: "https://example.bitrix24.com/rest/" },
        accessToken: "old-secret",
      })
      .mockResolvedValueOnce({
        connection: { clientEndpoint: "https://example.bitrix24.com/rest/" },
        accessToken: "new-secret",
      });
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ error: "expired_token" }), { status: 401 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ result: { item: { id: 7 } } }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    const client = new BitrixRestClient(config, { get } as unknown as TokenManager, logger);
    const response = await client.call("connection", "crm.item.get", { entityTypeId: 2, id: 7 });

    expect(response.result).toEqual({ item: { id: 7 } });
    expect(get).toHaveBeenNthCalledWith(1, "connection", false, undefined);
    expect(get).toHaveBeenNthCalledWith(2, "connection", true, "old-secret");
    expect(fetchMock.mock.calls[0]![0]).toBe("https://example.bitrix24.com/rest/crm.item.get.json");
    expect(String(fetchMock.mock.calls[0]![0])).not.toContain("old-secret");
    expect(JSON.parse(fetchMock.mock.calls[1]![1]!.body as string).auth).toBe("new-secret");
  });

  it("rejects an invalid REST method before making a request", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const client = new BitrixRestClient(config, { get: vi.fn() } as unknown as TokenManager, logger);
    await expect(client.call("connection", "https://evil.example")).rejects.toThrow(/invalid/i);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
