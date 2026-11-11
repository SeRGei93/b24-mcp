package mcp

import (
	"context"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerDiscoveryTools(
	srv *mcpserver.MCPServer,
	st *store.Store,
	schema *bitrix.SchemaDiscovery,
	authorizedConnectionID string,
	connFor connResolver,
) {
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
		mcpproto.WithInteger("entityTypeId", mcpproto.Description("Bitrix CRM entityTypeId (1 lead, 2 deal, 3 contact, 4 company, 7 quote, 31 smart invoice, or an SPA ID)")),
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
			catID := 0
			if catVal, ok := args["categoryId"]; ok && catVal != nil {
				catID = bitrix.ToInt(catVal)
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
}
