package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/mcp"
	"github.com/SeRGei93/b24-mcp/internal/store"
	"github.com/mark3labs/mcp-go/client"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
)

const testMCPKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestMCPServer_ListConnectionsAndIsolation(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewStore(":memory:", testMCPKeyHex)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	first, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "authorized.bitrix24.com",
		MemberID:       "member-1",
		BitrixUserID:   "1",
		ClientEndpoint: "https://authorized.bitrix24.com/rest/",
		AccessToken:    "must-not-leak-access",
		RefreshToken:   "must-not-leak-refresh",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("failed to seed first connection: %v", err)
	}
	_, err = st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "other.bitrix24.com",
		MemberID:       "member-2",
		BitrixUserID:   "2",
		ClientEndpoint: "https://other.bitrix24.com/rest/",
		AccessToken:    "other-secret-access",
		RefreshToken:   "other-secret-refresh",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("failed to seed second connection: %v", err)
	}

	// 1. Unscoped MCP server returns both connections without secrets
	srv := mcp.NewServer(st, "")
	inProc, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("NewInProcessClient failed: %v", err)
	}
	defer inProc.Close()
	if err := inProc.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	initReq := mcpproto.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpproto.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpproto.Implementation{Name: "test-client", Version: "1.0.0"}
	if _, err := inProc.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	toolsRes, err := inProc.ListTools(ctx, mcpproto.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	foundList := false
	for _, tool := range toolsRes.Tools {
		if tool.Name == "bitrix_list_connections" {
			foundList = true
		}
	}
	if !foundList {
		t.Fatalf("expected bitrix_list_connections tool to be registered")
	}

	callReq := mcpproto.CallToolRequest{}
	callReq.Params.Name = "bitrix_list_connections"
	callRes, err := inProc.CallTool(ctx, callReq)
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	serialized, _ := json.Marshal(callRes)
	str := string(serialized)
	if !strings.Contains(str, "authorized.bitrix24.com") || !strings.Contains(str, "other.bitrix24.com") {
		t.Errorf("expected both domains in unscoped response: %s", str)
	}
	if strings.Contains(str, "must-not-leak-access") || strings.Contains(str, "must-not-leak-refresh") {
		t.Errorf("leaked secret tokens in MCP tool response: %s", str)
	}

	// 2. Scoped MCP server only returns the authorized connection
	scopedSrv := mcp.NewServer(st, first.ID)
	scopedClient, err := client.NewInProcessClient(scopedSrv)
	if err != nil {
		t.Fatalf("NewInProcessClient failed: %v", err)
	}
	defer scopedClient.Close()
	if err := scopedClient.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if _, err := scopedClient.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	scopedRes, err := scopedClient.CallTool(ctx, callReq)
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	scopedBytes, _ := json.Marshal(scopedRes)
	scopedStr := string(scopedBytes)
	if !strings.Contains(scopedStr, "authorized.bitrix24.com") {
		t.Errorf("expected authorized.bitrix24.com in scoped response: %s", scopedStr)
	}
	if strings.Contains(scopedStr, "other.bitrix24.com") {
		t.Errorf("expected other.bitrix24.com to be filtered out: %s", scopedStr)
	}
}
