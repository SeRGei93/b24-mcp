import { afterEach, describe, expect, it, vi } from "vitest";
import type { AppConfig } from "../src/config/env.js";
import type { TokenManager } from "../src/bitrix/token-manager.js";
import { BitrixRestClient, buildBatchCommand } from "../src/bitrix/rest-client.js";
import { CrmService } from "../src/bitrix/crm.js";

const config = {
  bitrixMaxRetries: 2,
  bitrixRequestTimeoutMs: 1_000,
  bitrixMaxPages: 20,
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

  it("serializes nested parameters for Bitrix24 batch commands and executes batch calls", async () => {
    const cmdString = buildBatchCommand("crm.item.list", {
      entityTypeId: 2,
      filter: { stageId: "NEW" },
      select: ["id", "title"],
      start: 50,
    });
    expect(decodeURIComponent(cmdString)).toBe(
      "crm.item.list?entityTypeId=2&filter[stageId]=NEW&select[0]=id&select[1]=title&start=50",
    );

    const get = vi.fn().mockResolvedValue({
      connection: { clientEndpoint: "https://example.bitrix24.com/rest/" },
      accessToken: "token",
    });
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({
        result: { items: [{ id: 1 }, { id: 2 }] },
        next: 50,
        total: 120,
      }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({
        result: {
          result: {
            p_50: { items: [{ id: 3 }, { id: 4 }] },
            p_100: { items: [{ id: 5 }] },
          },
          result_error: [],
        },
      }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    const client = new BitrixRestClient(config, { get } as unknown as TokenManager, logger);
    const crm = new CrmService(config, client);
    const searchResult = await crm.searchItems({
      connectionId: "connection",
      entityTypeId: 2,
      limit: 200,
    });

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1]![0]).toBe("https://example.bitrix24.com/rest/batch.json");
    const batchBody = JSON.parse(fetchMock.mock.calls[1]![1]!.body as string);
    expect(Object.keys(batchBody.cmd)).toEqual(["p_50", "p_100"]);
    expect(searchResult.items).toEqual([{ id: 1 }, { id: 2 }, { id: 3 }, { id: 4 }, { id: 5 }]);
    expect(searchResult.total).toBe(120);
  });
});
