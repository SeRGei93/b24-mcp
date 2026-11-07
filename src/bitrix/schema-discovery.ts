import type { AppConfig } from "../config/env.js";
import type { SchemaCacheRepository } from "../db/repositories.js";
import { AppError } from "../errors.js";
import { BitrixRestClient } from "./rest-client.js";

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
    const stages = await Promise.all(
      (selectedCategories.length ? selectedCategories : [{ id: categoryId ?? 0 }]).map(async (category) => ({
        categoryId: Number(category.id ?? 0),
        stages: await this.stages(connectionId, entityTypeId, Number(category.id ?? 0), refresh).catch(() => []),
      })),
    );
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
    const values: T[] = [];
    let start = 0;
    for (let page = 0; page < this.config.bitrixMaxPages; page += 1) {
      const response = await this.bitrix.call<unknown>(connectionId, method, { ...params, start });
      const result = response.result as Record<string, unknown> | unknown[];
      const pageValues = nestedKey && !Array.isArray(result)
        ? (result[nestedKey] as T[] | undefined) ?? []
        : Array.isArray(result) ? result as T[] : [];
      values.push(...pageValues);
      if (response.next === undefined) break;
      start = response.next;
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
