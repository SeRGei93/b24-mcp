import type { AppConfig } from "../config/env.js";
import { BitrixRestClient, type BitrixBatchCommand } from "./rest-client.js";

const BITRIX_PAGE_SIZE = 50;

export class CrmService {
  constructor(private readonly config: AppConfig, private readonly bitrix: BitrixRestClient) {}

  async searchItems(input: {
    connectionId: string;
    entityTypeId: number;
    filter?: Record<string, unknown>;
    select?: string[];
    order?: Record<string, "ASC" | "DESC">;
    limit: number;
  }): Promise<{ items: unknown[]; total?: number; truncated: boolean }> {
    const baseParams = {
      entityTypeId: input.entityTypeId,
      filter: input.filter ?? {},
      select: input.select ?? ["*"],
      order: input.order ?? { id: "ASC" },
    };

    const firstResponse = await this.bitrix.call<{ items?: unknown[] }>(input.connectionId, "crm.item.list", {
      ...baseParams,
      start: 0,
    });
    const items: unknown[] = [...(firstResponse.result.items ?? [])];
    const total = firstResponse.total;

    if (firstResponse.next !== undefined && items.length < input.limit && this.config.bitrixMaxPages > 1) {
      if (total !== undefined && total > items.length) {
        const targetCount = Math.min(total, input.limit, this.config.bitrixMaxPages * BITRIX_PAGE_SIZE);
        const commands: Record<string, BitrixBatchCommand> = {};
        const keys: string[] = [];
        for (
          let start = firstResponse.next, page = 1;
          start < targetCount && page < this.config.bitrixMaxPages;
          start += BITRIX_PAGE_SIZE, page += 1
        ) {
          const key = `p_${start}`;
          keys.push(key);
          commands[key] = {
            method: "crm.item.list",
            params: { ...baseParams, start },
          };
        }
        if (keys.length > 0) {
          const batchResult = await this.bitrix.callBatch<{ items?: unknown[] }>(input.connectionId, commands);
          for (const key of keys) {
            items.push(...(batchResult[key]?.items ?? []));
          }
        }
      } else {
        let start = firstResponse.next;
        for (let page = 1; page < this.config.bitrixMaxPages && items.length < input.limit; page += 1) {
          const pageResponse = await this.bitrix.call<{ items?: unknown[] }>(input.connectionId, "crm.item.list", {
            ...baseParams,
            start,
          });
          items.push(...(pageResponse.result.items ?? []));
          if (pageResponse.next === undefined) break;
          start = pageResponse.next;
        }
      }
    }

    const sliced = items.slice(0, input.limit);
    return {
      items: sliced,
      ...(total === undefined ? {} : { total }),
      truncated: items.length > sliced.length || (total !== undefined && total > sliced.length),
    };
  }

  async getItem(connectionId: string, entityTypeId: number, id: number): Promise<unknown> {
    const response = await this.bitrix.call<{ item?: unknown }>(connectionId, "crm.item.get", { entityTypeId, id });
    return response.result.item ?? response.result;
  }

  async listUsers(connectionId: string, filter: Record<string, unknown> = {}): Promise<unknown[]> {
    const firstResponse = await this.bitrix.call<unknown[]>(connectionId, "user.get", { filter, start: 0 });
    const users: unknown[] = [...(Array.isArray(firstResponse.result) ? firstResponse.result : [])];
    const total = firstResponse.total;

    if (firstResponse.next !== undefined && this.config.bitrixMaxPages > 1) {
      if (total !== undefined && total > users.length) {
        const targetCount = Math.min(total, this.config.bitrixMaxPages * BITRIX_PAGE_SIZE);
        const commands: Record<string, BitrixBatchCommand> = {};
        const keys: string[] = [];
        for (
          let start = firstResponse.next, page = 1;
          start < targetCount && page < this.config.bitrixMaxPages;
          start += BITRIX_PAGE_SIZE, page += 1
        ) {
          const key = `u_${start}`;
          keys.push(key);
          commands[key] = {
            method: "user.get",
            params: { filter, start },
          };
        }
        if (keys.length > 0) {
          const batchResult = await this.bitrix.callBatch<unknown[]>(connectionId, commands);
          for (const key of keys) {
            const pageUsers = batchResult[key];
            if (Array.isArray(pageUsers)) {
              users.push(...pageUsers);
            }
          }
        }
      } else {
        let start = firstResponse.next;
        for (let page = 1; page < this.config.bitrixMaxPages; page += 1) {
          const pageResponse = await this.bitrix.call<unknown[]>(connectionId, "user.get", { filter, start });
          users.push(...(Array.isArray(pageResponse.result) ? pageResponse.result : []));
          if (pageResponse.next === undefined) break;
          start = pageResponse.next;
        }
      }
    }
    return users;
  }
}
