import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { z } from "zod/v4";
import type { AppContainer } from "../container.js";
import { AppError, safeErrorMessage } from "../errors.js";
import { toPublicConnection } from "../types.js";

const connectionId = z.uuid().describe("ID returned by bitrix_list_connections after OAuth authorization");
const entityTypeId = z.number().int().positive().describe("Bitrix CRM entityTypeId (1 lead, 2 deal, 3 contact, 4 company, or an SPA ID)");
const itemId = z.number().int().positive();
const jsonObject = z.record(z.string(), z.unknown());

export function createMcpServer(container: AppContainer, authorizedConnectionId?: string): McpServer {
  const server = new McpServer({ name: "bitrix24-mcp-server", version: "0.3.1" });
  const connectionFor = (requested?: string): string | undefined => {
    if (authorizedConnectionId && requested && requested !== authorizedConnectionId) {
      throw new AppError("This MCP token is not authorized for that Bitrix24 connection", "CONNECTION_ACCESS_DENIED", 403);
    }
    return authorizedConnectionId ?? requested;
  };

  server.registerTool("bitrix_list_connections", {
    title: "List Bitrix24 connections",
    description: "List configured Bitrix24 portals without exposing OAuth tokens or client secrets.",
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, () => execute(async () => (await container.connections.list())
    .filter((connection) => !authorizedConnectionId || connection.id === authorizedConnectionId)
    .map(toPublicConnection)));

  server.registerTool("bitrix_discover_portal", {
    title: "Discover Bitrix24 portal",
    description: "Return the portal's CRM entity types, including smart-process automations (SPAs).",
    inputSchema: { connectionId, refresh: z.boolean().default(false) },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: requested, refresh }) => execute(async () => {
    const id = connectionFor(requested)!;
    return {
      connection: toPublicConnection(await container.connections.get(id)),
      entityTypes: await container.schema.listEntityTypes(id, refresh),
    };
  }));

  server.registerTool("bitrix_list_crm_entities", {
    title: "List CRM entity types",
    description: "List built-in CRM entities and dynamic SPA entity types.",
    inputSchema: { connectionId, refresh: z.boolean().default(false) },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: id, refresh }) => execute(() => container.schema.listEntityTypes(connectionFor(id)!, refresh)));

  server.registerTool("bitrix_describe_entity", {
    title: "Describe CRM entity",
    description: "Describe an entity's fields, categories, and stages. Supply entityTypeId or an exact entity name.",
    inputSchema: {
      connectionId,
      entityTypeId: entityTypeId.optional(),
      entityName: z.string().min(1).optional(),
      categoryId: z.number().int().nonnegative().optional(),
      refresh: z.boolean().default(false),
    },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: id, entityTypeId: typeId, entityName, categoryId, refresh }) => execute(async () => {
    id = connectionFor(id)!;
    if (typeId === undefined && !entityName) throw new AppError("Supply entityTypeId or entityName", "MISSING_ENTITY");
    const resolved = typeId ?? (await container.schema.resolveEntityType(id, entityName!)).entityTypeId;
    return container.schema.describe(id, resolved, categoryId, refresh);
  }));

  server.registerTool("bitrix_list_fields", {
    title: "List CRM fields",
    description: "List standard and custom fields for a CRM or SPA entity.",
    inputSchema: { connectionId, entityTypeId, refresh: z.boolean().default(false) },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: id, entityTypeId: typeId, refresh }) =>
    execute(() => container.schema.fields(connectionFor(id)!, typeId, refresh)));

  server.registerTool("bitrix_list_categories", {
    title: "List CRM categories",
    description: "List funnels/categories for a CRM or SPA entity.",
    inputSchema: { connectionId, entityTypeId, refresh: z.boolean().default(false) },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: id, entityTypeId: typeId, refresh }) =>
    execute(() => container.schema.categories(connectionFor(id)!, typeId, refresh)));

  server.registerTool("bitrix_list_stages", {
    title: "List CRM stages",
    description: "List stages for an entity and optional category/funnel.",
    inputSchema: {
      connectionId,
      entityTypeId,
      categoryId: z.number().int().nonnegative().optional(),
      refresh: z.boolean().default(false),
    },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: id, entityTypeId: typeId, categoryId, refresh }) =>
    execute(() => container.schema.stages(connectionFor(id)!, typeId, categoryId, refresh)));

  server.registerTool("bitrix_refresh_schema_cache", {
    title: "Refresh schema cache",
    description: "Clear cached Bitrix24 metadata so the next discovery call loads fresh schema data.",
    inputSchema: { connectionId },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: id }) => execute(() => container.schema.refresh(connectionFor(id)!)));

  server.registerTool("bitrix_search_items", {
    title: "Search CRM items",
    description: "Search leads, deals, contacts, companies, or SPA items with Bitrix filters and batch pagination. Set includeProductRows=true to fetch product rows for all matched items in one batch request.",
    inputSchema: {
      connectionId,
      entityTypeId,
      filter: jsonObject.optional(),
      select: z.array(z.string().min(1)).max(100).optional(),
      order: z.record(z.string(), z.enum(["ASC", "DESC"])).optional(),
      limit: z.number().int().min(1).max(1_000).default(100),
      includeProductRows: z.boolean().default(false).describe("Automatically batch-fetch product rows (crm.item.productrow.list) for all returned items in one request"),
    },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, (input) => execute(() => container.crm.searchItems({
    ...input,
    connectionId: connectionFor(input.connectionId)!,
  })));

  server.registerTool("bitrix_get_item", {
    title: "Get CRM item(s)",
    description: "Get one CRM/SPA item by id, or multiple items in a single batch call by supplying ids.",
    inputSchema: {
      connectionId,
      entityTypeId,
      id: itemId.optional().describe("Single CRM item ID"),
      ids: z.array(itemId).min(1).max(250).optional().describe("Multiple CRM item IDs to fetch in one Bitrix24 batch request"),
    },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: connId, entityTypeId: typeId, id: crmId, ids }) => execute(async () => {
    const resolvedConn = connectionFor(connId)!;
    if (ids && ids.length > 0) {
      return container.crm.getItems(resolvedConn, typeId, ids);
    }
    if (crmId !== undefined) {
      return container.crm.getItem(resolvedConn, typeId, crmId);
    }
    throw new AppError("Supply id or ids", "MISSING_ITEM_ID");
  }));

  server.registerTool("bitrix_list_product_rows", {
    title: "List CRM item product rows (batch)",
    description: "Batch-fetch product rows (line items) for one or multiple deals/CRM items at once via Bitrix24 batch + crm.item.productrow.list. Pass dealIds/ownerIds array to fetch product rows for up to 250 deals in a single call instead of looping.",
    inputSchema: {
      connectionId,
      dealIds: z.array(itemId).min(1).max(250).optional().describe("Array of Deal IDs to fetch product rows for in one batch request (preferred over calling one by one)"),
      ownerIds: z.array(itemId).min(1).max(250).optional().describe("Array of CRM item IDs to fetch product rows for in one batch request"),
      ownerId: itemId.optional().describe("Single CRM item ID (alias: dealId)"),
      dealId: itemId.optional().describe("Single Deal ID (prefer dealIds array when checking multiple deals)"),
      entityTypeId: entityTypeId.default(2),
      ownerType: z.string().min(1).optional().describe("Optional short ownerType code ('D', 'L', 'Q', 'SI', 'T...'). Auto-derived from entityTypeId if omitted."),
      filter: jsonObject.optional(),
      order: z.record(z.string(), z.enum(["asc", "desc", "ASC", "DESC"])).optional(),
      limit: z.number().int().min(1).max(1_000).default(100),
    },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: id, dealIds, ownerIds, ownerId, dealId, entityTypeId: typeId, ownerType, filter, order, limit }) => execute(async () => {
    const resolvedIds = dealIds ?? ownerIds;
    const resolvedSingleId = ownerId ?? dealId;
    if (!resolvedIds?.length && resolvedSingleId === undefined) {
      throw new AppError("Supply dealIds, ownerIds, dealId, or ownerId", "MISSING_OWNER_ID");
    }
    return container.crm.listProductRows({
      connectionId: connectionFor(id)!,
      ownerIds: resolvedIds,
      ownerId: resolvedSingleId,
      entityTypeId: typeId,
      ownerType,
      filter,
      order,
      limit,
    });
  }));

  server.registerTool("bitrix_list_users", {
    title: "List Bitrix24 users",
    description: "List portal users visible to the OAuth user.",
    inputSchema: { connectionId, filter: jsonObject.optional() },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: id, filter }) => execute(() => container.crm.listUsers(connectionFor(id)!, filter)));

  server.registerTool("bitrix_prepare_create_item", {
    title: "Prepare CRM item creation",
    description: "Prepare, but do not execute, an item creation. Returns an operation ID requiring explicit confirmation.",
    inputSchema: { connectionId, entityTypeId, fields: jsonObject },
    annotations: { readOnlyHint: false, destructiveHint: false },
  }, ({ connectionId: id, entityTypeId: typeId, fields }) =>
    execute(() => container.operations.prepareCreate(connectionFor(id)!, typeId, fields)));

  server.registerTool("bitrix_prepare_update_item", {
    title: "Prepare CRM item update",
    description: "Prepare, but do not execute, an item update. Returns an operation ID requiring explicit confirmation.",
    inputSchema: { connectionId, entityTypeId, id: itemId, fields: jsonObject },
    annotations: { readOnlyHint: false, destructiveHint: false },
  }, ({ connectionId: connection, entityTypeId: typeId, id, fields }) =>
    execute(() => container.operations.prepareUpdate(connectionFor(connection)!, typeId, id, fields)));

  server.registerTool("bitrix_prepare_move_item_stage", {
    title: "Prepare CRM stage move",
    description: "Prepare, but do not execute, a stage move. Returns an operation ID requiring explicit confirmation.",
    inputSchema: { connectionId, entityTypeId, id: itemId, stageId: z.string().min(1) },
    annotations: { readOnlyHint: false, destructiveHint: false },
  }, ({ connectionId: connection, entityTypeId: typeId, id, stageId }) =>
    execute(() => container.operations.prepareMove(connectionFor(connection)!, typeId, id, stageId)));

  server.registerTool("bitrix_confirm_operation", {
    title: "Confirm pending operation",
    description: "Explicitly confirm and execute one pending Bitrix24 write operation.",
    inputSchema: { operationId: z.uuid() },
    annotations: { readOnlyHint: false, destructiveHint: true },
  }, ({ operationId }) => execute(() => container.operations.confirm(operationId, authorizedConnectionId)));

  server.registerTool("bitrix_cancel_operation", {
    title: "Cancel pending operation",
    description: "Cancel one pending operation without calling Bitrix24.",
    inputSchema: { operationId: z.uuid() },
    annotations: { readOnlyHint: false, destructiveHint: false },
  }, ({ operationId }) => execute(() => container.operations.cancel(operationId, authorizedConnectionId)));

  server.registerTool("bitrix_list_pending_operations", {
    title: "List pending operations",
    description: "List recent prepared operations and their current statuses.",
    inputSchema: { connectionId: connectionId.optional() },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: id }) => execute(() => container.operations.list(connectionFor(id))));

  return server;
}

async function execute<T>(work: () => Promise<T>) {
  try {
    const value = await work();
    return { content: [{ type: "text" as const, text: JSON.stringify(value, null, 2) }] };
  } catch (error) {
    const code = error instanceof AppError ? error.code : "INTERNAL_ERROR";
    return {
      isError: true,
      content: [{ type: "text" as const, text: JSON.stringify({ error: code, message: safeErrorMessage(error) }) }],
    };
  }
}
