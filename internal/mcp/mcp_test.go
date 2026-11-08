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
	if len(toolsRes.Tools) != 20 {
		t.Fatalf("expected all 20 MCP tools to be registered, got %d", len(toolsRes.Tools))
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

	// 5b. Prepare Delete Item and Confirm
	delReq := mcpproto.CallToolRequest{}
	delReq.Params.Name = "bitrix_prepare_delete_item"
	delReq.Params.Arguments = map[string]any{
		"connectionId": conn.ID,
		"entityTypeId": 2,
		"id":           99,
	}
	delRes, err := inProc.CallTool(ctx, delReq)
	if err != nil || delRes.IsError {
		t.Fatalf("bitrix_prepare_delete_item failed: %v, res: %+v", err, delRes)
	}
	delText := delRes.Content[0].(mcpproto.TextContent).Text
	var delObj map[string]any
	_ = json.Unmarshal([]byte(delText), &delObj)
	delOpID, _ := delObj["operation_id"].(string)
	if delOpID == "" {
		t.Fatalf("expected operation_id in del response: %s", delText)
	}

	confDelReq := mcpproto.CallToolRequest{}
	confDelReq.Params.Name = "bitrix_confirm_operation"
	confDelReq.Params.Arguments = map[string]any{"operationId": delOpID}
	confDelRes, err := inProc.CallTool(ctx, confDelReq)
	if err != nil || confDelRes.IsError {
		t.Fatalf("bitrix_confirm_operation for delete failed: %v, res: %+v", err, confDelRes)
	}
	confDelText := confDelRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(confDelText, "completed") {
		t.Errorf("expected completed status in confirm delete response: %s", confDelText)
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

func TestMCPServer_SchemaDiscoveryTools_QuoteAndDictionaries(t *testing.T) {
	ctx := context.Background()

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		path := r.URL.Path
		var bodyMap map[string]any
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &bodyMap)
		}

		if strings.HasSuffix(path, "crm.type.list.json") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"types": []map[string]any{
						{"entityTypeId": 128, "title": "Customer SPA"},
					},
				},
			}), nil
		}
		if strings.HasSuffix(path, "/rest/api/crm.quote.fields") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"fields": map[string]any{
						"TITLE": map[string]any{"type": "string", "title": "Quote Title"},
					},
				},
			}), nil
		}
		if strings.HasSuffix(path, "crm.status.list.json") {
			return jsonResponse(200, map[string]any{
				"result": []map[string]any{
					{"STATUS_ID": "DRAFT", "NAME": "Draft Quote"},
				},
			}), nil
		}
		return jsonResponse(200, map[string]any{"result": map[string]any{}}), nil
	})

	cfg, err := config.Load(map[string]string{
		"APP_URL":              "https://mcp.example.com",
		"DATABASE_PATH":        ":memory:",
		"BITRIX_CLIENT_ID":     "client-id",
		"BITRIX_CLIENT_SECRET": "client-secret",
		"MCP_AUTH_TOKEN":       strings.Repeat("b", 32),
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
		MemberID:       "member-quote-mcp",
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
	initReq.Params.ClientInfo = mcpproto.Implementation{Name: "quote-test-client", Version: "1.0.0"}
	if _, err := inProc.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// 1. bitrix_list_crm_entities
	listEntRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name:      "bitrix_list_crm_entities",
			Arguments: map[string]any{"connectionId": conn.ID},
		},
	})
	if err != nil || listEntRes.IsError {
		t.Fatalf("bitrix_list_crm_entities failed: %v, res: %+v", err, listEntRes)
	}
	listEntText := listEntRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(listEntText, "Quote") || !strings.Contains(listEntText, "QUOTE") {
		t.Errorf("expected Quote entity in bitrix_list_crm_entities: %s", listEntText)
	}

	// 2. bitrix_list_fields for Quote (entityTypeId 7)
	fieldsRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name: "bitrix_list_fields",
			Arguments: map[string]any{
				"connectionId": conn.ID,
				"entityTypeId": 7,
			},
		},
	})
	if err != nil || fieldsRes.IsError {
		t.Fatalf("bitrix_list_fields failed: %v, res: %+v", err, fieldsRes)
	}
	fieldsText := fieldsRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(fieldsText, "TITLE") {
		t.Errorf("expected TITLE in quote fields: %s", fieldsText)
	}

	// 3. bitrix_list_categories for Quote (entityTypeId 7) -> empty without error
	catsRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name: "bitrix_list_categories",
			Arguments: map[string]any{
				"connectionId": conn.ID,
				"entityTypeId": 7,
			},
		},
	})
	if err != nil || catsRes.IsError {
		t.Fatalf("bitrix_list_categories failed: %v, res: %+v", err, catsRes)
	}
	catsText := catsRes.Content[0].(mcpproto.TextContent).Text
	if strings.TrimSpace(catsText) != "[]" && strings.TrimSpace(catsText) != `{"categories":[]}` {
		t.Errorf("expected empty categories for quote: %s", catsText)
	}

	// 4. bitrix_list_stages for Quote (entityTypeId 7)
	stagesRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name: "bitrix_list_stages",
			Arguments: map[string]any{
				"connectionId": conn.ID,
				"entityTypeId": 7,
			},
		},
	})
	if err != nil || stagesRes.IsError {
		t.Fatalf("bitrix_list_stages failed: %v, res: %+v", err, stagesRes)
	}
	stagesText := stagesRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(stagesText, "DRAFT") {
		t.Errorf("expected DRAFT in quote stages: %s", stagesText)
	}

	// 5. bitrix_describe_entity for Quote (entityTypeId 7)
	descRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name: "bitrix_describe_entity",
			Arguments: map[string]any{
				"connectionId": conn.ID,
				"entityTypeId": 7,
			},
		},
	})
	if err != nil || descRes.IsError {
		t.Fatalf("bitrix_describe_entity failed: %v, res: %+v", err, descRes)
	}
	descText := descRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(descText, "Quote") || !strings.Contains(descText, "TITLE") {
		t.Errorf("expected Quote and TITLE in describe output: %s", descText)
	}

	// 6. bitrix_discover_portal
	discRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name:      "bitrix_discover_portal",
			Arguments: map[string]any{"connectionId": conn.ID},
		},
	})
	if err != nil || discRes.IsError {
		t.Fatalf("bitrix_discover_portal failed: %v, res: %+v", err, discRes)
	}
	discText := discRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(discText, "Customer SPA") {
		t.Errorf("expected Customer SPA in discover portal: %s", discText)
	}

	// 7. bitrix_refresh_schema_cache
	refRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name:      "bitrix_refresh_schema_cache",
			Arguments: map[string]any{"connectionId": conn.ID},
		},
	})
	if err != nil || refRes.IsError {
		t.Fatalf("bitrix_refresh_schema_cache failed: %v, res: %+v", err, refRes)
	}
}

func TestMCPServer_ReadEngineTools_REST3(t *testing.T) {
	ctx := context.Background()

	var lastListBody map[string]any
	var lastGetBody map[string]any

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		path := r.URL.Path
		var bodyMap map[string]any
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &bodyMap)
		}

		if strings.HasSuffix(path, "/rest/api/crm.deal.list") {
			lastListBody = bodyMap
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"items": []map[string]any{
						{"id": 1, "title": "MCP Deal 1", "stage": map[string]any{"name": "Initial"}},
						{"id": 2, "title": "MCP Deal 2", "stage": map[string]any{"name": "Won"}},
					},
				},
				"total": 2,
			}), nil
		}
		if strings.HasSuffix(path, "/rest/api/crm.deal.get") {
			lastGetBody = bodyMap
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"item": map[string]any{
						"id":        42,
						"title":     "MCP Single Deal",
						"contactId": 7,
						"stage":     map[string]any{"name": "Формируется к отправке"},
					},
				},
			}), nil
		}
		if strings.HasSuffix(path, "/rest/api/crm.deal.aggregate") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"count": map[string]any{"id": 42},
				},
			}), nil
		}
		if strings.HasSuffix(path, "/rest/api/crm.productrow.list") || strings.HasSuffix(path, "crm.item.productrow.list.json") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"productRows": []map[string]any{
						{"id": 901, "ownerId": 1, "ownerType": "D", "productName": "Cloud Hosting", "price": 49.99, "quantity": 1},
					},
				},
			}), nil
		}
		if strings.HasSuffix(path, "batch.json") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"result": map[string]any{
						"1": map[string]any{
							"productRows": []map[string]any{
								{"id": 901, "ownerId": 1, "ownerType": "D", "productName": "Cloud Hosting", "price": 49.99, "quantity": 1},
							},
						},
						"42": map[string]any{
							"productRows": []map[string]any{
								{"id": 902, "ownerId": 42, "ownerType": "D", "productName": "Товар в заказе 42", "price": 61.08, "quantity": 2},
							},
						},
						"pay_42": []map[string]any{
							{"id": 11, "accountNumber": "192522/1", "paid": "Y", "sum": 61.08, "currency": "BYN", "paySystemName": "QR-платежи «Оплати»"},
						},
						"deliv_42": []map[string]any{
							{"id": 22, "accountNumber": "192522/2", "deducted": "N", "priceDelivery": 0, "currency": "BYN", "deliveryName": "пр. Независимости 95"},
						},
						"full_42": map[string]any{
							"item": map[string]any{
								"id":                   42,
								"title":                "MCP Single Deal",
								"ufCrm_DeliveryOption": "пр. Независимости 95",
								"comments":             "Не перезванивать для подтверждения заказа",
							},
						},
						"contact_7": map[string]any{
							"item": map[string]any{
								"id":       7,
								"name":     "Маргарита",
								"lastName": "Шевченко",
								"fm": []map[string]any{
									{"typeId": "PHONE", "value": "+375 29 210-97-95"},
									{"typeId": "EMAIL", "value": "m.shevchenko.1099@gmail.com"},
								},
							},
						},
					},
					"result_error": map[string]any{},
				},
			}), nil
		}
		return jsonResponse(200, map[string]any{"result": map[string]any{}}), nil
	})

	cfg, err := config.Load(map[string]string{
		"APP_URL":              "https://mcp.example.com",
		"DATABASE_PATH":        ":memory:",
		"BITRIX_CLIENT_ID":     "client-id",
		"BITRIX_CLIENT_SECRET": "client-secret",
		"MCP_AUTH_TOKEN":       strings.Repeat("c", 32),
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
		MemberID:       "member-read-mcp",
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
	initReq.Params.ClientInfo = mcpproto.Implementation{Name: "read-test-client", Version: "1.0.0"}
	if _, err := inProc.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// 1. bitrix_search_items with REST 3.0 expression slice filter and page: 2
	searchRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name: "bitrix_search_items",
			Arguments: map[string]any{
				"connectionId": conn.ID,
				"entityTypeId": 2,
				"filter":       []any{[]any{"stageId", "=", "NEW"}},
				"limit":        10,
				"page":         2,
			},
		},
	})
	if err != nil || searchRes.IsError {
		t.Fatalf("bitrix_search_items failed: %v, res: %+v", err, searchRes)
	}
	searchText := searchRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(searchText, "MCP Deal 1") {
		t.Errorf("expected MCP Deal 1 in search response: %s", searchText)
	}
	if pag, ok := lastListBody["pagination"].(map[string]any); !ok || (pag["page"] != float64(2) && pag["page"] != 2) {
		t.Errorf("expected page: 2 in outbound search pagination, got: %v", lastListBody["pagination"])
	}
	if fSlice, ok := lastListBody["filter"].([]any); !ok || len(fSlice) != 1 {
		t.Errorf("expected REST 3.0 expression slice filter forwarded, got: %v", lastListBody["filter"])
	}

	// 2. bitrix_get_item (single with custom select)
	getItemRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name: "bitrix_get_item",
			Arguments: map[string]any{
				"connectionId": conn.ID,
				"entityTypeId": 2,
				"id":           42,
				"select":       []any{"title", "stage.name"},
			},
		},
	})
	if err != nil || getItemRes.IsError {
		t.Fatalf("bitrix_get_item single failed: %v, res: %+v", err, getItemRes)
	}
	getItemText := getItemRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(getItemText, "MCP Single Deal") {
		t.Errorf("expected MCP Single Deal in get response: %s", getItemText)
	}
	if selSlice, ok := lastGetBody["select"].([]any); !ok || len(selSlice) != 3 || selSlice[0] != "id" {
		t.Errorf("expected custom select ['id', 'title', 'stage.name'] forwarded to crm.deal.get, got: %v", lastGetBody["select"])
	}

	// 2b. bitrix_get_item (single without select) returns full order details (productRows, payments, deliveries, custom fields, contact fm)
	getFullOrderRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name: "bitrix_get_item",
			Arguments: map[string]any{
				"connectionId": conn.ID,
				"entityTypeId": 2,
				"id":           42,
			},
		},
	})
	if err != nil || getFullOrderRes.IsError {
		t.Fatalf("bitrix_get_item full order failed: %v, res: %+v", err, getFullOrderRes)
	}
	fullOrderText := getFullOrderRes.Content[0].(mcpproto.TextContent).Text
	for _, expected := range []string{
		"Товар в заказе 42",
		"QR-платежи «Оплати»",
		"пр. Независимости 95",
		"ufCrm_DeliveryOption",
		"+375 29 210-97-95",
	} {
		if !strings.Contains(fullOrderText, expected) {
			t.Errorf("expected %q in full order response, got: %s", expected, fullOrderText)
		}
	}

	// 3. bitrix_get_item (multiple ids with custom select)
	getItemsRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name: "bitrix_get_item",
			Arguments: map[string]any{
				"connectionId": conn.ID,
				"entityTypeId": 2,
				"ids":          []int{1, 2},
				"select":       []any{"title", "stage.name"},
			},
		},
	})
	if err != nil || getItemsRes.IsError {
		t.Fatalf("bitrix_get_item multiple failed: %v, res: %+v", err, getItemsRes)
	}
	getItemsText := getItemsRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(getItemsText, "MCP Deal 1") {
		t.Errorf("expected MCP Deal 1 in getItems response: %s", getItemsText)
	}
	if selSlice, ok := lastListBody["select"].([]any); !ok || len(selSlice) != 3 || selSlice[0] != "id" {
		t.Errorf("expected custom select ['id', 'title', 'stage.name'] forwarded to bulk crm.deal.list, got: %v", lastListBody["select"])
	}

	// 4. bitrix_count_items
	countRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name: "bitrix_count_items",
			Arguments: map[string]any{
				"connectionId": conn.ID,
				"entityTypeId": 2,
			},
		},
	})
	if err != nil || countRes.IsError {
		t.Fatalf("bitrix_count_items failed: %v, res: %+v", err, countRes)
	}
	countText := countRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(countText, "42") {
		t.Errorf("expected count 42 in count response: %s", countText)
	}

	// 5. bitrix_list_product_rows
	prodRes, err := inProc.CallTool(ctx, mcpproto.CallToolRequest{
		Params: mcpproto.CallToolParams{
			Name: "bitrix_list_product_rows",
			Arguments: map[string]any{
				"connectionId": conn.ID,
				"dealId":       1,
			},
		},
	})
	if err != nil || prodRes.IsError {
		t.Fatalf("bitrix_list_product_rows failed: %v, res: %+v", err, prodRes)
	}
	prodText := prodRes.Content[0].(mcpproto.TextContent).Text
	if !strings.Contains(prodText, "Cloud Hosting") {
		t.Errorf("expected Cloud Hosting in product rows response: %s", prodText)
	}
}
