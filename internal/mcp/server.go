package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func NewServer(st *store.Store, authorizedConnectionID string) *mcpserver.MCPServer {
	srv := mcpserver.NewMCPServer(
		"bitrix24-mcp-server",
		"0.3.1",
		mcpserver.WithToolCapabilities(false),
	)

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

	return srv
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
