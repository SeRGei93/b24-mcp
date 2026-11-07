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
    description: "Search leads, deals, contacts, companies, or SPA items with Bitrix filters and pagination.",
    inputSchema: {
      connectionId,
      entityTypeId,
      filter: jsonObject.optional(),
      select: z.array(z.string().min(1)).max(100).optional(),
      order: z.record(z.string(), z.enum(["ASC", "DESC"])).optional(),
      limit: z.number().int().min(1).max(1_000).default(100),
    },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, (input) => execute(() => container.crm.searchItems({
    ...input,
    connectionId: connectionFor(input.connectionId)!,
  })));

  server.registerTool("bitrix_get_item", {
    title: "Get CRM item",
    description: "Get one CRM or SPA item by entity type and ID.",
    inputSchema: { connectionId, entityTypeId, id: itemId },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: id, entityTypeId: typeId, id: crmId }) =>
    execute(() => container.crm.getItem(connectionFor(id)!, typeId, crmId)));

  server.registerTool("bitrix_list_product_rows", {
    title: "List CRM item product rows",
    description: "List product rows (line items) for a deal, lead, quote, smart invoice, or SPA item via crm.item.productrow.list. Defaults to Deal (entityTypeId=2, ownerType='D').",
    inputSchema: {
      connectionId,
      ownerId: itemId.optional().describe("ID of the deal or CRM item (alias: dealId)"),
      dealId: itemId.optional().describe("Deal ID (alias for ownerId with entityTypeId=2)"),
      entityTypeId: entityTypeId.default(2),
      ownerType: z.string().min(1).optional().describe("Optional short ownerType code ('D', 'L', 'Q', 'SI', 'T...'). Auto-derived from entityTypeId if omitted."),
      filter: jsonObject.optional(),
      order: z.record(z.string(), z.enum(["asc", "desc", "ASC", "DESC"])).optional(),
      limit: z.number().int().min(1).max(1_000).default(100),
    },
    annotations: { readOnlyHint: true, destructiveHint: false },
  }, ({ connectionId: id, ownerId, dealId, entityTypeId: typeId, ownerType, filter, order, limit }) => execute(async () => {
    const resolvedOwnerId = ownerId ?? dealId;
    if (resolvedOwnerId === undefined) {
      throw new AppError("Supply ownerId or dealId", "MISSING_OWNER_ID");
    }
    return container.crm.listProductRows({
      connectionId: connectionFor(id)!,
      ownerId: resolvedOwnerId,
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
