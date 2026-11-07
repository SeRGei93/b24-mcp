import type { AppConfig } from "../config/env.js";
import { BitrixRestClient, type BitrixBatchCommand } from "./rest-client.js";

const BITRIX_PAGE_SIZE = 50;

export function toOwnerTypeCode(entityTypeId: number): string {
  switch (entityTypeId) {
    case 1:
      return "L";
    case 2:
      return "D";
    case 3:
      return "C";
    case 4:
      return "CO";
    case 7:
      return "Q";
    case 31:
      return "SI";
    default:
      return `T${entityTypeId.toString(16)}`;
  }
}

export class CrmService {
  constructor(private readonly config: AppConfig, private readonly bitrix: BitrixRestClient) {}

  async searchItems(input: {
    connectionId: string;
    entityTypeId: number;
    filter?: Record<string, unknown>;
    select?: string[];
    order?: Record<string, "ASC" | "DESC">;
    limit: number;
    includeProductRows?: boolean;
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

    let sliced = items.slice(0, input.limit);
    if (input.includeProductRows && sliced.length > 0) {
      const ownerIds = sliced
        .map((item) => {
          if (item && typeof item === "object") {
            const rawId = (item as Record<string, unknown>).id ?? (item as Record<string, unknown>).ID;
            return Number(rawId);
          }
          return Number.NaN;
        })
        .filter((id) => Number.isInteger(id) && id > 0);

      if (ownerIds.length > 0) {
        const batchRows = await this.listProductRowsForOwners({
          connectionId: input.connectionId,
          ownerIds,
          entityTypeId: input.entityTypeId,
        });
        sliced = sliced.map((item) => {
          if (item && typeof item === "object") {
            const obj = item as Record<string, unknown>;
            const id = String(obj.id ?? obj.ID ?? "");
            return { ...obj, productRows: batchRows.byOwnerId[id] ?? [] };
          }
          return item;
        });
      }
    }

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

  async getItems(connectionId: string, entityTypeId: number, ids: number[]): Promise<Record<string, unknown>> {
    const uniqueIds = [...new Set(ids)];
    const commands: Record<string, BitrixBatchCommand> = {};
    for (const id of uniqueIds) {
      commands[String(id)] = {
        method: "crm.item.get",
        params: { entityTypeId, id },
      };
    }
    const batchResult = await this.bitrix.callBatch<{ item?: unknown }>(connectionId, commands);
    const itemsById: Record<string, unknown> = {};
    for (const id of uniqueIds) {
      const key = String(id);
      const entry = batchResult[key];
      if (entry !== undefined) {
        itemsById[key] = entry && typeof entry === "object" && "item" in entry ? entry.item : entry;
      }
    }
    return itemsById;
  }

  async listProductRowsForOwners(input: {
    connectionId: string;
    ownerIds: number[];
    entityTypeId?: number;
    ownerType?: string;
    filter?: Record<string, unknown>;
    order?: Record<string, "asc" | "desc" | "ASC" | "DESC">;
  }): Promise<{ byOwnerId: Record<string, unknown[]>; productRows: unknown[]; ownersCount: number }> {
    const uniqueOwnerIds = [...new Set(input.ownerIds)];
    const ownerType = input.ownerType ?? toOwnerTypeCode(input.entityTypeId ?? 2);
    const order: Record<string, "asc" | "desc"> = input.order
      ? Object.fromEntries(Object.entries(input.order).map(([k, v]) => [k, v.toLowerCase() as "asc" | "desc"]))
      : { sort: "asc" };

    const commands: Record<string, BitrixBatchCommand> = {};
    for (const ownerId of uniqueOwnerIds) {
      commands[String(ownerId)] = {
        method: "crm.item.productrow.list",
        params: {
          filter: {
            ...(input.filter ?? {}),
            "=ownerType": ownerType,
            "=ownerId": ownerId,
          },
          order,
          start: 0,
        },
      };
    }

    const batchResult = await this.bitrix.callBatch<{ productRows?: unknown[] }>(input.connectionId, commands);
    const byOwnerId: Record<string, unknown[]> = {};
    const productRows: unknown[] = [];

    for (const ownerId of uniqueOwnerIds) {
      const key = String(ownerId);
      const rows = batchResult[key]?.productRows ?? [];
      byOwnerId[key] = rows;
      productRows.push(...rows);
    }

    return {
      byOwnerId,
      productRows,
      ownersCount: uniqueOwnerIds.length,
    };
  }

  async listProductRows(input: {
    connectionId: string;
    ownerId?: number;
    ownerIds?: number[];
    entityTypeId?: number;
    ownerType?: string;
    filter?: Record<string, unknown>;
    order?: Record<string, "asc" | "desc" | "ASC" | "DESC">;
    limit?: number;
  }): Promise<
    | { productRows: unknown[]; total?: number; truncated: boolean }
    | { byOwnerId: Record<string, unknown[]>; productRows: unknown[]; ownersCount: number }
  > {
    const ids = input.ownerIds?.length
      ? input.ownerIds
      : input.ownerId !== undefined
        ? [input.ownerId]
        : [];

    if (ids.length > 1 || (input.ownerIds && input.ownerIds.length > 0)) {
      return this.listProductRowsForOwners({
        connectionId: input.connectionId,
        ownerIds: ids,
        entityTypeId: input.entityTypeId,
        ownerType: input.ownerType,
        filter: input.filter,
        order: input.order,
      });
    }

    const singleOwnerId = ids[0]!;
    const limit = input.limit ?? 100;
    const ownerType = input.ownerType ?? toOwnerTypeCode(input.entityTypeId ?? 2);
    const filter: Record<string, unknown> = {
      ...(input.filter ?? {}),
      "=ownerType": ownerType,
      "=ownerId": singleOwnerId,
    };
    const order: Record<string, "asc" | "desc"> = input.order
      ? Object.fromEntries(Object.entries(input.order).map(([k, v]) => [k, v.toLowerCase() as "asc" | "desc"]))
      : { sort: "asc" };

    // Execute via batch by default even for a single ownerId when requested, or standard call with batch follow-up
    const batchSingle = await this.listProductRowsForOwners({
      connectionId: input.connectionId,
      ownerIds: [singleOwnerId],
      entityTypeId: input.entityTypeId,
      ownerType,
      filter: input.filter,
      order,
    });
    const productRows = batchSingle.productRows.slice(0, limit);
    return {
      productRows,
      total: batchSingle.productRows.length,
      truncated: batchSingle.productRows.length > productRows.length,
    };
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
