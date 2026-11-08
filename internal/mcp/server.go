package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func NewServer(
	st *store.Store,
	schema *bitrix.SchemaDiscovery,
	crm *bitrix.CrmService,
	ops *bitrix.OperationService,
	authorizedConnectionID string,
) *mcpserver.MCPServer {
	srv := mcpserver.NewMCPServer(
		"bitrix24-mcp-server",
		"0.1",
		mcpserver.WithToolCapabilities(false),
	)

	connFor := func(requested string) (string, error) {
		resolved, err := ResolveConnectionID(authorizedConnectionID, requested)
		if err != nil {
			return "", err
		}
		if resolved == "" {
			return "", config.NewAppError("connectionId is required", "MISSING_CONNECTION_ID", 400)
		}
		return resolved, nil
	}

	// 1. bitrix_list_connections
	listConnTool := mcpproto.NewTool(
		"bitrix_list_connections",
		mcpproto.WithTitleAnnotation("List Bitrix24 connections"),
		mcpproto.WithDescription("List configured Bitrix24 portals without exposing OAuth tokens or client secrets."),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(listConnTool, func(ctx context.Context, _ mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			conns, err := st.ListConnections(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]store.PublicConnection, 0, len(conns))
			for _, c := range conns {
				if authorizedConnectionID != "" && c.ID != authorizedConnectionID {
					continue
				}
				out = append(out, c.ToPublic())
			}
			return out, nil
		}), nil
	})

	// 2. bitrix_discover_portal
	discoverTool := mcpproto.NewTool(
		"bitrix_discover_portal",
		mcpproto.WithTitleAnnotation("Discover Bitrix24 portal"),
		mcpproto.WithDescription("Return the portal's CRM entity types, including smart-process automations (SPAs)."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithBoolean("refresh", mcpproto.Description("Force refresh schema cache"), mcpproto.DefaultBool(false)),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(discoverTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if schema == nil {
				return nil, config.NewAppError("Schema discovery is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			conn, err := st.GetConnection(ctx, connID)
			if err != nil {
				return nil, err
			}
			types, err := schema.ListEntityTypes(ctx, connID, req.GetBool("refresh", false))
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"connection":  conn.ToPublic(),
				"entityTypes": types,
			}, nil
		}), nil
	})

	// 3. bitrix_list_crm_entities
	listEntitiesTool := mcpproto.NewTool(
		"bitrix_list_crm_entities",
		mcpproto.WithTitleAnnotation("List CRM entity types"),
		mcpproto.WithDescription("List built-in CRM entities and dynamic SPA entity types."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithBoolean("refresh", mcpproto.Description("Force refresh schema cache"), mcpproto.DefaultBool(false)),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(listEntitiesTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if schema == nil {
				return nil, config.NewAppError("Schema discovery is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			return schema.ListEntityTypes(ctx, connID, req.GetBool("refresh", false))
		}), nil
	})

	// 4. bitrix_describe_entity
	describeTool := mcpproto.NewTool(
		"bitrix_describe_entity",
		mcpproto.WithTitleAnnotation("Describe CRM entity"),
		mcpproto.WithDescription("Describe an entity's fields, categories, and stages. Supply entityTypeId or an exact entity name."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId (1 lead, 2 deal, 3 contact, 4 company, or an SPA ID)")),
		mcpproto.WithString("entityName", mcpproto.Description("Exact entity title or name")),
		mcpproto.WithInteger("categoryId", mcpproto.Description("Category/funnel ID")),
		mcpproto.WithBoolean("refresh", mcpproto.Description("Force refresh schema cache"), mcpproto.DefaultBool(false)),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(describeTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if schema == nil {
				return nil, config.NewAppError("Schema discovery is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			args := req.GetArguments()
			typeID := bitrix.ToInt(args["entityTypeId"])
			entityName := req.GetString("entityName", "")
			if typeID <= 0 && entityName == "" {
				return nil, config.NewAppError("Supply entityTypeId or entityName", "MISSING_ENTITY", 400)
			}
			if typeID <= 0 {
				resolved, err := schema.ResolveEntityType(ctx, connID, entityName)
				if err != nil {
					return nil, err
				}
				typeID = bitrix.ToInt(resolved["entityTypeId"])
			}
			var categoryID *int
			if catVal, ok := args["categoryId"]; ok && catVal != nil {
				c := bitrix.ToInt(catVal)
				categoryID = &c
			}
			return schema.Describe(ctx, connID, typeID, categoryID, req.GetBool("refresh", false))
		}), nil
	})

	// 5. bitrix_list_fields
	listFieldsTool := mcpproto.NewTool(
		"bitrix_list_fields",
		mcpproto.WithTitleAnnotation("List CRM fields"),
		mcpproto.WithDescription("List standard and custom fields for a CRM or SPA entity."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId"), mcpproto.Required()),
		mcpproto.WithBoolean("refresh", mcpproto.Description("Force refresh schema cache"), mcpproto.DefaultBool(false)),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(listFieldsTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if schema == nil {
				return nil, config.NewAppError("Schema discovery is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			typeID := req.GetInt("entityTypeId", 0)
			return schema.Fields(ctx, connID, typeID, req.GetBool("refresh", false))
		}), nil
	})

	// 6. bitrix_list_categories
	listCatsTool := mcpproto.NewTool(
		"bitrix_list_categories",
		mcpproto.WithTitleAnnotation("List CRM categories"),
		mcpproto.WithDescription("List funnels/categories for a CRM or SPA entity."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId"), mcpproto.Required()),
		mcpproto.WithBoolean("refresh", mcpproto.Description("Force refresh schema cache"), mcpproto.DefaultBool(false)),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(listCatsTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if schema == nil {
				return nil, config.NewAppError("Schema discovery is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			typeID := req.GetInt("entityTypeId", 0)
			return schema.Categories(ctx, connID, typeID, req.GetBool("refresh", false))
		}), nil
	})

	// 7. bitrix_list_stages
	listStagesTool := mcpproto.NewTool(
		"bitrix_list_stages",
		mcpproto.WithTitleAnnotation("List CRM stages"),
		mcpproto.WithDescription("List stages for an entity and optional category/funnel."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId"), mcpproto.Required()),
		mcpproto.WithInteger("categoryId", mcpproto.Description("Category/funnel ID")),
		mcpproto.WithBoolean("refresh", mcpproto.Description("Force refresh schema cache"), mcpproto.DefaultBool(false)),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(listStagesTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if schema == nil {
				return nil, config.NewAppError("Schema discovery is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			args := req.GetArguments()
			typeID := bitrix.ToInt(args["entityTypeId"])
			var categoryID *int
			if catVal, ok := args["categoryId"]; ok && catVal != nil {
				c := bitrix.ToInt(catVal)
				categoryID = &c
			}
			catID := 0
			if categoryID != nil {
				catID = *categoryID
			}
			return schema.Stages(ctx, connID, typeID, catID, req.GetBool("refresh", false))
		}), nil
	})

	// 8. bitrix_refresh_schema_cache
	refreshCacheTool := mcpproto.NewTool(
		"bitrix_refresh_schema_cache",
		mcpproto.WithTitleAnnotation("Refresh schema cache"),
		mcpproto.WithDescription("Clear cached Bitrix24 metadata so the next discovery call loads fresh schema data."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(refreshCacheTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if schema == nil {
				return nil, config.NewAppError("Schema discovery is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			return schema.Refresh(ctx, connID)
		}), nil
	})

	// 9. bitrix_search_items
	searchItemsTool := mcpproto.NewTool(
		"bitrix_search_items",
		mcpproto.WithTitleAnnotation("Search CRM items"),
		mcpproto.WithDescription("Search leads, deals, contacts, companies, quotes, smart invoices, or SPA items with Bitrix filters and pagination. Automatically includes productRows, payments, deliveries, and all custom fields by default."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId (1 lead, 2 deal, 3 contact, 4 company, 7 quote, 31 smart invoice, or an SPA ID)"), mcpproto.Required()),
		mcpproto.WithObject("filter", mcpproto.Description("Bitrix filter object or REST 3.0 filter expression array")),
		mcpproto.WithArray("select", mcpproto.Description("Fields to select (supports relational dot-notation such as stage.name, company.title)")),
		mcpproto.WithObject("order", mcpproto.Description("Order map (e.g. {'id': 'ASC'})")),
		mcpproto.WithInteger("limit", mcpproto.Description("Maximum items to return (up to 1000)"), mcpproto.DefaultNumber(100)),
		mcpproto.WithInteger("page", mcpproto.Description("Page number for pagination (1-based)"), mcpproto.DefaultNumber(1)),
		mcpproto.WithBoolean("includeDetails", mcpproto.Description("Automatically batch-fetch product rows, payments, deliveries, and full custom fields for returned items (default true)"), mcpproto.DefaultBool(true)),
		mcpproto.WithBoolean("includeProductRows", mcpproto.Description("Alias for includeDetails (default true)"), mcpproto.DefaultBool(true)),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(searchItemsTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if crm == nil {
				return nil, config.NewAppError("CRM service is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			args := req.GetArguments()
			filter := args["filter"]
			limit := bitrix.ToInt(args["limit"])
			if limit <= 0 {
				limit = 100
			}
			page := bitrix.ToInt(args["page"])
			if page <= 0 {
				page = 1
			}

			sel := extractStringSlice(args["select"])

			order := make(map[string]string)
			if ordRaw, ok := args["order"].(map[string]any); ok {
				for k, v := range ordRaw {
					if str, ok := v.(string); ok {
						order[k] = str
					}
				}
			}

			return crm.SearchItems(ctx, bitrix.SearchItemsInput{
				ConnectionID:   connID,
				EntityTypeID:   bitrix.ToInt(args["entityTypeId"]),
				Filter:         filter,
				Select:         sel,
				Order:          order,
				Limit:          limit,
				Page:           page,
				IncludeDetails: extractIncludeDetails(args),
			})
		}), nil
	})

	// 10. bitrix_count_items
	countItemsTool := mcpproto.NewTool(
		"bitrix_count_items",
		mcpproto.WithTitleAnnotation("Count CRM items"),
		mcpproto.WithDescription("Get the total count of CRM items matching an optional filter. Uses REST 3.0 aggregate when available, with automatic legacy fallback."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId (1 lead, 2 deal, 3 contact, 4 company, 7 quote, 31 smart invoice, or SPA ID)"), mcpproto.Required()),
		mcpproto.WithObject("filter", mcpproto.Description("Bitrix filter object or REST 3.0 filter expression array")),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(countItemsTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if crm == nil {
				return nil, config.NewAppError("CRM service is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			args := req.GetArguments()
			filter := args["filter"]
			typeID := bitrix.ToInt(args["entityTypeId"])

			count, err := crm.CountItems(ctx, bitrix.CountItemsInput{
				ConnectionID: connID,
				EntityTypeID: typeID,
				Filter:       filter,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"count": count}, nil
		}), nil
	})

	// 11. bitrix_get_item
	getItemTool := mcpproto.NewTool(
		"bitrix_get_item",
		mcpproto.WithTitleAnnotation("Get CRM item(s)"),
		mcpproto.WithDescription("Get one CRM/SPA item by id, or up to 1000 items in a single call by supplying ids. Automatically includes productRows, payments, deliveries, and all custom fields by default."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId"), mcpproto.Required()),
		mcpproto.WithInteger("id", mcpproto.Description("Single CRM item ID")),
		mcpproto.WithArray("ids", mcpproto.Description("Multiple CRM item IDs (up to 1000) to fetch in one request")),
		mcpproto.WithArray("select", mcpproto.Description("Fields to select (supports relational dot-notation such as stage.name, company.title)")),
		mcpproto.WithBoolean("includeDetails", mcpproto.Description("Automatically include productRows, payments, deliveries, and full custom fields (default true)"), mcpproto.DefaultBool(true)),
		mcpproto.WithBoolean("includeProductRows", mcpproto.Description("Alias for includeDetails (default true)"), mcpproto.DefaultBool(true)),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(getItemTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if crm == nil {
				return nil, config.NewAppError("CRM service is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			args := req.GetArguments()
			return crm.FetchItems(ctx, bitrix.GetItemInput{
				ConnectionID:   connID,
				EntityTypeID:   bitrix.ToInt(args["entityTypeId"]),
				ID:             bitrix.ToInt(args["id"]),
				IDs:            extractIntSlice(args["ids"]),
				Select:         extractStringSlice(args["select"]),
				IncludeDetails: extractIncludeDetails(args),
			})
		}), nil
	})

	// 11. bitrix_list_product_rows
	listProductRowsTool := mcpproto.NewTool(
		"bitrix_list_product_rows",
		mcpproto.WithTitleAnnotation("List CRM item product rows (batch)"),
		mcpproto.WithDescription("Batch-fetch product rows (line items) for one or multiple deals/CRM items at once via Bitrix24 batch + crm.item.productrow.list. Pass dealIds/ownerIds array to fetch product rows for up to 250 deals in a single call instead of looping."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithArray("dealIds", mcpproto.Description("Array of Deal IDs to fetch product rows for in one batch request (preferred over calling one by one)")),
		mcpproto.WithArray("ownerIds", mcpproto.Description("Array of CRM item IDs to fetch product rows for in one batch request")),
		mcpproto.WithInteger("ownerId", mcpproto.Description("Single CRM item ID (alias: dealId)")),
		mcpproto.WithInteger("dealId", mcpproto.Description("Single Deal ID (prefer dealIds array when checking multiple deals)")),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId (default: 2 for Deal)"), mcpproto.DefaultNumber(2)),
		mcpproto.WithString("ownerType", mcpproto.Description("Optional short ownerType code ('D', 'L', 'Q', 'SI', 'T...'). Auto-derived from entityTypeId if omitted.")),
		mcpproto.WithObject("filter", mcpproto.Description("Filter for product rows")),
		mcpproto.WithObject("order", mcpproto.Description("Order map")),
		mcpproto.WithInteger("limit", mcpproto.Description("Limit of product rows"), mcpproto.DefaultNumber(100)),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(listProductRowsTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if crm == nil {
				return nil, config.NewAppError("CRM service is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			args := req.GetArguments()

			var ownerIDs []int
			extractIDs := func(key string) {
				if raw, ok := args[key].([]any); ok {
					for _, item := range raw {
						if id := bitrix.ToInt(item); id > 0 {
							ownerIDs = append(ownerIDs, id)
						}
					}
				}
			}
			extractIDs("dealIds")
			if len(ownerIDs) == 0 {
				extractIDs("ownerIds")
			}

			var singleOwnerID *int
			if val, ok := args["ownerId"]; ok && val != nil {
				id := bitrix.ToInt(val)
				if id > 0 {
					singleOwnerID = &id
				}
			}
			if singleOwnerID == nil {
				if val, ok := args["dealId"]; ok && val != nil {
					id := bitrix.ToInt(val)
					if id > 0 {
						singleOwnerID = &id
					}
				}
			}

			if len(ownerIDs) == 0 && singleOwnerID == nil {
				return nil, config.NewAppError("Supply dealIds, ownerIds, dealId, or ownerId", "MISSING_OWNER_ID", 400)
			}

			typeID := bitrix.ToInt(args["entityTypeId"])
			if typeID <= 0 {
				typeID = 2
			}

			ownerType, _ := args["ownerType"].(string)
			filter, _ := args["filter"].(map[string]any)
			ord := make(map[string]string)
			if ordRaw, ok := args["order"].(map[string]any); ok {
				for k, v := range ordRaw {
					if s, ok := v.(string); ok {
						ord[k] = s
					}
				}
			}
			var limitPtr *int
			if limVal, ok := args["limit"]; ok && limVal != nil {
				l := bitrix.ToInt(limVal)
				if l > 0 {
					limitPtr = &l
				}
			}

			return crm.ListProductRows(ctx, bitrix.ListProductRowsInput{
				ConnectionID: connID,
				OwnerID:      singleOwnerID,
				OwnerIDs:     ownerIDs,
				EntityTypeID: &typeID,
				OwnerType:    ownerType,
				Filter:       filter,
				Order:        ord,
				Limit:        limitPtr,
			})
		}), nil
	})

	// 12. bitrix_list_users
	listUsersTool := mcpproto.NewTool(
		"bitrix_list_users",
		mcpproto.WithTitleAnnotation("List Bitrix24 users"),
		mcpproto.WithDescription("List portal users visible to the OAuth user."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithObject("filter", mcpproto.Description("Optional filter for users")),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(listUsersTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if crm == nil {
				return nil, config.NewAppError("CRM service is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			args := req.GetArguments()
			filter, _ := args["filter"].(map[string]any)
			return crm.ListUsers(ctx, connID, filter)
		}), nil
	})

	// 13. bitrix_prepare_create_item
	prepareCreateTool := mcpproto.NewTool(
		"bitrix_prepare_create_item",
		mcpproto.WithTitleAnnotation("Prepare CRM item creation"),
		mcpproto.WithDescription("Prepare, but do not execute, an item creation. Returns an operation ID requiring explicit confirmation."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId"), mcpproto.Required()),
		mcpproto.WithObject("fields", mcpproto.Description("Fields payload"), mcpproto.Required()),
		mcpproto.WithReadOnlyHintAnnotation(false),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(prepareCreateTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if ops == nil {
				return nil, config.NewAppError("Operation service is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			args := req.GetArguments()
			typeID := bitrix.ToInt(args["entityTypeId"])
			fields, _ := args["fields"].(map[string]any)
			return ops.PrepareCreate(ctx, connID, typeID, fields)
		}), nil
	})

	// 14. bitrix_prepare_update_item
	prepareUpdateTool := mcpproto.NewTool(
		"bitrix_prepare_update_item",
		mcpproto.WithTitleAnnotation("Prepare CRM item update"),
		mcpproto.WithDescription("Prepare, but do not execute, an item update. Returns an operation ID requiring explicit confirmation."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId"), mcpproto.Required()),
		mcpproto.WithInteger("id", mcpproto.Description("CRM item ID"), mcpproto.Required()),
		mcpproto.WithObject("fields", mcpproto.Description("Fields payload"), mcpproto.Required()),
		mcpproto.WithReadOnlyHintAnnotation(false),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(prepareUpdateTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if ops == nil {
				return nil, config.NewAppError("Operation service is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			args := req.GetArguments()
			typeID := bitrix.ToInt(args["entityTypeId"])
			id := bitrix.ToInt(args["id"])
			fields, _ := args["fields"].(map[string]any)
			return ops.PrepareUpdate(ctx, connID, typeID, id, fields)
		}), nil
	})

	// 15. bitrix_prepare_move_item_stage
	prepareMoveTool := mcpproto.NewTool(
		"bitrix_prepare_move_item_stage",
		mcpproto.WithTitleAnnotation("Prepare CRM stage move"),
		mcpproto.WithDescription("Prepare, but do not execute, a stage move. Returns an operation ID requiring explicit confirmation."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId"), mcpproto.Required()),
		mcpproto.WithInteger("id", mcpproto.Description("CRM item ID"), mcpproto.Required()),
		mcpproto.WithString("stageId", mcpproto.Description("Target stage ID"), mcpproto.Required()),
		mcpproto.WithReadOnlyHintAnnotation(false),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(prepareMoveTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if ops == nil {
				return nil, config.NewAppError("Operation service is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			args := req.GetArguments()
			typeID := bitrix.ToInt(args["entityTypeId"])
			id := bitrix.ToInt(args["id"])
			stageID := req.GetString("stageId", "")
			return ops.PrepareMove(ctx, connID, typeID, id, stageID)
		}), nil
	})

	// 16. bitrix_prepare_delete_item
	prepareDeleteTool := mcpproto.NewTool(
		"bitrix_prepare_delete_item",
		mcpproto.WithTitleAnnotation("Prepare CRM item deletion"),
		mcpproto.WithDescription("Prepare, but do not execute, an item deletion. Returns an operation ID requiring explicit confirmation."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId"), mcpproto.Required()),
		mcpproto.WithInteger("id", mcpproto.Description("CRM item ID to delete"), mcpproto.Required()),
		mcpproto.WithReadOnlyHintAnnotation(false),
		mcpproto.WithDestructiveHintAnnotation(true),
	)
	srv.AddTool(prepareDeleteTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if ops == nil {
				return nil, config.NewAppError("Operation service is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			connID, err := connFor(req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			args := req.GetArguments()
			typeID := bitrix.ToInt(args["entityTypeId"])
			id := bitrix.ToInt(args["id"])
			return ops.PrepareDelete(ctx, connID, typeID, id)
		}), nil
	})

	// 17. bitrix_confirm_operation
	confirmTool := mcpproto.NewTool(
		"bitrix_confirm_operation",
		mcpproto.WithTitleAnnotation("Confirm pending operation"),
		mcpproto.WithDescription("Explicitly confirm and execute one pending Bitrix24 write operation."),
		mcpproto.WithString("operationId", mcpproto.Description("Pending operation UUID"), mcpproto.Required()),
		mcpproto.WithReadOnlyHintAnnotation(false),
		mcpproto.WithDestructiveHintAnnotation(true),
	)
	srv.AddTool(confirmTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if ops == nil {
				return nil, config.NewAppError("Operation service is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			opID := req.GetString("operationId", "")
			return ops.Confirm(ctx, opID, authorizedConnectionID)
		}), nil
	})

	// 17. bitrix_cancel_operation
	cancelTool := mcpproto.NewTool(
		"bitrix_cancel_operation",
		mcpproto.WithTitleAnnotation("Cancel pending operation"),
		mcpproto.WithDescription("Cancel one pending operation without calling Bitrix24."),
		mcpproto.WithString("operationId", mcpproto.Description("Pending operation UUID"), mcpproto.Required()),
		mcpproto.WithReadOnlyHintAnnotation(false),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(cancelTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			if ops == nil {
				return nil, config.NewAppError("Operation service is unavailable", "SERVICE_UNAVAILABLE", 503)
			}
			opID := req.GetString("operationId", "")
			return ops.Cancel(ctx, opID, authorizedConnectionID)
		}), nil
	})

	// 18. bitrix_list_pending_operations
	listPendingTool := mcpproto.NewTool(
		"bitrix_list_pending_operations",
		mcpproto.WithTitleAnnotation("List pending operations"),
		mcpproto.WithDescription("List recent prepared operations and their current statuses."),
		mcpproto.WithString("connectionId", mcpproto.Description("Optional connection ID to filter by")),
		mcpproto.WithReadOnlyHintAnnotation(true),
		mcpproto.WithDestructiveHintAnnotation(false),
	)
	srv.AddTool(listPendingTool, func(ctx context.Context, req mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return executeTool(func() (any, error) {
			resolved, err := ResolveConnectionID(authorizedConnectionID, req.GetString("connectionId", ""))
			if err != nil {
				return nil, err
			}
			var filterConn *string
			if resolved != "" {
				filterConn = &resolved
			}
			return st.ListPendingOperations(ctx, filterConn)
		}), nil
	})

	return srv
}

func ResolveConnectionID(authorizedConnectionID, requestedConnectionID string) (string, error) {
	if authorizedConnectionID != "" && requestedConnectionID != "" && requestedConnectionID != authorizedConnectionID {
		return "", config.NewAppError("This MCP token is not authorized for that Bitrix24 connection", "CONNECTION_ACCESS_DENIED", 403)
	}
	if authorizedConnectionID != "" {
		return authorizedConnectionID, nil
	}
	return requestedConnectionID, nil
}

func extractStringSlice(raw any) []string {
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func extractIntSlice(raw any) []int {
	switch v := raw.(type) {
	case []int:
		var out []int
		for _, id := range v {
			if id > 0 {
				out = append(out, id)
			}
		}
		return out
	case []any:
		var out []int
		for _, item := range v {
			if id := bitrix.ToInt(item); id > 0 {
				out = append(out, id)
			}
		}
		return out
	default:
		return nil
	}
}

func extractIncludeDetails(args map[string]any) bool {
	if rawInc, ok := args["includeDetails"].(bool); ok {
		return rawInc
	}
	if rawInc, ok := args["includeProductRows"].(bool); ok {
		return rawInc
	}
	return true
}

func executeTool(work func() (any, error)) *mcpproto.CallToolResult {
	val, err := work()
	if err != nil {
		code := "INTERNAL_ERROR"
		var appErr *config.AppError
		if errors.As(err, &appErr) {
			code = appErr.Code
		}
		errPayload, _ := json.Marshal(map[string]string{
			"error":   code,
			"message": config.SafeErrorMessage(err),
		})
		return &mcpproto.CallToolResult{
			IsError: true,
			Content: []mcpproto.Content{
				mcpproto.NewTextContent(string(errPayload)),
			},
		}
	}
	encoded, _ := json.MarshalIndent(val, "", "  ")
	return &mcpproto.CallToolResult{
		Content: []mcpproto.Content{
			mcpproto.NewTextContent(string(encoded)),
		},
	}
}
