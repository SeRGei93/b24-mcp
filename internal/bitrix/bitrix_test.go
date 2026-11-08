package bitrix_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
)

const testBitrixKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonResponse(status int, body any) *http.Response {
	b, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(b))),
	}
}

func newTestEnv(t *testing.T, portalDomain, clientID string, rt http.RoundTripper) (*config.Config, *store.Store, *bitrix.OAuthService, *bitrix.TokenManager) {
	t.Helper()
	if clientID == "" {
		clientID = "client-id"
	}
	cfg, err := config.Load(map[string]string{
		"APP_URL":              "https://mcp.example.com",
		"DATABASE_PATH":        ":memory:",
		"BITRIX_CLIENT_ID":     clientID,
		"BITRIX_CLIENT_SECRET": "client-secret",
		"BITRIX_PORTAL_DOMAIN": portalDomain,
		"MCP_AUTH_TOKEN":       strings.Repeat("a", 32),
		"TOKEN_ENCRYPTION_KEY": testBitrixKeyHex,
	})
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	st, err := store.NewStore(cfg.DatabasePath, cfg.TokenEncryptionKey)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background(), nil); err != nil {
		t.Fatalf("failed to migrate store: %v", err)
	}
	httpClient := &http.Client{Transport: rt, Timeout: 2 * time.Second}
	logger := config.NewLogger(nil, "silent")
	oauth := bitrix.NewOAuthService(cfg, st, httpClient)
	tm := bitrix.NewTokenManager(st, oauth, logger)
	return cfg, st, oauth, tm
}

func TestNormalizePortalDomain(t *testing.T) {
	cases := map[string]string{
		"Example.BITRIX24.com":          "example.bitrix24.com",
		"https://example.bitrix24.com/": "example.bitrix24.com",
		"example.bitrix24.com.br":       "example.bitrix24.com.br",
	}
	for input, want := range cases {
		got, err := bitrix.NormalizePortalDomain(input, nil)
		if err != nil || got != want {
			t.Errorf("NormalizePortalDomain(%q) = %q, err=%v; want %q", input, got, err, want)
		}
	}

	// Custom operator-allowlisted domain
	gotCustom, err := bitrix.NormalizePortalDomain("https://roast24.by/", []string{"roast24.by"})
	if err != nil || gotCustom != "roast24.by" {
		t.Errorf("expected custom domain roast24.by, got %q err=%v", gotCustom, err)
	}
	for _, rejected := range []struct {
		input   string
		allowed []string
	}{
		{"other.hub-bnk.com", []string{"crm.hub-bnk.com"}},
		{"other.bitrix24.com", []string{"example.bitrix24.com"}},
		{"other.bitrix24.com", []string{"crm.hub-bnk.com"}},
		{"crm.hub-bnk.com", nil},
	} {
		if _, err := bitrix.NormalizePortalDomain(rejected.input, rejected.allowed); err == nil {
			t.Errorf("expected %q with allowed=%v to be rejected", rejected.input, rejected.allowed)
		}
	}

	unsafe := []string{
		"localhost",
		"127.0.0.1",
		"https://example.com",
		"bitrix24.com",
		"bitrix24.attacker.com",
		"sub.bitrix24.attacker.com",
		"http://example.bitrix24.com",
		"https://example.bitrix24.com:8443",
		"https://example.bitrix24.com/rest/",
		"https://example.bitrix24.com@evil.example",
	}
	for _, u := range unsafe {
		if _, err := bitrix.NormalizePortalDomain(u, nil); err == nil {
			t.Errorf("expected unsafe portal value %q to be rejected", u)
		}
	}
}

func TestValidateClientEndpoint(t *testing.T) {
	portal, err := bitrix.ValidateClientEndpoint("https://example.bitrix24.com/rest/", "example.bitrix24.com", nil)
	if err != nil || portal.Domain != "example.bitrix24.com" || portal.Endpoint != "https://example.bitrix24.com/rest/" {
		t.Fatalf("unexpected ValidateClientEndpoint result: %+v err=%v", portal, err)
	}

	if _, err := bitrix.ValidateClientEndpoint("https://other.bitrix24.com/rest/", "example.bitrix24.com", nil); err == nil {
		t.Errorf("expected mismatched portal endpoint to fail")
	}
	if _, err := bitrix.ValidateClientEndpoint("https://evil.example/rest/", "", nil); err == nil {
		t.Errorf("expected non-Bitrix24 endpoint to fail")
	}
	if _, err := bitrix.ValidateClientEndpoint("https://example.bitrix24.com/oauth/token/", "", nil); err == nil {
		t.Errorf("expected non-REST endpoint to fail")
	}

	custom, err := bitrix.ValidateClientEndpoint("https://crm.hub-bnk.com/rest/", "crm.hub-bnk.com", []string{"https://crm.hub-bnk.com/"})
	if err != nil || custom.Domain != "crm.hub-bnk.com" || custom.Endpoint != "https://crm.hub-bnk.com/rest/" {
		t.Errorf("expected allowlisted custom endpoint to pass, got %+v err=%v", custom, err)
	}
}

func TestOAuthService_CallbackAndCustomPortal(t *testing.T) {
	ctx := context.Background()
	var requestedURLs []string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requestedURLs = append(requestedURLs, r.URL.String())
		if r.URL.Hostname() == "oauth.bitrix.info" {
			return jsonResponse(200, map[string]any{
				"access_token":    "access-secret",
				"client_endpoint": "https://crm.hub-bnk.com/rest/",
				"domain":          "oauth.bitrix.info",
				"expires_in":      3600,
				"member_id":       "portal-member",
				"refresh_token":   "refresh-secret",
				"scope":           "crm",
			}), nil
		}
		return jsonResponse(200, map[string]any{
			"result": map[string]any{"ID": "7"},
		}), nil
	})

	_, st, oauth, _ := newTestEnv(t, "https://crm.hub-bnk.com/", "client-id", rt)

	authURLStr, err := oauth.CreateAuthorizationURL(ctx, "https://crm.hub-bnk.com/", config.ModeReadOnly, "")
	if err != nil {
		t.Fatalf("CreateAuthorizationURL failed: %v", err)
	}
	authURL, err := url.Parse(authURLStr)
	if err != nil {
		t.Fatalf("invalid auth URL: %v", err)
	}
	if authURL.Host != "crm.hub-bnk.com" || authURL.Path != "/oauth/authorize/" {
		t.Errorf("unexpected auth URL: %s", authURLStr)
	}
	stateParam := authURL.Query().Get("state")
	if stateParam == "" {
		t.Fatalf("expected state query parameter")
	}

	res, err := oauth.HandleCallbackWithContext(ctx, bitrix.CallbackInput{
		Code:     "authorization-code",
		State:    stateParam,
		Domain:   "crm.hub-bnk.com",
		MemberID: "portal-member",
	})
	if err != nil {
		t.Fatalf("HandleCallbackWithContext failed: %v", err)
	}
	if res.Connection.Domain != "crm.hub-bnk.com" || res.Connection.BitrixUserID != "7" || res.Connection.AccessToken != "access-secret" {
		t.Errorf("unexpected connection: %+v", res.Connection)
	}
	if len(requestedURLs) != 2 || !strings.HasPrefix(requestedURLs[0], "https://oauth.bitrix.info/oauth/token/") || requestedURLs[1] != "https://crm.hub-bnk.com/rest/profile.json" {
		t.Errorf("unexpected outbound requests: %v", requestedURLs)
	}

	// Verify connection is persisted in SQLite
	listed, err := st.ListConnections(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("expected 1 stored connection, got %d err=%v", len(listed), err)
	}
}

func TestOAuthService_HandleInstallationAndDomainLock(t *testing.T) {
	ctx := context.Background()
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "app.info.json") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{"CODE": "local.abc123", "INSTALLED": false},
			}), nil
		}
		return jsonResponse(200, map[string]any{
			"result": map[string]any{"ID": 7},
		}), nil
	})

	_, _, oauth, _ := newTestEnv(t, "", "pending-bitrix-app", rt)

	conn, err := oauth.HandleInstallation(ctx, map[string]any{
		"DOMAIN":            "example.bitrix24.by",
		"AUTH_ID":           "placement-access",
		"AUTH_EXPIRES":      "3600",
		"REFRESH_ID":        "placement-refresh",
		"APPLICATION_SCOPE": "crm,user",
		"member_id":         "80bcec0a985d0c2ae3a436711bc473f0",
	})
	if err != nil {
		t.Fatalf("HandleInstallation failed: %v", err)
	}
	if conn.Domain != "example.bitrix24.by" || conn.BitrixUserID != "7" || conn.Scope != "crm,user" {
		t.Errorf("unexpected installed connection: %+v", conn)
	}

	// Once an active connection exists on example.bitrix24.by, unauthenticated install from another portal is locked out
	_, err = oauth.HandleInstallation(ctx, map[string]any{
		"DOMAIN":     "other.bitrix24.com",
		"AUTH_ID":    "other-access",
		"REFRESH_ID": "other-refresh",
		"member_id":  "other-member",
	})
	if err == nil {
		t.Errorf("expected unauthenticated install for second domain to be rejected once active domain is established")
	}
}

func TestTokenManager_DeduplicatesRefreshAndMarksRevoked(t *testing.T) {
	ctx := context.Background()
	var refreshCalls atomic.Int32
	shouldFailInvalidGrant := false

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		refreshCalls.Add(1)
		time.Sleep(25 * time.Millisecond)
		if shouldFailInvalidGrant {
			return jsonResponse(400, map[string]any{"error": "invalid_grant"}), nil
		}
		return jsonResponse(200, map[string]any{
			"access_token":    "new-access",
			"refresh_token":   "new-refresh",
			"expires_in":      3600,
			"scope":           "crm",
			"domain":          "oauth.bitrix.info",
			"member_id":       "member-1",
			"user_id":         "1",
			"client_endpoint": "https://crm.hub-bnk.com/rest/",
		}), nil
	})

	_, st, _, tm := newTestEnv(t, "crm.hub-bnk.com", "client-id", rt)
	created, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "crm.hub-bnk.com",
		MemberID:       "member-1",
		BitrixUserID:   "1",
		ClientEndpoint: "https://crm.hub-bnk.com/rest/",
		AccessToken:    "old-access",
		RefreshToken:   "old-refresh",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("failed to seed connection: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res, err := tm.Get(ctx, created.ID, true, "old-access")
			if err != nil {
				errs[idx] = err
				return
			}
			results[idx] = res.AccessToken
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d failed: %v", i, err)
		}
		if results[i] != "new-access" {
			t.Errorf("goroutine %d got %q, want 'new-access'", i, results[i])
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Errorf("expected exactly 1 refresh HTTP call, got %d", got)
	}

	// Verify invalid_grant marks connection revoked
	shouldFailInvalidGrant = true
	if _, err := tm.Get(ctx, created.ID, true, ""); err == nil {
		t.Fatalf("expected error on invalid_grant")
	}
	afterRevoke, err := st.GetConnection(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConnection failed: %v", err)
	}
	if afterRevoke.Status != "revoked" {
		t.Errorf("expected connection status 'revoked', got %q", afterRevoke.Status)
	}
}

func TestOAuthService_CreateMcpAuthorizationURLAndContinuationError(t *testing.T) {
	ctx := context.Background()
	_, st, oauth, _ := newTestEnv(t, "", "client-id", nil)

	// 1. Without BITRIX_PORTAL_DOMAIN or active connection -> 503 BITRIX_PORTAL_NOT_CONFIGURED
	if _, err := oauth.CreateMcpAuthorizationURL(ctx, ""); err == nil || !strings.Contains(err.Error(), "BITRIX_PORTAL_DOMAIN") {
		t.Fatalf("expected BITRIX_PORTAL_NOT_CONFIGURED error when no portal is configured, got %v", err)
	}

	// 2. Seed a single active connection and an MCP authorization request
	_, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "active.bitrix24.com",
		MemberID:       "member-active",
		BitrixUserID:   "1",
		ClientEndpoint: "https://active.bitrix24.com/rest/",
		AccessToken:    "acc",
		RefreshToken:   "ref",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("UpsertConnection failed: %v", err)
	}
	if err := st.RegisterOAuthClient(ctx, store.McpOAuthClient{
		ClientID:                "mcp-client",
		RedirectURIs:            []string{"http://127.0.0.1:3000/cb"},
		TokenEndpointAuthMethod: "none",
		GrantTypes:              []string{"authorization_code"},
		ResponseTypes:           []string{"code"},
		Scope:                   "mcp:tools",
		CreatedAt:               time.Now(),
	}); err != nil {
		t.Fatalf("RegisterOAuthClient failed: %v", err)
	}
	if err := st.CreateAuthorizationRequest(ctx, store.McpOAuthAuthorizationRequest{
		RequestHash:   "req-hash-cont",
		ClientID:      "mcp-client",
		RedirectURI:   "http://127.0.0.1:3000/cb",
		State:         "client-state",
		CodeChallenge: "challenge",
		Resource:      "https://mcp.example.com/mcp",
		Scope:         "mcp:tools",
		ExpiresAt:     time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("CreateAuthorizationRequest failed: %v", err)
	}

	authURLStr, err := oauth.CreateMcpAuthorizationURL(ctx, "req-hash-cont")
	if err != nil {
		t.Fatalf("CreateMcpAuthorizationURL failed: %v", err)
	}
	parsedURL, err := url.Parse(authURLStr)
	if err != nil || parsedURL.Host != "active.bitrix24.com" {
		t.Fatalf("expected redirect to active.bitrix24.com, got %q err=%v", authURLStr, err)
	}
	stateParam := parsedURL.Query().Get("state")

	// 3. Denied callback with mcp_request_hash returns ContinuationError with access_denied
	_, cbErr := oauth.HandleCallbackWithContext(ctx, bitrix.CallbackInput{
		State: stateParam,
		Error: "access_denied",
	})
	var contErr *bitrix.ContinuationError
	if cbErr == nil || !strings.Contains(cbErr.Error(), "Bitrix24 authorization was denied") {
		t.Fatalf("expected continuation error on denied callback, got %v", cbErr)
	}
	if ok := errorsAsContinuation(cbErr, &contErr); !ok || contErr.McpRequestHash != "req-hash-cont" || contErr.OAuthErrorCode != "access_denied" {
		t.Errorf("unexpected ContinuationError: %+v", contErr)
	}
}

func errorsAsContinuation(err error, target **bitrix.ContinuationError) bool {
	if c, ok := err.(*bitrix.ContinuationError); ok {
		*target = c
		return true
	}
	return false
}

func TestFlexMap_DecodesEmptyArrayAndObject(t *testing.T) {
	var empty bitrix.FlexMap[string]
	if err := json.Unmarshal([]byte(`[]`), &empty); err != nil {
		t.Fatalf("expected [] to unmarshal into empty FlexMap, got err=%v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected empty FlexMap, got %v", empty)
	}

	var obj bitrix.FlexMap[int]
	if err := json.Unmarshal([]byte(`{"a":1,"b":2}`), &obj); err != nil {
		t.Fatalf("expected object to unmarshal into FlexMap, got err=%v", err)
	}
	if obj["a"] != 1 || obj["b"] != 2 {
		t.Errorf("unexpected FlexMap contents: %v", obj)
	}
}

func TestRestClient_RefreshesExpiredTokenRetriesAndBatch(t *testing.T) {
	ctx := context.Background()

	// 1. BuildBatchCommand nested parameter serialization
	cmdStr, err := bitrix.BuildBatchCommand("crm.item.list", map[string]any{
		"entityTypeId": 2,
		"filter":       map[string]any{"stageId": "NEW"},
		"select":       []string{"id", "title"},
		"start":        50,
	})
	if err != nil {
		t.Fatalf("BuildBatchCommand failed: %v", err)
	}
	decodedCmd, _ := url.QueryUnescape(cmdStr)
	if decodedCmd != "crm.item.list?entityTypeId=2&filter[stageId]=NEW&select[0]=id&select[1]=title&start=50" {
		t.Errorf("unexpected batch command string: %s", decodedCmd)
	}

	var restCalls atomic.Int32
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "oauth.bitrix.info" {
			return jsonResponse(200, map[string]any{
				"access_token":    "new-secret",
				"refresh_token":   "new-refresh",
				"expires_in":      3600,
				"scope":           "crm",
				"domain":          "oauth.bitrix.info",
				"member_id":       "member-1",
				"user_id":         "1",
				"client_endpoint": "https://example.bitrix24.com/rest/",
			}), nil
		}
		callNum := restCalls.Add(1)
		bodyBytes, _ := io.ReadAll(r.Body)
		var bodyMap map[string]any
		_ = json.Unmarshal(bodyBytes, &bodyMap)

		if strings.HasSuffix(r.URL.Path, "crm.item.get.json") {
			if strings.Contains(r.URL.String(), "old-secret") || strings.Contains(r.URL.String(), "new-secret") {
				t.Errorf("token must not be exposed in URL: %s", r.URL.String())
			}
			if callNum == 1 {
				return jsonResponse(401, map[string]any{"error": "expired_token"}), nil
			}
			if callNum == 2 {
				if bodyMap["auth"] != "new-secret" {
					t.Errorf("expected refreshed auth token 'new-secret', got %v", bodyMap["auth"])
				}
				resp := jsonResponse(429, map[string]any{"error": "QUERY_LIMIT_EXCEEDED"})
				resp.Header.Set("Retry-After", "0")
				return resp, nil
			}
			return jsonResponse(200, map[string]any{
				"result": map[string]any{"item": map[string]any{"id": 7}},
			}), nil
		}
		if strings.HasSuffix(r.URL.Path, "batch.json") {
			cmdMap, _ := bodyMap["cmd"].(map[string]any)
			resMap := make(map[string]any, len(cmdMap))
			for k := range cmdMap {
				resMap[k] = map[string]any{"ok": k}
			}
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"result":       resMap,
					"result_error": []any{},
				},
			}), nil
		}
		return jsonResponse(200, map[string]any{"result": true}), nil
	})

	cfg, st, _, tm := newTestEnv(t, "", "client-id", rt)
	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-1",
		BitrixUserID:   "1",
		ClientEndpoint: "https://example.bitrix24.com/rest/",
		AccessToken:    "old-secret",
		RefreshToken:   "old-refresh",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("UpsertConnection failed: %v", err)
	}

	httpClient := &http.Client{Transport: rt, Timeout: 2 * time.Second}
	rc := bitrix.NewRestClient(cfg, tm, config.NewLogger(nil, "silent"), httpClient)

	// 2. Reject invalid method before making network call
	if _, err := rc.Call(ctx, conn.ID, "https://evil.example", nil); err == nil {
		t.Fatalf("expected invalid method to be rejected")
	}
	if restCalls.Load() != 0 {
		t.Fatalf("expected 0 network calls for invalid method, got %d", restCalls.Load())
	}

	// 3. Call refreshes on expired_token and retries on 429 QUERY_LIMIT_EXCEEDED
	resp, err := rc.Call(ctx, conn.ID, "crm.item.get", map[string]any{"entityTypeId": 2, "id": 7})
	if err != nil {
		t.Fatalf("rc.Call failed: %v", err)
	}
	if !strings.Contains(string(resp.Result), `"id":7`) {
		t.Errorf("unexpected response result: %s", string(resp.Result))
	}

	// 4. CallBatch chunks 55 commands into 2 batch requests (50 + 5)
	var commands []bitrix.NamedBatchCommand
	for i := 0; i < 55; i++ {
		commands = append(commands, bitrix.NamedBatchCommand{
			Key:    "k_" + string(rune('a'+(i%26))) + "_" + url.QueryEscape(string(rune('0'+(i/26)))),
			Method: "crm.item.get",
			Params: map[string]any{"id": i + 1},
		})
	}
	batchRes, err := rc.CallBatch(ctx, conn.ID, commands, false)
	if err != nil || len(batchRes) != 55 {
		t.Fatalf("expected 55 batch results, got %d err=%v", len(batchRes), err)
	}
}

func TestSchemaDiscovery_CachingAndDescribeBatchStages(t *testing.T) {
	ctx := context.Background()
	var batchCalls atomic.Int32
	var typeCalls atomic.Int32

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "crm.type.list.json"):
			typeCalls.Add(1)
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"types": []map[string]any{
						{"entityTypeId": 128, "title": "Smart Process"},
					},
				},
			}), nil
		case strings.HasSuffix(r.URL.Path, "crm.item.fields.json"):
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"fields": map[string]any{"TITLE": map[string]any{"type": "string"}},
				},
			}), nil
		case strings.HasSuffix(r.URL.Path, "crm.category.list.json"):
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"categories": []map[string]any{
						{"id": 0, "name": "Default"},
						{"id": 5, "name": "Enterprise"},
					},
				},
			}), nil
		case strings.HasSuffix(r.URL.Path, "batch.json"):
			batchCalls.Add(1)
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"result": map[string]any{
						"cat_0": []map[string]any{{"STATUS_ID": "NEW", "NAME": "New"}},
						"cat_5": []map[string]any{{"STATUS_ID": "C5:NEW", "NAME": "Ent New"}},
					},
					"result_error": []any{},
				},
			}), nil
		case strings.HasPrefix(r.URL.Path, "/rest/api/"):
			return jsonResponse(404, map[string]any{"error": "BITRIX_REST_V3_EXCEPTION_UNKNOWNMETHOD"}), nil
		default:
			return jsonResponse(200, map[string]any{"result": []any{}}), nil
		}
	})

	cfg, st, _, tm := newTestEnv(t, "", "client-id", rt)
	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-schema",
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
	rc := bitrix.NewRestClient(cfg, tm, config.NewLogger(nil, "silent"), httpClient)
	schema := bitrix.NewSchemaDiscovery(cfg, st, rc)

	// 1. ListEntityTypes merges built-in + dynamic and caches result
	types1, err := schema.ListEntityTypes(ctx, conn.ID, false)
	if err != nil || len(types1) != 7 {
		t.Fatalf("expected 7 entity types (6 built-in + 1 dynamic), got %d err=%v", len(types1), err)
	}
	types2, err := schema.ListEntityTypes(ctx, conn.ID, false)
	if err != nil || len(types2) != 7 || typeCalls.Load() != 1 {
		t.Fatalf("expected cached ListEntityTypes (1 HTTP call), got calls=%d err=%v", typeCalls.Load(), err)
	}

	// 2. ResolveEntityType by title
	resolved, err := schema.ResolveEntityType(ctx, conn.ID, "Smart Process")
	if err != nil || resolved["entityTypeId"] != 128 {
		t.Fatalf("expected entityTypeId 128 for 'Smart Process', got %+v err=%v", resolved, err)
	}

	// 3. Describe batch-fetches uncached category stages
	desc, err := schema.Describe(ctx, conn.ID, 2, nil, false)
	if err != nil {
		t.Fatalf("Describe failed: %v", err)
	}
	if batchCalls.Load() != 1 {
		t.Errorf("expected 1 batch call for 2 uncached categories, got %d", batchCalls.Load())
	}
	descJSON, _ := json.Marshal(desc)
	if !strings.Contains(string(descJSON), "C5:NEW") {
		t.Errorf("expected C5:NEW in describe output: %s", string(descJSON))
	}

	// 4. Refresh clears schema cache
	refreshed, err := schema.Refresh(ctx, conn.ID)
	if err != nil || refreshed["clearedEntries"] == int64(0) {
		t.Errorf("expected non-zero clearedEntries on Refresh, got %+v err=%v", refreshed, err)
	}
}

func TestCrmService_SearchBatchAndProducts(t *testing.T) {
	ctx := context.Background()

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		bodyBytes, _ := io.ReadAll(r.Body)
		bodyStr := string(bodyBytes)

		switch {
		case strings.HasSuffix(r.URL.Path, "crm.item.list.json"):
			if strings.Contains(bodyStr, `"start":50`) {
				return jsonResponse(200, map[string]any{
					"result": map[string]any{
						"items": []map[string]any{
							{"id": 51, "title": "Deal 51"},
						},
					},
					"total": 51,
				}), nil
			}
			// Page 1: 50 items, next=50
			items := make([]map[string]any, 50)
			for i := 0; i < 50; i++ {
				items[i] = map[string]any{"id": i + 1, "title": fmt.Sprintf("Deal %d", i+1)}
			}
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"items": items,
				},
				"next":  50,
				"total": 51,
			}), nil

		case strings.HasSuffix(r.URL.Path, "crm.item.get.json"):
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"item": map[string]any{"id": 100, "title": "Smart Item 100"},
				},
			}), nil

		case strings.HasSuffix(r.URL.Path, "user.get.json"):
			return jsonResponse(200, map[string]any{
				"result": []map[string]any{
					{"ID": "1", "NAME": "John", "LAST_NAME": "Doe"},
				},
			}), nil

		case strings.HasSuffix(r.URL.Path, "batch.json"):
			if strings.Contains(bodyStr, "crm.item.list") {
				return jsonResponse(200, map[string]any{
					"result": map[string]any{
						"result": map[string]any{
							"p_50": map[string]any{
								"items": []map[string]any{
									{"id": 51, "title": "Deal 51"},
								},
							},
						},
						"result_error": []any{},
					},
				}), nil
			}
			if strings.Contains(bodyStr, "crm.item.productrow.list") {
				return jsonResponse(200, map[string]any{
					"result": map[string]any{
						"result": map[string]any{
							"1": map[string]any{
								"productRows": []map[string]any{
									{"ID": "10", "PRODUCT_NAME": "Widget A", "PRICE": 100},
								},
							},
						},
						"result_error": []any{},
					},
				}), nil
			}
			if strings.Contains(bodyStr, "crm.item.get") {
				return jsonResponse(200, map[string]any{
					"result": map[string]any{
						"result": map[string]any{
							"1": map[string]any{"item": map[string]any{"id": 1, "title": "Deal 1"}},
							"2": map[string]any{"item": map[string]any{"id": 2, "title": "Deal 2"}},
						},
						"result_error": []any{},
					},
				}), nil
			}
			return jsonResponse(200, map[string]any{"result": map[string]any{"result": map[string]any{}}}), nil

		case strings.HasPrefix(r.URL.Path, "/rest/api/"):
			return jsonResponse(404, map[string]any{"error": "BITRIX_REST_V3_EXCEPTION_UNKNOWNMETHOD"}), nil

		default:
			return jsonResponse(200, map[string]any{"result": map[string]any{}}), nil
		}
	})

	cfg, st, _, tm := newTestEnv(t, "", "client-id", rt)
	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-crm",
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
	rc := bitrix.NewRestClient(cfg, tm, config.NewLogger(nil, "silent"), httpClient)
	crmSvc := bitrix.NewCrmService(cfg, rc)

	// Test 1: SearchItems pagination (fetching 51 deals across 2 pages)
	searchRes, err := crmSvc.SearchItems(ctx, bitrix.SearchItemsInput{
		ConnectionID:       conn.ID,
		EntityTypeID:       2,
		Limit:              60,
		IncludeProductRows: true,
	})
	if err != nil {
		t.Fatalf("SearchItems failed: %v", err)
	}
	if len(searchRes.Items) != 51 {
		t.Fatalf("expected 51 items, got %d", len(searchRes.Items))
	}
	// Verify product rows were attached to item 1
	if rows, ok := searchRes.Items[0]["productRows"].([]map[string]any); !ok || len(rows) != 1 {
		t.Errorf("expected productRows attached to item 1, got %+v", searchRes.Items[0]["productRows"])
	}

	// Test 2: GetItem for dynamic entity
	itemRes, err := crmSvc.GetItem(ctx, conn.ID, 128, 100)
	if err != nil {
		t.Fatalf("GetItem failed: %v", err)
	}
	itemMap, ok := itemRes.(map[string]any)
	if !ok || itemMap["title"] != "Smart Item 100" {
		t.Errorf("expected item title 'Smart Item 100', got %+v", itemRes)
	}

	// Test 3: GetItems batch
	batchItems, err := crmSvc.GetItems(ctx, conn.ID, 2, []int{1, 2})
	if err != nil {
		t.Fatalf("GetItems failed: %v", err)
	}
	if len(batchItems) != 2 {
		t.Fatalf("expected 2 items, got %d", len(batchItems))
	}

	// Test 4: ListUsers batch
	users, err := crmSvc.ListUsers(ctx, conn.ID, map[string]any{"ID": "1"})
	if err != nil {
		t.Fatalf("ListUsers failed: %v", err)
	}
	if len(users) != 1 || users[0]["NAME"] != "John" {
		t.Fatalf("expected user John, got %+v", users)
	}

	// Test 5: Upper-bound validation on batch IDs (250) and search limit (1000)
	tooManyIDs := make([]int, 251)
	for i := range tooManyIDs {
		tooManyIDs[i] = i + 1
	}
	if _, err := crmSvc.GetItems(ctx, conn.ID, 2, tooManyIDs); err == nil {
		t.Fatalf("expected GetItems to reject >250 IDs")
	}
	if _, err := crmSvc.ListProductRows(ctx, bitrix.ListProductRowsInput{
		ConnectionID: conn.ID,
		OwnerIDs:     tooManyIDs,
	}); err == nil {
		t.Fatalf("expected ListProductRows to reject >250 ownerIDs")
	}
	if _, err := crmSvc.SearchItems(ctx, bitrix.SearchItemsInput{
		ConnectionID: conn.ID,
		EntityTypeID: 2,
		Limit:        1001,
	}); err == nil {
		t.Fatalf("expected SearchItems to reject limit >1000")
	}
}

func TestOperationService_TwoStepLifecycleAndPolicyGuard(t *testing.T) {
	ctx := context.Background()

	executedMethod := ""
	var executedParams any

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		bodyBytes, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "crm.item.add.json"):
			executedMethod = "crm.item.add"
			_ = json.Unmarshal(bodyBytes, &executedParams)
			return jsonResponse(200, map[string]any{"result": 42}), nil
		case strings.HasSuffix(r.URL.Path, "crm.item.update.json"):
			executedMethod = "crm.item.update"
			_ = json.Unmarshal(bodyBytes, &executedParams)
			return jsonResponse(200, map[string]any{"result": true}), nil
		default:
			return jsonResponse(200, map[string]any{"result": true}), nil
		}
	})

	cfg, st, _, tm := newTestEnv(t, "", "client-id", rt)
	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-ops",
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
	rc := bitrix.NewRestClient(cfg, tm, config.NewLogger(nil, "silent"), httpClient)
	policyGuard := bitrix.NewPolicyGuard()
	opSvc := bitrix.NewOperationService(cfg, st, policyGuard, rc)

	// 1. Prepare Create
	prep, err := opSvc.PrepareCreate(ctx, conn.ID, 2, map[string]any{"TITLE": "New Big Deal"})
	if err != nil {
		t.Fatalf("PrepareCreate failed: %v", err)
	}
	opID, _ := prep["operation_id"].(string)
	if opID == "" || prep["requires_confirmation"] != true {
		t.Fatalf("expected pending operation, got %+v", prep)
	}

	// Check listing pending
	list, err := st.ListPendingOperations(ctx, &conn.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 pending operation in list, got %d, err=%v", len(list), err)
	}

	// 2. Confirm operation
	confirmRes, err := opSvc.Confirm(ctx, opID, "")
	if err != nil {
		t.Fatalf("Confirm failed: %v", err)
	}
	if confirmRes["status"] != "completed" {
		t.Fatalf("expected completed status, got %+v", confirmRes)
	}
	if executedMethod != "crm.item.add" {
		t.Fatalf("expected crm.item.add to be executed, got %q", executedMethod)
	}

	// Verify completed operation state and double-confirm prevention
	completedOp, err := st.GetPendingOperation(ctx, opID)
	if err != nil || completedOp.Status != store.OpStatusCompleted {
		t.Fatalf("expected operation %s to have status completed, got %+v err=%v", opID, completedOp, err)
	}
	if _, err := opSvc.Confirm(ctx, opID, ""); err == nil {
		t.Fatalf("expected second Confirm on completed operation to be rejected")
	}

	// Verify PolicyGuard capabilities (Read, Admin, Dangerous)
	if err := policyGuard.Assert(conn, bitrix.CapabilityRead, false); err != nil {
		t.Fatalf("expected CapabilityRead to be allowed, got %v", err)
	}
	if err := policyGuard.Assert(conn, bitrix.CapabilityDangerous, true); err == nil {
		t.Fatalf("expected CapabilityDangerous to be rejected")
	}
	if err := policyGuard.Assert(conn, bitrix.CapabilityAdmin, true); err == nil {
		t.Fatalf("expected CapabilityAdmin to be rejected when AdminToolsEnabled is false")
	}

	// 3. Cancel test
	prepCancel, err := opSvc.PrepareUpdate(ctx, conn.ID, 2, 42, map[string]any{"TITLE": "Updated Deal"})
	if err != nil {
		t.Fatalf("PrepareUpdate failed: %v", err)
	}
	cancelID, _ := prepCancel["operation_id"].(string)

	cancelRes, err := opSvc.Cancel(ctx, cancelID, "")
	if err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}
	if cancelRes["status"] != "cancelled" {
		t.Fatalf("expected cancelled status, got %+v", cancelRes)
	}

	// 4. Policy Guard rejection in read_only mode
	_, err = st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-ops",
		BitrixUserID:   "1",
		ClientEndpoint: "https://example.bitrix24.com/rest/",
		AccessToken:    "acc",
		RefreshToken:   "ref",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("update connection to read_only failed: %v", err)
	}

	_, err = opSvc.PrepareCreate(ctx, conn.ID, 2, map[string]any{"TITLE": "Blocked Deal"})
	if err == nil {
		t.Fatalf("expected PrepareCreate to be blocked in read_only mode")
	}
}

func TestRestClient_REST3AndLegacyRouting_OAuthAndWebhook(t *testing.T) {
	ctx := context.Background()

	var recordedRequests []struct {
		URL    string
		Auth   any
		IsJSON bool
		Raw    string
	}
	var mu sync.Mutex

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var bodyMap map[string]any
		isJSON := json.Unmarshal(bodyBytes, &bodyMap) == nil

		mu.Lock()
		recordedRequests = append(recordedRequests, struct {
			URL    string
			Auth   any
			IsJSON bool
			Raw    string
		}{
			URL:    r.URL.String(),
			Auth:   bodyMap["auth"],
			IsJSON: isJSON,
			Raw:    string(bodyBytes),
		})
		mu.Unlock()

		if strings.Contains(r.URL.Path, "batch") {
			if strings.Contains(r.URL.Path, "/rest/api") {
				// REST 3.0 batch response: json array
				return jsonResponse(200, map[string]any{
					"result": []any{
						map[string]any{"id": 1, "title": "Deal 1"},
					},
				}), nil
			}
			// Legacy batch response: map
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"result": map[string]any{
						"cmd0": map[string]any{"id": 1, "title": "Deal 1"},
					},
					"result_error": []any{},
				},
			}), nil
		}

		return jsonResponse(200, map[string]any{
			"result": map[string]any{"item": map[string]any{"id": 10}},
		}), nil
	})

	cfg, st, _, tm := newTestEnv(t, "", "client-id", rt)
	rc := bitrix.NewRestClient(cfg, tm, config.NewLogger(nil, "silent"), &http.Client{Transport: rt})

	// 1. OAuth Connection
	oauthConn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-oauth",
		BitrixUserID:   "1",
		ClientEndpoint: "https://example.bitrix24.com/rest/",
		AccessToken:    "oauth-secret",
		RefreshToken:   "oauth-refresh",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("UpsertConnection failed: %v", err)
	}

	// OAuth v3 single call
	_, err = rc.Call(ctx, oauthConn.ID, "crm.deal.get", map[string]any{"id": 10}, bitrix.WithAPIVersion("v3"))
	if err != nil {
		t.Fatalf("OAuth v3 Call failed: %v", err)
	}
	// OAuth v2 single call
	_, err = rc.Call(ctx, oauthConn.ID, "crm.deal.get", map[string]any{"id": 10}, bitrix.WithAPIVersion("v2"))
	if err != nil {
		t.Fatalf("OAuth v2 Call failed: %v", err)
	}
	// OAuth v3 batch
	_, err = rc.CallBatch(ctx, oauthConn.ID, []bitrix.NamedBatchCommand{
		{Key: "cmd0", Method: "crm.deal.get", Params: map[string]any{"id": 1}},
	}, false, bitrix.WithAPIVersion("v3"))
	if err != nil {
		t.Fatalf("OAuth v3 CallBatch failed: %v", err)
	}
	// OAuth v2 batch
	_, err = rc.CallBatch(ctx, oauthConn.ID, []bitrix.NamedBatchCommand{
		{Key: "cmd0", Method: "crm.deal.get", Params: map[string]any{"id": 1}},
	}, false, bitrix.WithAPIVersion("v2"))
	if err != nil {
		t.Fatalf("OAuth v2 CallBatch failed: %v", err)
	}

	// 2. Webhook Connection
	whCfg := &config.Config{
		BitrixWebhookURL: "https://example.bitrix24.com/rest/42/webhook-secret-code/",
		DefaultMode:      config.ModeReadOnly,
	}
	whConn, err := st.EnsureWebhookConnection(ctx, whCfg)
	if err != nil {
		t.Fatalf("EnsureWebhookConnection failed: %v", err)
	}

	// Webhook v3 single call
	_, err = rc.Call(ctx, whConn.ID, "crm.deal.get", map[string]any{"id": 10}, bitrix.WithAPIVersion("v3"))
	if err != nil {
		t.Fatalf("Webhook v3 Call failed: %v", err)
	}
	// Webhook v2 single call
	_, err = rc.Call(ctx, whConn.ID, "crm.deal.get", map[string]any{"id": 10}, bitrix.WithAPIVersion("v2"))
	if err != nil {
		t.Fatalf("Webhook v2 Call failed: %v", err)
	}
	// Webhook v3 batch
	_, err = rc.CallBatch(ctx, whConn.ID, []bitrix.NamedBatchCommand{
		{Key: "cmd0", Method: "crm.deal.get", Params: map[string]any{"id": 1}},
	}, false, bitrix.WithAPIVersion("v3"))
	if err != nil {
		t.Fatalf("Webhook v3 CallBatch failed: %v", err)
	}
	// Webhook v2 batch
	_, err = rc.CallBatch(ctx, whConn.ID, []bitrix.NamedBatchCommand{
		{Key: "cmd0", Method: "crm.deal.get", Params: map[string]any{"id": 1}},
	}, false, bitrix.WithAPIVersion("v2"))
	if err != nil {
		t.Fatalf("Webhook v2 CallBatch failed: %v", err)
	}

	mu.Lock()
	reqs := recordedRequests
	mu.Unlock()

	if len(reqs) != 8 {
		t.Fatalf("expected 8 recorded requests, got %d", len(reqs))
	}

	// Check 0: OAuth v3 single
	if reqs[0].URL != "https://example.bitrix24.com/rest/api/crm.deal.get" {
		t.Errorf("OAuth v3 single URL mismatch: %s", reqs[0].URL)
	}
	if reqs[0].Auth != "oauth-secret" {
		t.Errorf("OAuth v3 single expected auth in body, got: %v", reqs[0].Auth)
	}

	// Check 1: OAuth v2 single
	if reqs[1].URL != "https://example.bitrix24.com/rest/crm.deal.get.json" {
		t.Errorf("OAuth v2 single URL mismatch: %s", reqs[1].URL)
	}
	if reqs[1].Auth != "oauth-secret" {
		t.Errorf("OAuth v2 single expected auth in body, got: %v", reqs[1].Auth)
	}

	// Check 2: OAuth v3 batch -> /rest/api/batch?auth=... with native JSON array
	if !strings.HasPrefix(reqs[2].URL, "https://example.bitrix24.com/rest/api/batch") {
		t.Errorf("OAuth v3 batch URL mismatch: %s", reqs[2].URL)
	}
	if !strings.HasPrefix(strings.TrimSpace(reqs[2].Raw), "[") {
		t.Errorf("OAuth v3 batch expected JSON array body, got: %s", reqs[2].Raw)
	}

	// Check 3: OAuth v2 batch -> /rest/batch.json
	if reqs[3].URL != "https://example.bitrix24.com/rest/batch.json" {
		t.Errorf("OAuth v2 batch URL mismatch: %s", reqs[3].URL)
	}
	if reqs[3].Auth != "oauth-secret" {
		t.Errorf("OAuth v2 batch expected auth in body, got: %v", reqs[3].Auth)
	}

	// Check 4: Webhook v3 single -> /rest/api/42/webhook-secret-code/crm.deal.get (no auth)
	if reqs[4].URL != "https://example.bitrix24.com/rest/api/42/webhook-secret-code/crm.deal.get" {
		t.Errorf("Webhook v3 single URL mismatch: %s", reqs[4].URL)
	}
	if reqs[4].Auth != nil {
		t.Errorf("Webhook v3 single must NOT contain auth in body, got: %v", reqs[4].Auth)
	}

	// Check 5: Webhook v2 single -> /rest/42/webhook-secret-code/crm.deal.get.json (no auth)
	if reqs[5].URL != "https://example.bitrix24.com/rest/42/webhook-secret-code/crm.deal.get.json" {
		t.Errorf("Webhook v2 single URL mismatch: %s", reqs[5].URL)
	}
	if reqs[5].Auth != nil {
		t.Errorf("Webhook v2 single must NOT contain auth in body, got: %v", reqs[5].Auth)
	}

	// Check 6: Webhook v3 batch -> /rest/api/42/webhook-secret-code/batch
	if reqs[6].URL != "https://example.bitrix24.com/rest/api/42/webhook-secret-code/batch" {
		t.Errorf("Webhook v3 batch URL mismatch: %s", reqs[6].URL)
	}
	if !strings.HasPrefix(strings.TrimSpace(reqs[6].Raw), "[") {
		t.Errorf("Webhook v3 batch expected JSON array body, got: %s", reqs[6].Raw)
	}

	// Check 7: Webhook v2 batch -> /rest/42/webhook-secret-code/batch.json
	if reqs[7].URL != "https://example.bitrix24.com/rest/42/webhook-secret-code/batch.json" {
		t.Errorf("Webhook v2 batch URL mismatch: %s", reqs[7].URL)
	}
	if reqs[7].Auth != nil {
		t.Errorf("Webhook v2 batch must NOT contain auth in body, got: %v", reqs[7].Auth)
	}
}

func TestRestClient_PolymorphicErrorsAndValidation(t *testing.T) {
	ctx := context.Background()

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var bodyBytes []byte
		if r.Body != nil {
			bodyBytes, _ = io.ReadAll(r.Body)
		}
		var bodyMap map[string]any
		_ = json.Unmarshal(bodyBytes, &bodyMap)

		if strings.Contains(r.URL.Path, "legacy_err") {
			return jsonResponse(400, map[string]any{
				"error":             "ERROR_LEGACY",
				"error_description": "Legacy error description",
			}), nil
		}
		if strings.Contains(r.URL.Path, "rest3_validation") {
			return jsonResponse(400, map[string]any{
				"error": map[string]any{
					"code":    "BITRIX_REST_V3_EXCEPTION_VALIDATION_REQUESTVALIDATIONEXCEPTION",
					"message": "Validation failed",
					"validation": []any{
						map[string]any{"field": "title", "message": "Title is required"},
					},
				},
				"time": map[string]any{
					"duration": 0.05,
				},
			}), nil
		}
		if strings.Contains(r.URL.Path, "rest3_expired") {
			if bodyMap["auth"] == "refreshed-secret" {
				return jsonResponse(200, map[string]any{"result": true}), nil
			}
			return jsonResponse(401, map[string]any{
				"error": map[string]any{
					"code":    "expired_token",
					"message": "Token expired in REST 3.0",
				},
			}), nil
		}
		if r.URL.Hostname() == "oauth.bitrix.info" {
			return jsonResponse(200, map[string]any{
				"access_token":    "refreshed-secret",
				"refresh_token":   "refreshed-refresh",
				"expires_in":      3600,
				"scope":           "crm",
				"domain":          "example.bitrix24.com",
				"member_id":       "member-err",
				"user_id":         "1",
				"client_endpoint": "https://example.bitrix24.com/rest/",
			}), nil
		}
		return jsonResponse(200, map[string]any{"result": true}), nil
	})

	cfg, st, _, tm := newTestEnv(t, "", "client-id", rt)
	rc := bitrix.NewRestClient(cfg, tm, config.NewLogger(nil, "silent"), &http.Client{Transport: rt})

	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-err",
		BitrixUserID:   "1",
		ClientEndpoint: "https://example.bitrix24.com/rest/",
		AccessToken:    "old-secret",
		RefreshToken:   "old-refresh",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("UpsertConnection failed: %v", err)
	}

	// 1. Legacy error string decoded
	_, err = rc.Call(ctx, conn.ID, "legacy_err", nil)
	if err == nil {
		t.Fatalf("expected error for legacy_err")
	}
	var appErr *config.AppError
	if !errorsAsAppError(err, &appErr) {
		t.Fatalf("expected *config.AppError, got %T: %v", err, err)
	}
	if appErr.Code != "ERROR_LEGACY" {
		t.Errorf("expected code 'ERROR_LEGACY', got %q", appErr.Code)
	}

	// 2. REST 3.0 nested error with validation items decoded
	_, err = rc.Call(ctx, conn.ID, "rest3_validation", nil)
	if err == nil {
		t.Fatalf("expected error for rest3_validation")
	}
	if !errorsAsAppError(err, &appErr) {
		t.Fatalf("expected *config.AppError, got %T: %v", err, err)
	}
	if appErr.Code != "BITRIX_REST_V3_EXCEPTION_VALIDATION_REQUESTVALIDATIONEXCEPTION" {
		t.Errorf("expected REST 3.0 code, got %q", appErr.Code)
	}
	if !strings.Contains(appErr.Message, "title: Title is required") {
		t.Errorf("expected validation field details in message, got %q", appErr.Message)
	}
	if len(appErr.Validation) != 1 || appErr.Validation[0].Field != "title" {
		t.Errorf("expected validation slice on AppError, got %+v", appErr.Validation)
	}

	// 3. REST 3.0 nested expired_token triggers singleflight OAuth token refresh
	_, err = rc.Call(ctx, conn.ID, "rest3_expired", nil)
	if err != nil {
		t.Fatalf("expected successful call after token refresh, got: %v", err)
	}
	updatedConn, err := st.GetConnection(ctx, conn.ID)
	if err != nil || updatedConn.AccessToken != "refreshed-secret" {
		t.Errorf("expected access token to be refreshed to 'refreshed-secret', got %+v err=%v", updatedConn, err)
	}
}

func TestRestClient_TelemetryTime_OperatingResetAt_AndIdempotencyKey(t *testing.T) {
	ctx := context.Background()

	var requestCount atomic.Int32
	var idempotencyHeaders []string
	var mu sync.Mutex

	nowSec := time.Now().Unix()
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		idempotencyHeaders = append(idempotencyHeaders, r.Header.Get("Idempotency-Key"))
		mu.Unlock()

		cnt := requestCount.Add(1)
		if cnt == 1 {
			// First attempt: return 429 with operating_reset_at in time block
			resp := jsonResponse(429, map[string]any{
				"error": "OPERATION_TIME_LIMIT",
				"time": map[string]any{
					"duration":           0.5,
					"operating":          360.5,
					"operating_reset_at": nowSec + 1,
				},
			})
			return resp, nil
		}
		// Second attempt succeeds
		return jsonResponse(200, map[string]any{
			"result": map[string]any{"id": 42},
			"time": map[string]any{
				"duration":           0.08,
				"operating":          10.2,
				"operating_reset_at": nowSec + 600,
			},
		}), nil
	})

	cfg, st, _, tm := newTestEnv(t, "", "client-id", rt)
	rc := bitrix.NewRestClient(cfg, tm, config.NewLogger(nil, "silent"), &http.Client{Transport: rt})

	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-time",
		BitrixUserID:   "1",
		ClientEndpoint: "https://example.bitrix24.com/rest/",
		AccessToken:    "secret",
		RefreshToken:   "refresh",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("UpsertConnection failed: %v", err)
	}

	start := time.Now()
	resp, err := rc.Call(ctx, conn.ID, "crm.deal.get", map[string]any{"id": 42}, bitrix.WithIdempotencyKey("uuid-idem-123"))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("rc.Call failed: %v", err)
	}
	if resp == nil || resp.Time == nil {
		t.Fatalf("expected non-nil resp and resp.Time")
	}
	if resp.Time.OperatingResetAt != nowSec+600 {
		t.Errorf("expected OperatingResetAt %d, got %d", nowSec+600, resp.Time.OperatingResetAt)
	}
	if elapsed < 500*time.Millisecond {
		t.Errorf("expected backoff waiting for operating_reset_at, elapsed: %v", elapsed)
	}

	mu.Lock()
	headers := idempotencyHeaders
	mu.Unlock()
	if len(headers) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(headers))
	}
	for i, h := range headers {
		if h != "uuid-idem-123" {
			t.Errorf("attempt %d missing or wrong Idempotency-Key: %q", i, h)
		}
	}
}

func TestRestClient_AutoFallbackToLegacyOn404OrMethodNotFound(t *testing.T) {
	ctx := context.Background()

	var requestedPaths []string
	var mu sync.Mutex

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		requestedPaths = append(requestedPaths, r.URL.Path)
		mu.Unlock()

		if strings.Contains(r.URL.Path, "/rest/api/crm.onpremise.html404") {
			// Older On-Premise returning non-JSON HTML 404
			return &http.Response{
				StatusCode: 404,
				Header:     http.Header{"Content-Type": []string{"text/html; charset=UTF-8"}},
				Body:       io.NopCloser(strings.NewReader("<!DOCTYPE html><html><head><title>404 Not Found</title></head><body><h1>404 Not Found</h1></body></html>")),
			}, nil
		}
		if strings.Contains(r.URL.Path, "/rest/api/crm.unknown.method") {
			return jsonResponse(404, map[string]any{
				"error": map[string]any{
					"code":    "BITRIX_REST_V3_EXCEPTION_UNKNOWNMETHOD",
					"message": "Method not found in REST 3.0",
				},
			}), nil
		}
		// Legacy fallback endpoint succeeds
		return jsonResponse(200, map[string]any{
			"result": map[string]any{"fallback": true},
		}), nil
	})

	cfg, st, _, tm := newTestEnv(t, "", "client-id", rt)
	rc := bitrix.NewRestClient(cfg, tm, config.NewLogger(nil, "silent"), &http.Client{Transport: rt})

	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "onpremise.example.com",
		MemberID:       "member-onprem",
		BitrixUserID:   "1",
		ClientEndpoint: "https://onpremise.example.com/rest/",
		AccessToken:    "token",
		RefreshToken:   "refresh",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("UpsertConnection failed: %v", err)
	}

	// 1. auto mode: REST 3.0 returns HTML 404 -> caches v2 in schema_cache and seamlessly executes legacy v2
	resp, err := rc.Call(ctx, conn.ID, "crm.onpremise.html404", nil)
	if err != nil {
		t.Fatalf("expected seamless fallback to v2, got error: %v", err)
	}
	if !strings.Contains(string(resp.Result), `"fallback":true`) {
		t.Errorf("expected fallback result, got: %s", string(resp.Result))
	}

	// Verify schema_cache cached "v2"
	var cachedVersion string
	found, err := st.GetSchemaCache(ctx, conn.ID, "api_version", 900, &cachedVersion)
	if err != nil || !found || cachedVersion != "v2" {
		t.Errorf("expected api_version='v2' in schema_cache, found=%v version=%q err=%v", found, cachedVersion, err)
	}

	// Subsequent call to another method should directly call v2 (path ending in .json)
	mu.Lock()
	beforeLen := len(requestedPaths)
	mu.Unlock()

	_, err = rc.Call(ctx, conn.ID, "crm.another.method", nil)
	if err != nil {
		t.Fatalf("subsequent call failed: %v", err)
	}
	mu.Lock()
	lastPath := requestedPaths[len(requestedPaths)-1]
	afterLen := len(requestedPaths)
	mu.Unlock()

	if afterLen-beforeLen != 1 {
		t.Errorf("expected exactly 1 request for subsequent call, got %d", afterLen-beforeLen)
	}
	if !strings.HasSuffix(lastPath, ".json") || strings.Contains(lastPath, "/rest/api/") {
		t.Errorf("expected direct Legacy REST call ending with .json, got: %s", lastPath)
	}

	// 2. Explicit WithAPIVersion("v3") does NOT fallback on 404 and returns error
	_, err = rc.Call(ctx, conn.ID, "crm.onpremise.html404", nil, bitrix.WithAPIVersion("v3"))
	if err == nil {
		t.Fatalf("expected error for explicit v3 on 404")
	}
}

func errorsAsAppError(err error, target **config.AppError) bool {
	return errors.As(err, target)
}

func TestSchemaDiscovery_HybridREST3_QuoteAndDictionaries(t *testing.T) {
	ctx := context.Background()

	// 1. Check EntitySlug mappings and built-in Quote
	for _, tc := range []struct {
		id       int
		wantSlug string
		wantCats bool
		wantStg  bool
	}{
		{1, "lead", false, true},
		{2, "deal", true, true},
		{3, "contact", false, false},
		{4, "company", false, false},
		{7, "quote", false, true},
		{31, "smartinvoice", true, true},
		{128, "smartprocess128", true, true},
	} {
		slug, err := bitrix.EntitySlug(tc.id)
		if err != nil || slug != tc.wantSlug {
			t.Errorf("EntitySlug(%d) = %q, want %q, err=%v", tc.id, slug, tc.wantSlug, err)
		}
		if bitrix.SupportsCategories(tc.id) != tc.wantCats {
			t.Errorf("SupportsCategories(%d) = %v, want %v", tc.id, bitrix.SupportsCategories(tc.id), tc.wantCats)
		}
		if bitrix.SupportsStages(tc.id) != tc.wantStg {
			t.Errorf("SupportsStages(%d) = %v, want %v", tc.id, bitrix.SupportsStages(tc.id), tc.wantStg)
		}
	}

	var recordedRequests []struct {
		Method string
		URL    string
		Body   map[string]any
		Raw    string
	}
	var mu sync.Mutex

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var bodyMap map[string]any
		var rawBody string
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			rawBody = string(b)
			_ = json.Unmarshal(b, &bodyMap)
		}

		mu.Lock()
		recordedRequests = append(recordedRequests, struct {
			Method string
			URL    string
			Body   map[string]any
			Raw    string
		}{
			Method: r.Method,
			URL:    r.URL.String(),
			Body:   bodyMap,
			Raw:    rawBody,
		})
		mu.Unlock()

		path := r.URL.Path

		// crm.type.list -> returns dynamic SPA 128
		if strings.HasSuffix(path, "crm.type.list.json") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"types": []map[string]any{
						{"entityTypeId": 128, "title": "Customer Request SPA"},
					},
				},
			}), nil
		}

		// REST 3.0 crm.deal.fields
		if strings.HasSuffix(path, "/rest/api/crm.deal.fields") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"fields": map[string]any{
						"TITLE": map[string]any{"type": "string", "title": "Deal Title"},
					},
				},
			}), nil
		}

		// REST 3.0 crm.quote.fields
		if strings.HasSuffix(path, "/rest/api/crm.quote.fields") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"fields": map[string]any{
						"TITLE": map[string]any{"type": "string", "title": "Quote Title"},
					},
				},
			}), nil
		}

		// REST 3.0 crm.deal.category.list
		if strings.HasSuffix(path, "/rest/api/crm.deal.category.list") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"items": []map[string]any{
						{"id": 0, "name": "General Deals"},
						{"id": 3, "name": "VIP Deals"},
					},
				},
			}), nil
		}

		// Legacy /rest/crm.category.list for Smart Invoice 31
		if strings.HasSuffix(path, "crm.category.list.json") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"categories": []map[string]any{
						{"id": 10, "name": "Invoice Funnel 1"},
					},
				},
			}), nil
		}

		// REST 3.0 crm.deal.category.stage.list with flat filter ["categoryId", "=", 0]
		if strings.HasSuffix(path, "/rest/api/crm.deal.category.stage.list") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"items": []map[string]any{
						{"statusId": "NEW", "name": "Initial Stage"},
					},
				},
			}), nil
		}

		// Legacy /rest/crm.status.list for Quote (QUOTE_STATUS) and Lead (STATUS)
		if strings.HasSuffix(path, "crm.status.list.json") {
			entityID := ""
			if f, ok := bodyMap["filter"].(map[string]any); ok {
				entityID, _ = f["ENTITY_ID"].(string)
			}
			return jsonResponse(200, map[string]any{
				"result": []map[string]any{
					{"STATUS_ID": entityID + "_1", "NAME": "Stage 1 for " + entityID},
				},
			}), nil
		}

		// REST 3.0 /rest/api/batch for Describe uncached categories
		if strings.Contains(path, "/rest/api/batch") {
			return jsonResponse(200, map[string]any{
				"result": []any{
					map[string]any{
						"items": []map[string]any{{"statusId": "NEW", "name": "New"}},
					},
					map[string]any{
						"items": []map[string]any{{"statusId": "C3:NEW", "name": "VIP New"}},
					},
				},
			}), nil
		}

		return jsonResponse(200, map[string]any{"result": true}), nil
	})

	cfg, st, _, tm := newTestEnv(t, "", "client-id", rt)
	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-schema-hybrid",
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

	rc := bitrix.NewRestClient(cfg, tm, config.NewLogger(nil, "silent"), &http.Client{Transport: rt})
	schema := bitrix.NewSchemaDiscovery(cfg, st, rc)

	// 2. ListEntityTypes sends start: -1 to crm.type.list and returns 6 built-ins + 1 SPA
	types, err := schema.ListEntityTypes(ctx, conn.ID, false)
	if err != nil {
		t.Fatalf("ListEntityTypes failed: %v", err)
	}
	if len(types) != 7 {
		t.Fatalf("expected 7 entity types (6 built-in including Quote + 1 dynamic), got %d", len(types))
	}
	mu.Lock()
	firstReq := recordedRequests[0]
	mu.Unlock()
	if firstReq.Body["start"] != -1 && firstReq.Body["start"] != float64(-1) {
		t.Errorf("expected start: -1 in crm.type.list request, got: %v", firstReq.Body["start"])
	}

	// 3. ListFields for Deal (2) calls REST 3.0 crm.deal.fields
	fieldsDeal, err := schema.Fields(ctx, conn.ID, 2, false)
	if err != nil {
		t.Fatalf("Fields(2) failed: %v", err)
	}
	if fieldsDeal["TITLE"] == nil {
		t.Errorf("expected TITLE field in fieldsDeal: %+v", fieldsDeal)
	}

	// 4. ListFields for Quote (7) calls REST 3.0 crm.quote.fields
	fieldsQuote, err := schema.Fields(ctx, conn.ID, 7, false)
	if err != nil {
		t.Fatalf("Fields(7) failed: %v", err)
	}
	if fieldsQuote["TITLE"] == nil {
		t.Errorf("expected TITLE field in fieldsQuote: %+v", fieldsQuote)
	}

	// 5. ListCategories:
	// Category-less entities (1 Lead, 3 Contact, 4 Company, 7 Quote) make 0 network requests
	mu.Lock()
	reqsBefore := len(recordedRequests)
	mu.Unlock()
	for _, catLessID := range []int{1, 3, 4, 7} {
		cats, err := schema.Categories(ctx, conn.ID, catLessID, false)
		if err != nil || len(cats) != 0 {
			t.Errorf("expected empty categories without error for %d, got %v err=%v", catLessID, cats, err)
		}
	}
	mu.Lock()
	reqsAfter := len(recordedRequests)
	mu.Unlock()
	if reqsAfter != reqsBefore {
		t.Errorf("expected 0 HTTP requests for category-less entities, made %d", reqsAfter-reqsBefore)
	}

	// Deals (2) calls REST 3.0 crm.deal.category.list
	catsDeal, err := schema.Categories(ctx, conn.ID, 2, false)
	if err != nil || len(catsDeal) != 2 {
		t.Fatalf("expected 2 deal categories, got %d err=%v", len(catsDeal), err)
	}

	// Smart Invoices (31) calls legacy crm.category.list
	catsInvoice, err := schema.Categories(ctx, conn.ID, 31, false)
	if err != nil || len(catsInvoice) != 1 {
		t.Fatalf("expected 1 invoice category via legacy endpoint, got %d err=%v", len(catsInvoice), err)
	}

	// 6. ListStages:
	// Deals (2) calls REST 3.0 crm.deal.category.stage.list with flat 3-element filter ["categoryId", "=", 0]
	stagesDeal, err := schema.Stages(ctx, conn.ID, 2, 0, false)
	if err != nil || len(stagesDeal) != 1 {
		t.Fatalf("expected 1 deal stage, got %d err=%v", len(stagesDeal), err)
	}
	mu.Lock()
	var lastStageReq struct {
		Method string
		URL    string
		Body   map[string]any
		Raw    string
	}
	for _, r := range recordedRequests {
		if strings.Contains(r.URL, "crm.deal.category.stage.list") {
			lastStageReq = r
		}
	}
	mu.Unlock()
	if filterSlice, ok := lastStageReq.Body["filter"].([]any); !ok || len(filterSlice) != 3 || filterSlice[0] != "categoryId" || filterSlice[1] != "=" {
		t.Errorf("expected flat 3-element filter [categoryId, =, X], got: %v (raw: %s)", lastStageReq.Body["filter"], lastStageReq.Raw)
	}

	// Quotes (7) calls legacy crm.status.list with ENTITY_ID: QUOTE_STATUS
	stagesQuote, err := schema.Stages(ctx, conn.ID, 7, 0, false)
	if err != nil || len(stagesQuote) != 1 {
		t.Fatalf("expected 1 quote stage, got %d err=%v", len(stagesQuote), err)
	}

	// Leads (1) calls legacy crm.status.list with ENTITY_ID: STATUS
	stagesLead, err := schema.Stages(ctx, conn.ID, 1, 0, false)
	if err != nil || len(stagesLead) != 1 {
		t.Fatalf("expected 1 lead stage, got %d err=%v", len(stagesLead), err)
	}

	// 7. Describe for Deals batches uncached categories via /rest/api/batch
	descDeal, err := schema.Describe(ctx, conn.ID, 2, nil, true)
	if err != nil {
		t.Fatalf("Describe(2) failed: %v", err)
	}
	if descDeal["entity"] == nil || descDeal["fields"] == nil || descDeal["categories"] == nil || descDeal["stages"] == nil {
		t.Errorf("incomplete Describe output: %+v", descDeal)
	}
}

func TestCrmService_REST3_ReadEngine(t *testing.T) {
	ctx := context.Background()

	// 1. Test CuratedSelect and normalization
	selDealEmpty := bitrix.CuratedSelect(2, nil)
	if !slices.Contains(selDealEmpty, "id") || !slices.Contains(selDealEmpty, "stage.name") || !slices.Contains(selDealEmpty, "company.title") {
		t.Errorf("expected relational fields in default Deal select, got: %v", selDealEmpty)
	}

	selCustom := bitrix.CuratedSelect(2, []string{"title", "stage.name", "ufCrmCustomCode"})
	if selCustom[0] != "id" {
		t.Errorf("expected 'id' injected as first field, got: %v", selCustom)
	}
	if !slices.Contains(selCustom, "UF_CRM_CUSTOMCODE") {
		t.Errorf("expected ufCrmCustomCode normalized to UF_CRM_CUSTOMCODE, got: %v", selCustom)
	}
	if !slices.Contains(selCustom, "stage.name") {
		t.Errorf("expected stage.name preserved in select, got: %v", selCustom)
	}

	// 2. Test TranslateFilter
	legacyFilter := map[string]any{
		"=STAGE_ID":     "NEW",
		">=OPPORTUNITY": 1000,
		"@ID":           []any{1, 2},
	}
	trans, hasSub := bitrix.TranslateFilter(legacyFilter)
	if hasSub {
		t.Errorf("expected hasSub=false for legacy filter without %%, got true")
	}
	transSlice, ok := trans.([]any)
	if !ok || len(transSlice) != 3 {
		t.Fatalf("expected 3 conditions in translated filter, got: %v", trans)
	}

	subFilter := map[string]any{"%TITLE": "Acme"}
	_, hasSub2 := bitrix.TranslateFilter(subFilter)
	if !hasSub2 {
		t.Errorf("expected hasSub=true for %%TITLE filter")
	}

	likeFilter := []any{[]any{"title", "like", "%Acme%"}}
	_, hasSub3 := bitrix.TranslateFilter(likeFilter)
	if !hasSub3 {
		t.Errorf("expected hasSub=true for like filter")
	}

	// 3. Set up mock HTTP transport for REST 3.0 calls
	var recordedRequests []struct {
		Method string
		URL    string
		Body   map[string]any
		Raw    string
	}
	var mu sync.Mutex

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var bodyMap map[string]any
		var rawBody string
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			rawBody = string(b)
			_ = json.Unmarshal(b, &bodyMap)
		}

		mu.Lock()
		recordedRequests = append(recordedRequests, struct {
			Method string
			URL    string
			Body   map[string]any
			Raw    string
		}{
			Method: r.Method,
			URL:    r.URL.String(),
			Body:   bodyMap,
			Raw:    rawBody,
		})
		mu.Unlock()

		path := r.URL.Path

		// REST 3.0 crm.deal.list
		if strings.HasSuffix(path, "/rest/api/crm.deal.list") {
			// Check if this is a bulk GetItems call with filter id in [10, 20]
			if fRaw, ok := bodyMap["filter"]; ok {
				fStr := fmt.Sprintf("%v", fRaw)
				if strings.Contains(fStr, "10") && strings.Contains(fStr, "20") {
					return jsonResponse(200, map[string]any{
						"result": map[string]any{
							"items": []map[string]any{
								{"id": 20, "title": "Deal 20"},
								{"id": 10, "title": "Deal 10"},
							},
						},
					}), nil
				}
			}
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"items": []map[string]any{
						{
							"id":       1,
							"title":    "Deal 1",
							"stage":    map[string]any{"name": "New Stage"},
							"category": map[string]any{"name": "General"},
							"company":  map[string]any{"title": "Acme Corp"},
						},
					},
				},
				"total": 1,
			}), nil
		}

		// REST 3.0 crm.deal.get
		if strings.HasSuffix(path, "/rest/api/crm.deal.get") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"item": map[string]any{
						"id":    42,
						"title": "Single Deal 42",
						"stage": map[string]any{"name": "In Progress"},
					},
				},
			}), nil
		}

		// REST 3.0 crm.deal.aggregate
		if strings.HasSuffix(path, "/rest/api/crm.deal.aggregate") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"count": map[string]any{"id": 150},
				},
			}), nil
		}

		// REST 3.0 crm.productrow.list
		if strings.HasSuffix(path, "/rest/api/crm.productrow.list") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"items": []map[string]any{
						{
							"id":          101,
							"ownerId":     1,
							"productId":   5,
							"productName": "Widget Pro",
							"price":       99.9,
						},
					},
				},
			}), nil
		}

		// Legacy /rest/crm.item.list.json fallback
		if strings.HasSuffix(path, "crm.item.list.json") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"items": []map[string]any{
						{"id": 999, "title": "Substring Matched Item"},
					},
				},
				"total": 1,
			}), nil
		}

		return jsonResponse(200, map[string]any{"result": true}), nil
	})

	cfg, st, _, tm := newTestEnv(t, "", "client-id", rt)
	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-crm-rest3",
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

	rc := bitrix.NewRestClient(cfg, tm, config.NewLogger(nil, "silent"), &http.Client{Transport: rt})
	crm := bitrix.NewCrmService(cfg, rc)

	// 4. SearchItems calls REST 3.0 crm.deal.list with curated select & single-call pagination
	searchRes, err := crm.SearchItems(ctx, bitrix.SearchItemsInput{
		ConnectionID: conn.ID,
		EntityTypeID: 2,
		Limit:        500,
	})
	if err != nil {
		t.Fatalf("SearchItems failed: %v", err)
	}
	if len(searchRes.Items) != 1 || searchRes.Items[0]["id"] != float64(1) && searchRes.Items[0]["id"] != 1 {
		t.Fatalf("unexpected items in searchRes: %+v", searchRes.Items)
	}
	mu.Lock()
	var lastSearchReq struct {
		Method string
		URL    string
		Body   map[string]any
		Raw    string
	}
	for _, req := range recordedRequests {
		if strings.Contains(req.URL, "crm.deal.list") {
			lastSearchReq = req
		}
	}
	mu.Unlock()
	if !strings.Contains(lastSearchReq.URL, "/rest/api/crm.deal.list") {
		t.Errorf("expected REST 3.0 crm.deal.list, got: %s", lastSearchReq.URL)
	}
	if pag, ok := lastSearchReq.Body["pagination"].(map[string]any); !ok || pag["limit"] != float64(500) && pag["limit"] != 500 {
		t.Errorf("expected pagination limit 500, got: %v", lastSearchReq.Body["pagination"])
	}
	if selList, ok := lastSearchReq.Body["select"].([]any); !ok || slices.Contains(selList, any("*")) {
		t.Errorf("expected curated select list without wildcard, got: %v", lastSearchReq.Body["select"])
	}

	// 5. SearchItems with substring filter falls back to legacy crm.item.list.json with start: -1
	subRes, err := crm.SearchItems(ctx, bitrix.SearchItemsInput{
		ConnectionID: conn.ID,
		EntityTypeID: 2,
		Filter:       map[string]any{"%TITLE": "Substring"},
		Limit:        50,
	})
	if err != nil {
		t.Fatalf("SearchItems with substring filter failed: %v", err)
	}
	if len(subRes.Items) != 1 || subRes.Items[0]["id"] != float64(999) && subRes.Items[0]["id"] != 999 {
		t.Fatalf("unexpected items in substring searchRes: %+v", subRes.Items)
	}
	mu.Lock()
	var lastLegacyReq struct {
		Method string
		URL    string
		Body   map[string]any
		Raw    string
	}
	for _, req := range recordedRequests {
		if strings.Contains(req.URL, "crm.item.list.json") {
			lastLegacyReq = req
		}
	}
	mu.Unlock()
	if !strings.Contains(lastLegacyReq.URL, "crm.item.list.json") {
		t.Errorf("expected legacy crm.item.list.json fallback for substring match, got: %s", lastLegacyReq.URL)
	}
	if lastLegacyReq.Body["start"] != -1 && lastLegacyReq.Body["start"] != float64(-1) {
		t.Errorf("expected start: -1 on legacy list query, got: %v", lastLegacyReq.Body["start"])
	}

	// 6. GetItem calls REST 3.0 crm.deal.get with explicit select
	getItemRes, err := crm.GetItem(ctx, conn.ID, 2, 42)
	if err != nil {
		t.Fatalf("GetItem failed: %v", err)
	}
	getItemMap, ok := getItemRes.(map[string]any)
	if !ok || (getItemMap["id"] != float64(42) && getItemMap["id"] != 42) {
		t.Fatalf("unexpected GetItem result: %+v", getItemRes)
	}
	mu.Lock()
	var lastGetReq struct {
		Method string
		URL    string
		Body   map[string]any
		Raw    string
	}
	for _, req := range recordedRequests {
		if strings.Contains(req.URL, "crm.deal.get") {
			lastGetReq = req
		}
	}
	mu.Unlock()
	if !strings.Contains(lastGetReq.URL, "/rest/api/crm.deal.get") {
		t.Errorf("expected REST 3.0 crm.deal.get, got: %s", lastGetReq.URL)
	}
	if selList, ok := lastGetReq.Body["select"].([]any); !ok || slices.Contains(selList, any("*")) {
		t.Errorf("expected explicit select list for GetItem, got: %v", lastGetReq.Body["select"])
	}

	// 7. GetItems calls REST 3.0 crm.deal.list with filter id in [10, 20] and preserves order
	getItemsRes, err := crm.GetItems(ctx, conn.ID, 2, []int{10, 20})
	if err != nil {
		t.Fatalf("GetItems failed: %v", err)
	}
	if getItemsRes["10"] == nil || getItemsRes["20"] == nil {
		t.Fatalf("expected items 10 and 20 in GetItems result, got: %+v", getItemsRes)
	}

	// 8. CountItems calls REST 3.0 crm.deal.aggregate
	countRes, err := crm.CountItems(ctx, bitrix.CountItemsInput{
		ConnectionID: conn.ID,
		EntityTypeID: 2,
		Filter:       map[string]any{"=STAGE_ID": "NEW"},
	})
	if err != nil {
		t.Fatalf("CountItems failed: %v", err)
	}
	if countRes != 150 {
		t.Errorf("expected count 150, got: %d", countRes)
	}
	mu.Lock()
	var lastAggReq struct {
		Method string
		URL    string
		Body   map[string]any
		Raw    string
	}
	for _, req := range recordedRequests {
		if strings.Contains(req.URL, "crm.deal.aggregate") {
			lastAggReq = req
		}
	}
	mu.Unlock()
	if !strings.Contains(lastAggReq.URL, "/rest/api/crm.deal.aggregate") {
		t.Errorf("expected REST 3.0 crm.deal.aggregate, got: %s", lastAggReq.URL)
	}

	// 9. CountItems with substring filter falls back to legacy crm.item.list.json total
	countSubRes, err := crm.CountItems(ctx, bitrix.CountItemsInput{
		ConnectionID: conn.ID,
		EntityTypeID: 2,
		Filter:       map[string]any{"%TITLE": "Substring"},
	})
	if err != nil {
		t.Fatalf("CountItems with substring filter failed: %v", err)
	}
	if countSubRes != 1 {
		t.Errorf("expected legacy fallback count 1, got: %d", countSubRes)
	}

	// 10. ListProductRows calls REST 3.0 crm.productrow.list with explicit select & limit 1000
	oID := 1
	prodRowsRes, err := crm.ListProductRows(ctx, bitrix.ListProductRowsInput{
		ConnectionID: conn.ID,
		OwnerID:      &oID,
		EntityTypeID: &oID,
	})
	if err != nil {
		t.Fatalf("ListProductRows failed: %v", err)
	}
	if len(prodRowsRes.ProductRows) != 1 || (prodRowsRes.ProductRows[0]["id"] != float64(101) && prodRowsRes.ProductRows[0]["id"] != 101) {
		t.Fatalf("unexpected ProductRows result: %+v", prodRowsRes.ProductRows)
	}
	mu.Lock()
	var lastProdReq struct {
		Method string
		URL    string
		Body   map[string]any
		Raw    string
	}
	for _, req := range recordedRequests {
		if strings.Contains(req.URL, "crm.productrow.list") {
			lastProdReq = req
		}
	}
	mu.Unlock()
	if !strings.Contains(lastProdReq.URL, "/rest/api/crm.productrow.list") {
		t.Errorf("expected REST 3.0 crm.productrow.list, got: %s", lastProdReq.URL)
	}
	if pag, ok := lastProdReq.Body["pagination"].(map[string]any); !ok || pag["limit"] != float64(1000) && pag["limit"] != 1000 {
		t.Errorf("expected pagination limit 1000 on productrow.list, got: %v", lastProdReq.Body["pagination"])
	}
}


