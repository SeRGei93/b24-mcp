package mcp

import (
	"context"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
	"github.com/SeRGei93/b24-mcp/internal/config"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func extractStringMap(v any) map[string]string {
	out := make(map[string]string)
	if m, ok := v.(map[string]any); ok {
		for k, val := range m {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

func registerCRMTools(
	srv *mcpserver.MCPServer,
	crm *bitrix.CrmService,
	connFor connResolver,
) {
	// 9. bitrix_search_items
	searchItemsTool := mcpproto.NewTool(
		"bitrix_search_items",
		mcpproto.WithTitleAnnotation("Search CRM items"),
		mcpproto.WithDescription("Search leads, deals, contacts, companies, quotes, smart invoices, or SPA items with Bitrix filters and single-call REST 3.0 pagination up to 1000 items. Pass includeDetails: true to also batch-fetch productRows, payments, deliveries, and full custom fields."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId (1 lead, 2 deal, 3 contact, 4 company, 7 quote, 31 smart invoice, or an SPA ID)"), mcpproto.Required()),
		withFilterSchema("Bitrix filter object or REST 3.0 filter expression array"),
		mcpproto.WithArray("select", mcpproto.Description("Fields to select (supports relational dot-notation such as stage.name, company.title)")),
		mcpproto.WithObject("order", mcpproto.Description("Order map (e.g. {'id': 'ASC'})")),
		mcpproto.WithInteger("limit", mcpproto.Description("Maximum items to return (up to 1000)"), mcpproto.DefaultNumber(100)),
		mcpproto.WithInteger("page", mcpproto.Description("Page number for pagination (1-based)"), mcpproto.DefaultNumber(1)),
		mcpproto.WithBoolean("includeDetails", mcpproto.Description("Batch-fetch product rows, payments, deliveries, and full custom fields for returned items (default false)"), mcpproto.DefaultBool(false)),
		mcpproto.WithBoolean("includeProductRows", mcpproto.Description("Alias for includeDetails (default false)"), mcpproto.DefaultBool(false)),
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
			limit := bitrix.ToInt(args["limit"])
			if limit <= 0 {
				limit = 100
			}
			page := bitrix.ToInt(args["page"])
			if page <= 0 {
				page = 1
			}

			order := extractStringMap(args["order"])

			return crm.SearchItems(ctx, bitrix.SearchItemsInput{
				ConnectionID:   connID,
				EntityTypeID:   bitrix.ToInt(args["entityTypeId"]),
				Filter:         args["filter"],
				Select:         extractStringSlice(args["select"]),
				Order:          order,
				Limit:          limit,
				Page:           page,
				IncludeDetails: extractIncludeDetails(args, false),
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
		withFilterSchema("Bitrix filter object or REST 3.0 filter expression array"),
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
			count, err := crm.CountItems(ctx, bitrix.CountItemsInput{
				ConnectionID: connID,
				EntityTypeID: bitrix.ToInt(args["entityTypeId"]),
				Filter:       args["filter"],
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
		mcpproto.WithDescription("Get one CRM/SPA item by id, or up to 1000 items in a single call by supplying ids. Automatically includes productRows, payments, deliveries, and all custom fields by default for single items and batches up to 50."),
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
				ConnectionID:           connID,
				EntityTypeID:           bitrix.ToInt(args["entityTypeId"]),
				ID:                     bitrix.ToInt(args["id"]),
				IDs:                    extractIntSlice(args["ids"]),
				Select:                 extractStringSlice(args["select"]),
				IncludeDetails:         extractIncludeDetails(args, true),
				IncludeDetailsExplicit: hasExplicitIncludeDetails(args),
			})
		}), nil
	})

	// 12. bitrix_list_product_rows
	listProductRowsTool := mcpproto.NewTool(
		"bitrix_list_product_rows",
		mcpproto.WithTitleAnnotation("List CRM item product rows (batch)"),
		mcpproto.WithDescription("Batch-fetch product rows (line items) for one or multiple deals/CRM items at once via Bitrix24 REST 3.0 crm.<slug>.productrow.list or legacy batch fallback. Pass dealIds/ownerIds array to fetch product rows for up to 250 deals in a single call instead of looping."),
		mcpproto.WithString("connectionId", mcpproto.Description("ID returned by bitrix_list_connections after OAuth authorization"), mcpproto.Required()),
		mcpproto.WithArray("dealIds", mcpproto.Description("Array of Deal IDs to fetch product rows for in one batch request (preferred over calling one by one)")),
		mcpproto.WithArray("ownerIds", mcpproto.Description("Array of CRM item IDs to fetch product rows for in one batch request")),
		mcpproto.WithInteger("ownerId", mcpproto.Description("Single CRM item ID (alias: dealId)")),
		mcpproto.WithInteger("dealId", mcpproto.Description("Single Deal ID (prefer dealIds array when checking multiple deals)")),
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId (default: 2 for Deal)"), mcpproto.DefaultNumber(bitrix.EntityTypeDeal)),
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

			ownerIDs := extractIntSlice(args["dealIds"])
			if len(ownerIDs) == 0 {
				ownerIDs = extractIntSlice(args["ownerIds"])
			}

			var singleOwnerID *int
			for _, key := range []string{"ownerId", "dealId"} {
				if val, ok := args[key]; ok && val != nil {
					if id := bitrix.ToInt(val); id > 0 {
						singleOwnerID = &id
						break
					}
				}
			}

			if len(ownerIDs) == 0 && singleOwnerID == nil {
				return nil, config.NewAppError("Supply dealIds, ownerIds, dealId, or ownerId", "MISSING_OWNER_ID", 400)
			}

			typeID := bitrix.ToInt(args["entityTypeId"])
			if typeID <= 0 {
				typeID = bitrix.EntityTypeDeal
			}

			ownerType, _ := args["ownerType"].(string)
			filter, _ := args["filter"].(map[string]any)
			ord := extractStringMap(args["order"])
			var limitPtr *int
			if limVal, ok := args["limit"]; ok && limVal != nil {
				if l := bitrix.ToInt(limVal); l > 0 {
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

	// 13. bitrix_list_users
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
}
