import type { AppConfig } from "../config/env.js";
import type { SchemaCacheRepository } from "../db/repositories.js";
import { AppError } from "../errors.js";
import { BitrixRestClient, type BitrixBatchCommand } from "./rest-client.js";

const BITRIX_PAGE_SIZE = 50;

export interface EntityType {
  entityTypeId: number;
  title: string;
  isDynamic: boolean;
  [key: string]: unknown;
}

const BUILT_IN_TYPES: EntityType[] = [
  { entityTypeId: 1, title: "Lead", isDynamic: false },
  { entityTypeId: 2, title: "Deal", isDynamic: false },
  { entityTypeId: 3, title: "Contact", isDynamic: false },
  { entityTypeId: 4, title: "Company", isDynamic: false },
  { entityTypeId: 31, title: "Invoice", isDynamic: false },
];

export class SchemaDiscoveryService {
  constructor(
    private readonly config: AppConfig,
    private readonly cache: SchemaCacheRepository,
    private readonly bitrix: BitrixRestClient,
  ) {}

  async listEntityTypes(connectionId: string, refresh = false): Promise<EntityType[]> {
    return this.cached(connectionId, "entity-types", refresh, async () => {
      const dynamic = await this.collect<Record<string, unknown>>(connectionId, "crm.type.list", {}, "types");
      const normalized = dynamic.map((item) => ({
        ...item,
        entityTypeId: Number(item.entityTypeId),
        title: String(item.title ?? item.name ?? `Entity ${String(item.entityTypeId)}`),
        isDynamic: true,
      })).filter((item) => Number.isInteger(item.entityTypeId));
      const ids = new Set(normalized.map((item) => item.entityTypeId));
      return [...BUILT_IN_TYPES.filter((item) => !ids.has(item.entityTypeId)), ...normalized];
    });
  }

  async fields(connectionId: string, entityTypeId: number, refresh = false): Promise<Record<string, unknown>> {
    return this.cached(connectionId, `fields:${entityTypeId}`, refresh, async () => {
      const response = await this.bitrix.call<{ fields?: Record<string, unknown> }>(connectionId, "crm.item.fields", { entityTypeId });
      return response.result.fields ?? {};
    });
  }

  async categories(connectionId: string, entityTypeId: number, refresh = false): Promise<Record<string, unknown>[]> {
    return this.cached(connectionId, `categories:${entityTypeId}`, refresh, () =>
      this.collect(connectionId, "crm.category.list", { entityTypeId }, "categories"));
  }

  async stages(connectionId: string, entityTypeId: number, categoryId?: number, refresh = false): Promise<Record<string, unknown>[]> {
    const effectiveCategory = categoryId ?? 0;
    const key = `stages:${entityTypeId}:${effectiveCategory}`;
    return this.cached(connectionId, key, refresh, async () => {
      const entityId = stageEntityId(entityTypeId, effectiveCategory);
      return this.collect(connectionId, "crm.status.list", { filter: { ENTITY_ID: entityId }, order: { SORT: "ASC" } });
    });
  }

  async describe(connectionId: string, entityTypeId: number, categoryId?: number, refresh = false): Promise<Record<string, unknown>> {
    const entities = await this.listEntityTypes(connectionId, refresh);
    const entity = entities.find((candidate) => candidate.entityTypeId === entityTypeId);
    if (!entity) throw new AppError(`Unknown CRM entity type '${entityTypeId}'`, "UNKNOWN_ENTITY_TYPE", 404);
    const [fields, categories] = await Promise.all([
      this.fields(connectionId, entityTypeId, refresh),
      this.categories(connectionId, entityTypeId, refresh).catch(() => []),
    ]);
    const selectedCategories = categoryId === undefined ? categories : categories.filter((category) => Number(category.id) === categoryId);
    const categoryIds = (selectedCategories.length ? selectedCategories : [{ id: categoryId ?? 0 }])
      .map((category) => Number(category.id ?? 0));

    const stagesByCategory = new Map<number, Record<string, unknown>[]>();
    const uncachedCategoryIds: number[] = [];

    for (const catId of categoryIds) {
      if (!refresh) {
        const cachedStages = await this.cache.get<Record<string, unknown>[]>(
          connectionId,
          `stages:${entityTypeId}:${catId}`,
          this.config.schemaCacheTtlSeconds,
        );
        if (cachedStages !== null) {
          stagesByCategory.set(catId, cachedStages);
          continue;
        }
      }
      uncachedCategoryIds.push(catId);
    }

    if (uncachedCategoryIds.length === 1) {
      const catId = uncachedCategoryIds[0]!;
      const loaded = await this.stages(connectionId, entityTypeId, catId, refresh).catch(() => []);
      stagesByCategory.set(catId, loaded);
    } else if (uncachedCategoryIds.length > 1) {
      const commands: Record<string, BitrixBatchCommand> = {};
      for (const catId of uncachedCategoryIds) {
        commands[`cat_${catId}`] = {
          method: "crm.status.list",
          params: {
            filter: { ENTITY_ID: stageEntityId(entityTypeId, catId) },
            order: { SORT: "ASC" },
          },
        };
      }
      try {
        const batchResult = await this.bitrix.callBatch<Record<string, unknown>[]>(connectionId, commands);
        for (const catId of uncachedCategoryIds) {
          const list = Array.isArray(batchResult[`cat_${catId}`]) ? batchResult[`cat_${catId}`]! : [];
          await this.cache.set(connectionId, `stages:${entityTypeId}:${catId}`, list);
          stagesByCategory.set(catId, list);
        }
      } catch {
        for (const catId of uncachedCategoryIds) {
          stagesByCategory.set(catId, []);
        }
      }
    }

    const stages = categoryIds.map((catId) => ({
      categoryId: catId,
      stages: stagesByCategory.get(catId) ?? [],
    }));
    return { entity, fields, categories, stages };
  }

  async resolveEntityType(connectionId: string, name: string): Promise<EntityType> {
    const normalized = normalizeName(name);
    const matches = (await this.listEntityTypes(connectionId)).filter((entity) =>
      normalizeName(entity.title) === normalized || String(entity.entityTypeId) === normalized);
    if (matches.length === 1) return matches[0]!;
    if (matches.length > 1) throw new AppError(`Entity name '${name}' is ambiguous`, "AMBIGUOUS_ENTITY");
    throw new AppError(`No CRM entity matches '${name}'`, "UNKNOWN_ENTITY_TYPE", 404);
  }

  async refresh(connectionId: string): Promise<{ clearedEntries: number }> {
    return { clearedEntries: await this.cache.clear(connectionId) };
  }

  private async cached<T>(connectionId: string, key: string, refresh: boolean, load: () => Promise<T>): Promise<T> {
    if (!refresh) {
      const found = await this.cache.get<T>(connectionId, key, this.config.schemaCacheTtlSeconds);
      if (found !== null) return found;
    }
    const value = await load();
    await this.cache.set(connectionId, key, value);
    return value;
  }

  private async collect<T extends Record<string, unknown>>(
    connectionId: string,
    method: string,
    params: Record<string, unknown>,
    nestedKey?: string,
  ): Promise<T[]> {
    const extract = (result: unknown): T[] => {
      if (nestedKey && result && typeof result === "object" && !Array.isArray(result)) {
        return ((result as Record<string, unknown>)[nestedKey] as T[] | undefined) ?? [];
      }
      return Array.isArray(result) ? (result as T[]) : [];
    };

    const firstResponse = await this.bitrix.call<unknown>(connectionId, method, { ...params, start: 0 });
    const values: T[] = [...extract(firstResponse.result)];
    const total = firstResponse.total;

    if (firstResponse.next !== undefined && this.config.bitrixMaxPages > 1) {
      if (total !== undefined && total > values.length) {
        const targetCount = Math.min(total, this.config.bitrixMaxPages * BITRIX_PAGE_SIZE);
        const commands: Record<string, BitrixBatchCommand> = {};
        const keys: string[] = [];
        for (
          let start = firstResponse.next, page = 1;
          start < targetCount && page < this.config.bitrixMaxPages;
          start += BITRIX_PAGE_SIZE, page += 1
        ) {
          const key = `p_${start}`;
          keys.push(key);
          commands[key] = { method, params: { ...params, start } };
        }
        if (keys.length > 0) {
          const batchResult = await this.bitrix.callBatch<unknown>(connectionId, commands);
          for (const key of keys) {
            values.push(...extract(batchResult[key]));
          }
        }
      } else {
        let start = firstResponse.next;
        for (let page = 1; page < this.config.bitrixMaxPages; page += 1) {
          const response = await this.bitrix.call<unknown>(connectionId, method, { ...params, start });
          values.push(...extract(response.result));
          if (response.next === undefined) break;
          start = response.next;
        }
      }
    }
    return values;
  }
}

function stageEntityId(entityTypeId: number, categoryId: number): string {
  if (entityTypeId === 1) return "STATUS";
  if (entityTypeId === 2) return categoryId === 0 ? "DEAL_STAGE" : `DEAL_STAGE_${categoryId}`;
  return `DYNAMIC_${entityTypeId}_STAGE_${categoryId}`;
}

function normalizeName(value: string): string {
  return value.normalize("NFKD").replace(/[\u0300-\u036f]/g, "").trim().toLocaleLowerCase();
}
