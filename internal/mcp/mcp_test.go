package mcp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
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
	srv := mcp.NewServer(st, nil, nil, nil, "")
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
	if len(toolsRes.Tools) != 18 {
		t.Fatalf("expected all 18 MCP tools to be registered, got %d", len(toolsRes.Tools))
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
	scopedSrv := mcp.NewServer(st, nil, nil, nil, first.ID)
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

	// 3. ResolveConnectionID denies access to another connection
	if _, err := mcp.ResolveConnectionID(first.ID, "00000000-0000-4000-8000-000000000002"); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("expected CONNECTION_ACCESS_DENIED when requesting another connection, got %v", err)
	}
	resolved, err := mcp.ResolveConnectionID(first.ID, first.ID)
	if err != nil || resolved != first.ID {
		t.Errorf("expected ResolveConnectionID to allow matching connection, got %q err=%v", resolved, err)
	}
}

func TestMcpOAuthService_LoopbackAndPKCEAndRotation(t *testing.T) {
	ctx := context.Background()

	// 1. RedirectURIMatches & IsSafeOAuthRedirectURI
	if !mcp.RedirectURIMatches("http://127.0.0.1:49152/callback", "http://127.0.0.1:3000/callback") {
		t.Errorf("expected loopback port flexibility on 127.0.0.1")
	}
	if mcp.RedirectURIMatches("http://localhost:49152/callback", "http://127.0.0.1:3000/callback") {
		t.Errorf("expected localhost vs 127.0.0.1 mismatch")
	}
	if mcp.RedirectURIMatches("https://client.example/callback", "https://client.example/other") {
		t.Errorf("expected path mismatch to fail")
	}
	if !mcp.IsSafeOAuthRedirectURI("http://[::1]:43123/callback") {
		t.Errorf("expected IPv6 loopback redirect URI to be safe")
	}
	if mcp.IsSafeOAuthRedirectURI("http://public.example.com/callback") {
		t.Errorf("expected non-loopback HTTP redirect URI to be rejected")
	}

	cfg, err := config.Load(map[string]string{
		"APP_URL":              "https://mcp.example.com",
		"DATABASE_PATH":        ":memory:",
		"BITRIX_CLIENT_ID":     "client-id",
		"BITRIX_CLIENT_SECRET": "client-secret",
		"MCP_AUTH_TOKEN":       strings.Repeat("a", 32),
		"TOKEN_ENCRYPTION_KEY": testMCPKeyHex,
	})
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	st, err := store.NewStore(cfg.DatabasePath, cfg.TokenEncryptionKey)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-1",
		BitrixUserID:   "1",
		ClientEndpoint: "https://example.bitrix24.com/rest/",
		AccessToken:    "acc",
		RefreshToken:   "ref",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("UpsertConnection failed: %v", err)
	}

	svc := mcp.NewOAuthService(cfg, st)

	// 2. Reject unsafe redirect registration
	if _, err := svc.RegisterClient(ctx, mcp.ClientRegistrationInput{
		RedirectURIs: []string{"http://public.example.com/callback"},
	}); err == nil {
		t.Fatalf("expected unsafe redirect registration to fail")
	}

	// 3. Register client, begin authorization, complete Bitrix auth, and reject mismatched PKCE verifier
	reg, err := svc.RegisterClient(ctx, mcp.ClientRegistrationInput{
		RedirectURIs: []string{"https://client.example.com/callback"},
		ClientName:   "Security test",
	})
	if err != nil {
		t.Fatalf("RegisterClient failed: %v", err)
	}

	verifier := strings.Repeat("z", 64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	pending, err := svc.BeginAuthorization(ctx, mcp.AuthorizationInput{
		ResponseType:        "code",
		ClientID:            reg.ClientID,
		RedirectURI:         "https://client.example.com/callback",
		CodeChallenge:       challenge,
		CodeChallengeMethod: "S256",
		Resource:            "https://mcp.example.com/mcp",
	})
	if err != nil {
		t.Fatalf("BeginAuthorization failed: %v", err)
	}

	cbURLStr, err := svc.CompleteBitrixAuthorization(ctx, pending.RequestHash, conn.ID)
	if err != nil {
		t.Fatalf("CompleteBitrixAuthorization failed: %v", err)
	}
	cbURL, err := url.Parse(cbURLStr)
	if err != nil {
		t.Fatalf("invalid callback URL: %v", err)
	}
	authCode := cbURL.Query().Get("code")

	if _, err := svc.ExchangeAuthorizationCode(ctx, mcp.AuthorizationCodeExchangeInput{
		ClientID:     reg.ClientID,
		Code:         authCode,
		CodeVerifier: strings.Repeat("x", 64),
		RedirectURI:  "https://client.example.com/callback",
	}); err == nil {
		t.Fatalf("expected mismatched PKCE verifier to be rejected")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonResponse(status int, body any) *http.Response {
	b, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
	}
}

func TestMCPServer_AllToolsExecution(t *testing.T) {
	ctx := context.Background()

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "crm.type.list.json"):
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"types": []map[string]any{
						{"id": 128, "entityTypeId": 128, "title": "Invoice SPA"},
					},
				},
			}), nil
		case strings.HasSuffix(r.URL.Path, "crm.item.list.json"):
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"items": []map[string]any{
						{"id": 1, "title": "Test Item 1"},
					},
				},
				"total": 1,
			}), nil
		case strings.HasSuffix(r.URL.Path, "crm.item.get.json"):
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"item": map[string]any{"id": 1, "title": "Test Item 1"},
				},
			}), nil
		case strings.HasSuffix(r.URL.Path, "crm.item.add.json"):
			return jsonResponse(200, map[string]any{"result": map[string]any{"item": map[string]any{"id": 99}}}), nil
		default:
			return jsonResponse(200, map[string]any{"result": map[string]any{}}), nil
		}
	})

	cfg, err := config.Load(map[string]string{
		"APP_URL":              "https://mcp.example.com",
		"DATABASE_PATH":        ":memory:",
		"BITRIX_CLIENT_ID":     "client-id",
		"BITRIX_CLIENT_SECRET": "client-secret",
		"MCP_AUTH_TOKEN":       strings.Repeat("a", 32),
		"TOKEN_ENCRYPTION_KEY": testMCPKeyHex,
	})
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	st, err := store.NewStore(cfg.DatabasePath, cfg.TokenEncryptionKey)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-mcp",
		BitrixUserID:   "1",
		ClientEndpoint: "https://example.bitrix24.com/rest/",
		AccessToken:    "acc",
		RefreshToken:   "ref",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeWriteWithConfirmation,
	})
	if err != nil {
		t.Fatalf("UpsertConnection failed: %v", err)
	}

	httpClient := &http.Client{Transport: rt, Timeout: 2 * time.Second}
	oauth := bitrix.NewOAuthService(cfg, st, httpClient)
	tokens := bitrix.NewTokenManager(st, oauth, nil)
	rc := bitrix.NewRestClient(cfg, tokens, nil, httpClient)
	schema := bitrix.NewSchemaDiscovery(cfg, st, rc)
	crmSvc := bitrix.NewCrmService(cfg, rc)
	guard := bitrix.NewPolicyGuard()
	ops := bitrix.NewOperationService(cfg, st, guard, rc)

	srv := mcp.NewServer(st, schema, crmSvc, ops, "")
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

	// 1. Discover Portal
	discReq := mcpproto.CallToolRequest{}
	discReq.Params.Name = "bitrix_discover_portal"
	discReq.Params.Arguments = map[string]any{"connectionId": conn.ID}
	discRes, err := inProc.CallTool(ctx, discReq)
	if err != nil || discRes.IsError {
		t.Fatalf("bitrix_discover_portal failed: %v, res: %+v", err, discRes)
	}
	discText := discRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(discText, "Invoice SPA") {
		t.Errorf("expected Invoice SPA in discover response, got: %s", discText)
	}

	// 2. Search Items
	searchReq := mcpproto.CallToolRequest{}
	searchReq.Params.Name = "bitrix_search_items"
	searchReq.Params.Arguments = map[string]any{
		"connectionId": conn.ID,
		"entityTypeId": 2,
		"limit":        10,
	}
	searchRes, err := inProc.CallTool(ctx, searchReq)
	if err != nil || searchRes.IsError {
		t.Fatalf("bitrix_search_items failed: %v, res: %+v", err, searchRes)
	}
	searchText := searchRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(searchText, "Test Item 1") {
		t.Errorf("expected Test Item 1 in search response, got: %s", searchText)
	}

	// 3. Prepare Create Item
	prepReq := mcpproto.CallToolRequest{}
	prepReq.Params.Name = "bitrix_prepare_create_item"
	prepReq.Params.Arguments = map[string]any{
		"connectionId": conn.ID,
		"entityTypeId": 2,
		"fields":       map[string]any{"TITLE": "Brand New Deal"},
	}
	prepRes, err := inProc.CallTool(ctx, prepReq)
	if err != nil || prepRes.IsError {
		t.Fatalf("bitrix_prepare_create_item failed: %v, res: %+v", err, prepRes)
	}
	prepText := prepRes.Content[0].(mcpproto.TextContent).Text
	var prepObj map[string]any
	_ = json.Unmarshal([]byte(prepText), &prepObj)
	opID, _ := prepObj["operation_id"].(string)
	if opID == "" {
		t.Fatalf("expected operation_id in prep response: %s", prepText)
	}

	// 4. List Pending Operations
	listReq := mcpproto.CallToolRequest{}
	listReq.Params.Name = "bitrix_list_pending_operations"
	listReq.Params.Arguments = map[string]any{"connectionId": conn.ID}
	listRes, err := inProc.CallTool(ctx, listReq)
	if err != nil || listRes.IsError {
		t.Fatalf("bitrix_list_pending_operations failed: %v, res: %+v", err, listRes)
	}
	listText := listRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(listText, opID) {
		t.Errorf("expected opID %s in list pending operations: %s", opID, listText)
	}

	// 5. Confirm Operation
	confReq := mcpproto.CallToolRequest{}
	confReq.Params.Name = "bitrix_confirm_operation"
	confReq.Params.Arguments = map[string]any{"operationId": opID}
	confRes, err := inProc.CallTool(ctx, confReq)
	if err != nil || confRes.IsError {
		t.Fatalf("bitrix_confirm_operation failed: %v, res: %+v", err, confRes)
	}
	confText := confRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(confText, "completed") {
		t.Errorf("expected completed status in confirm response: %s", confText)
	}

	// 6. Test Scoped Server isolates connection access
	scopedSrv := mcp.NewServer(st, schema, crmSvc, ops, conn.ID)
	scopedInProc, err := client.NewInProcessClient(scopedSrv)
	if err != nil {
		t.Fatalf("NewInProcessClient failed: %v", err)
	}
	defer scopedInProc.Close()
	if err := scopedInProc.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if _, err := scopedInProc.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Calling with another connection ID should be rejected
	badReq := mcpproto.CallToolRequest{}
	badReq.Params.Name = "bitrix_discover_portal"
	badReq.Params.Arguments = map[string]any{"connectionId": "wrong-connection-id"}
	badRes, err := scopedInProc.CallTool(ctx, badReq)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !badRes.IsError {
		t.Fatalf("expected error when accessing unauthorized connection")
	}
	badText := badRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(badText, "CONNECTION_ACCESS_DENIED") {
		t.Errorf("expected CONNECTION_ACCESS_DENIED error code, got: %s", badText)
	}
}

