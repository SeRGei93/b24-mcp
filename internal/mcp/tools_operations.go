package mcp

import (
	"context"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerOperationTools(
	srv *mcpserver.MCPServer,
	st *store.Store,
	ops *bitrix.OperationService,
	authorizedConnectionID string,
	connFor connResolver,
) {
	// 14. bitrix_prepare_create_item
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

	// 15. bitrix_prepare_update_item
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

	// 16. bitrix_prepare_move_item_stage
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

	// 17. bitrix_prepare_delete_item
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

	// 18. bitrix_confirm_operation
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

	// 19. bitrix_cancel_operation
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

	// 20. bitrix_list_pending_operations
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
}
