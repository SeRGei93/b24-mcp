import type { AppConfig } from "../config/env.js";
import { BitrixRestClient } from "./rest-client.js";

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
    const items: unknown[] = [];
    let start = 0;
    let total: number | undefined;
    for (let page = 0; page < this.config.bitrixMaxPages && items.length < input.limit; page += 1) {
      const response = await this.bitrix.call<{ items?: unknown[] }>(input.connectionId, "crm.item.list", {
        entityTypeId: input.entityTypeId,
        filter: input.filter ?? {},
        select: input.select ?? ["*"],
        order: input.order ?? { id: "ASC" },
        start,
      });
      items.push(...(response.result.items ?? []));
      total = response.total;
      if (response.next === undefined) break;
      start = response.next;
    }
    const sliced = items.slice(0, input.limit);
    return { items: sliced, ...(total === undefined ? {} : { total }), truncated: items.length > sliced.length || (total !== undefined && total > sliced.length) };
  }

  async getItem(connectionId: string, entityTypeId: number, id: number): Promise<unknown> {
    const response = await this.bitrix.call<{ item?: unknown }>(connectionId, "crm.item.get", { entityTypeId, id });
    return response.result.item ?? response.result;
  }

  async listUsers(connectionId: string, filter: Record<string, unknown> = {}): Promise<unknown[]> {
    const users: unknown[] = [];
    let start = 0;
    for (let page = 0; page < this.config.bitrixMaxPages; page += 1) {
      const response = await this.bitrix.call<unknown[]>(connectionId, "user.get", { filter, start });
      users.push(...(Array.isArray(response.result) ? response.result : []));
      if (response.next === undefined) break;
      start = response.next;
    }
    return users;
  }
}
