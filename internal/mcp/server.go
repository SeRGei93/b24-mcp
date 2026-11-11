package mcp

import (
	"encoding/json"
	"errors"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

type connResolver func(requested string) (string, error)

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

	registerDiscoveryTools(srv, st, schema, authorizedConnectionID, connFor)
	registerCRMTools(srv, crm, connFor)
	registerOperationTools(srv, st, ops, authorizedConnectionID, connFor)

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

func withFilterSchema(description string) mcpproto.ToolOption {
	return mcpproto.WithAny(
		"filter",
		mcpproto.Description(description),
		func(s map[string]any) {
			s["anyOf"] = []map[string]any{
				{"type": "object"},
				{"type": "array"},
			}
		},
	)
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

func lookupBoolArg(args map[string]any, keys ...string) (bool, bool) {
	for _, key := range keys {
		if val, ok := args[key].(bool); ok {
			return val, true
		}
	}
	return false, false
}

func extractIncludeDetails(args map[string]any, defaultVal bool) bool {
	if val, ok := lookupBoolArg(args, "includeDetails", "includeProductRows"); ok {
		return val
	}
	return defaultVal
}

func hasExplicitIncludeDetails(args map[string]any) bool {
	_, ok := lookupBoolArg(args, "includeDetails", "includeProductRows")
	return ok
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
