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
	var tokenSeq atomic.Int32
	shouldFailInvalidGrant := false

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seq := tokenSeq.Add(1)
		time.Sleep(25 * time.Millisecond)
		if shouldFailInvalidGrant {
			return jsonResponse(400, map[string]any{"error": "invalid_grant"}), nil
		}
		return jsonResponse(200, map[string]any{
			"access_token":    fmt.Sprintf("new-access-%d", seq),
			"refresh_token":   fmt.Sprintf("new-refresh-%d", seq),
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

	t.Run("deduplicates concurrent refresh into same token", func(t *testing.T) {
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
			if results[i] != "new-access-1" {
				t.Errorf("goroutine %d got %q, want deduplicated 'new-access-1'", i, results[i])
			}
		}
	})

	t.Run("marks connection revoked on invalid_grant", func(t *testing.T) {
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
	})
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
	if !errors.As(cbErr, &contErr) || contErr.McpRequestHash != "req-hash-cont" || contErr.OAuthErrorCode != "access_denied" {
		t.Errorf("unexpected ContinuationError: %+v", contErr)
	}
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
	var upstreamTitle atomic.Value
	upstreamTitle.Store("Smart Process")

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "crm.type.list.json"):
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"types": []map[string]any{
						{"entityTypeId": 128, "title": upstreamTitle.Load().(string)},
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
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"result": map[string]any{
						"cat_0": []map[string]any{{"STATUS_ID": "NEW", "NAME": "New"}},
						"cat_5": []map[string]any{{"STATUS_ID": "C5:NEW", "NAME": "Ent New"}},
					},
					"result_error": []any{},
				},
			}), nil
		case strings.HasPrefix(r.URL.Path, "/rest/api/"), strings.Contains(r.URL.Path, "crm.deal."):
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

	t.Run("lists and caches entity types until refresh", func(t *testing.T) {
		types1, err := schema.ListEntityTypes(ctx, conn.ID, false)
		if err != nil || len(types1) != 7 {
			t.Fatalf("expected 7 entity types (6 built-in + 1 dynamic), got %d err=%v", len(types1), err)
		}

		// Mutate upstream title; cached read must still return original "Smart Process"
		upstreamTitle.Store("Renamed Smart Process")
		resolved, err := schema.ResolveEntityType(ctx, conn.ID, "Smart Process")
		if err != nil || resolved["entityTypeId"] != 128 {
			t.Fatalf("expected cached entityTypeId 128 for 'Smart Process', got %+v err=%v", resolved, err)
		}
	})

	t.Run("describes entity with category stages", func(t *testing.T) {
		desc, err := schema.Describe(ctx, conn.ID, 2, nil, false)
		if err != nil {
			t.Fatalf("Describe failed: %v", err)
		}
		descJSON, _ := json.Marshal(desc)
		if !strings.Contains(string(descJSON), "C5:NEW") || !strings.Contains(string(descJSON), "NEW") {
			t.Errorf("expected NEW and C5:NEW in describe output: %s", string(descJSON))
		}
	})

	t.Run("refresh clears schema cache and observes updated upstream", func(t *testing.T) {
		refreshed, err := schema.Refresh(ctx, conn.ID)
		if err != nil || refreshed["clearedEntries"] == int64(0) {
			t.Errorf("expected non-zero clearedEntries on Refresh, got %+v err=%v", refreshed, err)
		}
		resolvedAfter, err := schema.ResolveEntityType(ctx, conn.ID, "Renamed Smart Process")
		if err != nil || resolvedAfter["entityTypeId"] != 128 {
			t.Fatalf("expected refreshed entityTypeId 128 for 'Renamed Smart Process', got %+v err=%v", resolvedAfter, err)
		}
	})
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

		case strings.HasPrefix(r.URL.Path, "/rest/api/"), strings.Contains(r.URL.Path, "crm.deal."):
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

	t.Run("SearchItems pagination and product rows", func(t *testing.T) {
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
		if rows, ok := searchRes.Items[0]["productRows"].([]map[string]any); !ok || len(rows) != 1 {
			t.Errorf("expected productRows attached to item 1, got %+v", searchRes.Items[0]["productRows"])
		}
	})

	t.Run("GetItem dynamic entity", func(t *testing.T) {
		itemRes, err := crmSvc.GetItem(ctx, conn.ID, 128, 100)
		if err != nil {
			t.Fatalf("GetItem failed: %v", err)
		}
		itemMap, ok := itemRes.(map[string]any)
		if !ok || itemMap["title"] != "Smart Item 100" {
			t.Errorf("expected item title 'Smart Item 100', got %+v", itemRes)
		}
	})

	t.Run("GetItems batch", func(t *testing.T) {
		batchItems, err := crmSvc.GetItems(ctx, conn.ID, 2, []int{1, 2})
		if err != nil {
			t.Fatalf("GetItems failed: %v", err)
		}
		if len(batchItems) != 2 {
			t.Fatalf("expected 2 items, got %d", len(batchItems))
		}
	})

	t.Run("ListUsers batch", func(t *testing.T) {
		users, err := crmSvc.ListUsers(ctx, conn.ID, map[string]any{"ID": "1"})
		if err != nil {
			t.Fatalf("ListUsers failed: %v", err)
		}
		if len(users) != 1 || users[0]["NAME"] != "John" {
			t.Fatalf("expected user John, got %+v", users)
		}
	})

	t.Run("upper-bound validation on legacy batch IDs and search limit", func(t *testing.T) {
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
	})
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
		case strings.HasPrefix(r.URL.Path, "/rest/api/"), strings.Contains(r.URL.Path, "crm.deal."):
			return jsonResponse(404, map[string]any{"error": "BITRIX_REST_V3_EXCEPTION_UNKNOWNMETHOD"}), nil
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
	opSvc := bitrix.NewOperationService(cfg, st, rc)

	t.Run("prepare, list, confirm, and prevent double confirm", func(t *testing.T) {
		prep, err := opSvc.PrepareCreate(ctx, conn.ID, 2, map[string]any{"TITLE": "New Big Deal"})
		if err != nil {
			t.Fatalf("PrepareCreate failed: %v", err)
		}
		opID, _ := prep["operation_id"].(string)
		if opID == "" || prep["requires_confirmation"] != true {
			t.Fatalf("expected pending operation, got %+v", prep)
		}

		list, err := st.ListPendingOperations(ctx, &conn.ID)
		if err != nil || len(list) != 1 {
			t.Fatalf("expected 1 pending operation in list, got %d, err=%v", len(list), err)
		}

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

		completedOp, err := st.GetPendingOperation(ctx, opID)
		if err != nil || completedOp.Status != store.OpStatusCompleted {
			t.Fatalf("expected operation %s to have status completed, got %+v err=%v", opID, completedOp, err)
		}
		if _, err := opSvc.Confirm(ctx, opID, ""); err == nil {
			t.Fatalf("expected second Confirm on completed operation to be rejected")
		}
	})

	t.Run("policy assertions enforce connection mode", func(t *testing.T) {
		if err := bitrix.AssertCanPrepareWrite(conn); err != nil {
			t.Fatalf("expected AssertCanPrepareWrite to allow write_with_confirmation, got %v", err)
		}
		if err := bitrix.AssertCanExecuteWrite(conn, false); err == nil {
			t.Fatalf("expected AssertCanExecuteWrite without confirmation to fail in write_with_confirmation mode")
		}
		if err := bitrix.AssertCanExecuteWrite(conn, true); err != nil {
			t.Fatalf("expected AssertCanExecuteWrite with confirmation to succeed, got %v", err)
		}
	})

	t.Run("cancel pending operation", func(t *testing.T) {
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
	})

	t.Run("blocks prepare in read_only mode", func(t *testing.T) {
		_, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
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

		if _, err := opSvc.PrepareCreate(ctx, conn.ID, 2, map[string]any{"TITLE": "Blocked Deal"}); err == nil {
			t.Fatalf("expected PrepareCreate to be blocked in read_only mode")
		}
	})
}

func TestRestClient_REST3AndLegacyRouting_OAuthAndWebhook(t *testing.T) {
	ctx := context.Background()

	type capturedReq struct {
		URL  string
		Auth any
		Raw  string
	}
	var lastReq capturedReq
	var mu sync.Mutex

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var bodyMap map[string]any
		_ = json.Unmarshal(bodyBytes, &bodyMap)

		mu.Lock()
		lastReq = capturedReq{
			URL:  r.URL.String(),
			Auth: bodyMap["auth"],
			Raw:  string(bodyBytes),
		}
		mu.Unlock()

		if strings.Contains(r.URL.Path, "batch") {
			if strings.Contains(r.URL.Path, "/api/") {
				// REST 3.0 batch response: json array (supports wrapped item {"result": ...})
				return jsonResponse(200, map[string]any{
					"result": []any{
						map[string]any{
							"result": map[string]any{"id": 1, "title": "Deal 1"},
						},
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

	getLastReq := func() capturedReq {
		mu.Lock()
		defer mu.Unlock()
		return lastReq
	}

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

	whCfg := &config.Config{
		BitrixWebhookURL: "https://example.bitrix24.com/rest/42/webhook-secret-code/",
		DefaultMode:      config.ModeReadOnly,
	}
	whConn, err := st.EnsureWebhookConnection(ctx, whCfg)
	if err != nil {
		t.Fatalf("EnsureWebhookConnection failed: %v", err)
	}

	t.Run("OAuth v3 single call", func(t *testing.T) {
		if _, err := rc.Call(ctx, oauthConn.ID, "crm.deal.get", map[string]any{"id": 10}, bitrix.WithAPIVersion(config.APIVersionV3)); err != nil {
			t.Fatalf("OAuth v3 Call failed: %v", err)
		}
		got := getLastReq()
		if got.URL != "https://example.bitrix24.com/rest/api/crm.deal.get" {
			t.Errorf("OAuth v3 single URL mismatch: %s", got.URL)
		}
		if got.Auth != "oauth-secret" {
			t.Errorf("OAuth v3 single expected auth in body, got: %v", got.Auth)
		}
	})

	t.Run("OAuth v2 single call", func(t *testing.T) {
		if _, err := rc.Call(ctx, oauthConn.ID, "crm.deal.get", map[string]any{"id": 10}, bitrix.WithAPIVersion(config.APIVersionV2)); err != nil {
			t.Fatalf("OAuth v2 Call failed: %v", err)
		}
		got := getLastReq()
		if got.URL != "https://example.bitrix24.com/rest/crm.deal.get.json" {
			t.Errorf("OAuth v2 single URL mismatch: %s", got.URL)
		}
		if got.Auth != "oauth-secret" {
			t.Errorf("OAuth v2 single expected auth in body, got: %v", got.Auth)
		}
	})

	t.Run("OAuth v3 batch puts auth and halt in query string and unwraps item result", func(t *testing.T) {
		res, err := rc.CallBatch(ctx, oauthConn.ID, []bitrix.NamedBatchCommand{
			{Key: "cmd0", Method: "crm.deal.get", Params: map[string]any{"id": 1}},
		}, true, bitrix.WithAPIVersion(config.APIVersionV3))
		if err != nil {
			t.Fatalf("OAuth v3 CallBatch failed: %v", err)
		}
		got := getLastReq()
		parsed, err := url.Parse(got.URL)
		if err != nil {
			t.Fatalf("invalid batch URL %q: %v", got.URL, err)
		}
		if parsed.Path != "/rest/api/batch" || parsed.Query().Get("auth") != "oauth-secret" || parsed.Query().Get("halt") != "1" {
			t.Errorf("OAuth v3 batch URL/query mismatch: %s", got.URL)
		}
		var dtoItems []map[string]any
		if err := json.Unmarshal([]byte(got.Raw), &dtoItems); err != nil || len(dtoItems) != 1 {
			t.Fatalf("expected 1 DTO item in JSON array body, got %s (err=%v)", got.Raw, err)
		}
		if q, ok := dtoItems[0]["query"].(map[string]any); !ok || q["auth"] != nil || q["halt"] != nil {
			t.Errorf("item query must not contain auth or halt: %+v", dtoItems[0])
		}
		var unwrapped map[string]any
		if err := json.Unmarshal(res["cmd0"], &unwrapped); err != nil || unwrapped["title"] != "Deal 1" {
			t.Errorf("expected unwrapped item result for cmd0, got %s (err=%v)", string(res["cmd0"]), err)
		}
	})

	t.Run("OAuth v2 batch", func(t *testing.T) {
		if _, err := rc.CallBatch(ctx, oauthConn.ID, []bitrix.NamedBatchCommand{
			{Key: "cmd0", Method: "crm.deal.get", Params: map[string]any{"id": 1}},
		}, false, bitrix.WithAPIVersion(config.APIVersionV2)); err != nil {
			t.Fatalf("OAuth v2 CallBatch failed: %v", err)
		}
		got := getLastReq()
		if got.URL != "https://example.bitrix24.com/rest/batch.json" || got.Auth != "oauth-secret" {
			t.Errorf("OAuth v2 batch mismatch: url=%s auth=%v", got.URL, got.Auth)
		}
	})

	t.Run("Webhook v3 single call", func(t *testing.T) {
		if _, err := rc.Call(ctx, whConn.ID, "crm.deal.get", map[string]any{"id": 10}, bitrix.WithAPIVersion(config.APIVersionV3)); err != nil {
			t.Fatalf("Webhook v3 Call failed: %v", err)
		}
		got := getLastReq()
		if got.URL != "https://example.bitrix24.com/rest/42/webhook-secret-code/api/crm.deal.get" {
			t.Errorf("Webhook v3 single URL mismatch: %s", got.URL)
		}
		if got.Auth != nil {
			t.Errorf("Webhook v3 single must NOT contain auth in body, got: %v", got.Auth)
		}
	})

	t.Run("Webhook v2 single call", func(t *testing.T) {
		if _, err := rc.Call(ctx, whConn.ID, "crm.deal.get", map[string]any{"id": 10}, bitrix.WithAPIVersion(config.APIVersionV2)); err != nil {
			t.Fatalf("Webhook v2 Call failed: %v", err)
		}
		got := getLastReq()
		if got.URL != "https://example.bitrix24.com/rest/42/webhook-secret-code/crm.deal.get.json" || got.Auth != nil {
			t.Errorf("Webhook v2 single mismatch: url=%s auth=%v", got.URL, got.Auth)
		}
	})

	t.Run("Webhook v3 batch", func(t *testing.T) {
		if _, err := rc.CallBatch(ctx, whConn.ID, []bitrix.NamedBatchCommand{
			{Key: "cmd0", Method: "crm.deal.get", Params: map[string]any{"id": 1}},
		}, true, bitrix.WithAPIVersion(config.APIVersionV3)); err != nil {
			t.Fatalf("Webhook v3 CallBatch failed: %v", err)
		}
		got := getLastReq()
		if got.URL != "https://example.bitrix24.com/rest/42/webhook-secret-code/api/batch?halt=1" {
			t.Errorf("Webhook v3 batch URL mismatch: %s", got.URL)
		}
		if !strings.HasPrefix(strings.TrimSpace(got.Raw), "[") {
			t.Errorf("Webhook v3 batch expected JSON array body, got: %s", got.Raw)
		}
	})

	t.Run("Webhook v2 batch", func(t *testing.T) {
		if _, err := rc.CallBatch(ctx, whConn.ID, []bitrix.NamedBatchCommand{
			{Key: "cmd0", Method: "crm.deal.get", Params: map[string]any{"id": 1}},
		}, false, bitrix.WithAPIVersion(config.APIVersionV2)); err != nil {
			t.Fatalf("Webhook v2 CallBatch failed: %v", err)
		}
		got := getLastReq()
		if got.URL != "https://example.bitrix24.com/rest/42/webhook-secret-code/batch.json" || got.Auth != nil {
			t.Errorf("Webhook v2 batch mismatch: url=%s auth=%v", got.URL, got.Auth)
		}
	})
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

	t.Run("legacy error string decoded", func(t *testing.T) {
		_, err := rc.Call(ctx, conn.ID, "legacy_err", nil)
		if err == nil {
			t.Fatalf("expected error for legacy_err")
		}
		var appErr *config.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *config.AppError, got %T: %v", err, err)
		}
		if appErr.Code != "ERROR_LEGACY" {
			t.Errorf("expected code 'ERROR_LEGACY', got %q", appErr.Code)
		}
	})

	t.Run("REST 3.0 nested error with validation items decoded", func(t *testing.T) {
		_, err := rc.Call(ctx, conn.ID, "rest3_validation", nil)
		if err == nil {
			t.Fatalf("expected error for rest3_validation")
		}
		var appErr *config.AppError
		if !errors.As(err, &appErr) {
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
	})

	t.Run("REST 3.0 nested expired_token triggers OAuth token refresh", func(t *testing.T) {
		if _, err := rc.Call(ctx, conn.ID, "rest3_expired", nil); err != nil {
			t.Fatalf("expected successful call after token refresh, got: %v", err)
		}
		updatedConn, err := st.GetConnection(ctx, conn.ID)
		if err != nil || updatedConn.AccessToken != "refreshed-secret" {
			t.Errorf("expected access token to be refreshed to 'refreshed-secret', got %+v err=%v", updatedConn, err)
		}
	})
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
			// First attempt: return 429 with operating_reset_at in time block (+2s avoids integer-second boundary rollover)
			resp := jsonResponse(429, map[string]any{
				"error": "OPERATION_TIME_LIMIT",
				"time": map[string]any{
					"duration":           0.5,
					"operating":          360.5,
					"operating_reset_at": time.Now().Unix() + 2,
				},
			})
			return resp, nil
		}
		// Second attempt succeeds with high operating time (>= 400s) to trigger proactive pacing on the next call
		if cnt == 2 {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{"id": 42},
				"time": map[string]any{
					"duration":           0.08,
					"operating":          420.5,
					"operating_reset_at": nowSec + 600,
				},
			}), nil
		}
		// Third attempt (after operating pacing) succeeds
		return jsonResponse(200, map[string]any{
			"result": map[string]any{"id": 43},
			"time": map[string]any{
				"duration":           0.05,
				"operating":          12.0,
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
	headers := slices.Clone(idempotencyHeaders)
	mu.Unlock()
	for i, h := range headers {
		if h != "uuid-idem-123" {
			t.Errorf("attempt %d missing or wrong Idempotency-Key: %q", i, h)
		}
	}

	// Next call on the same connection should be proactively paced because previous time.operating >= 400
	paceStart := time.Now()
	_, err = rc.Call(ctx, conn.ID, "crm.deal.get", map[string]any{"id": 43})
	paceElapsed := time.Since(paceStart)
	if err != nil {
		t.Fatalf("paced rc.Call failed: %v", err)
	}
	if paceElapsed < 150*time.Millisecond {
		t.Errorf("expected proactive operating pacing delay (>=150ms), got %v", paceElapsed)
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

		// Connection 1 (onpremise.example.com): returns HTML 404 on /rest/api/*
		if r.URL.Host == "onpremise.example.com" && strings.Contains(r.URL.Path, "/rest/api/") {
			return &http.Response{
				StatusCode: 404,
				Header:     http.Header{"Content-Type": []string{"text/html; charset=UTF-8"}},
				Body:       io.NopCloser(strings.NewReader("<!DOCTYPE html><html><head><title>404 Not Found</title></head><body><h1>404 Not Found</h1></body></html>")),
			}, nil
		}
		if r.URL.Host == "onpremise.example.com" && strings.HasSuffix(r.URL.Path, "crm.item.get.json") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"item": map[string]any{"id": 77, "title": "Legacy OnPrem Deal"},
				},
			}), nil
		}

		// Connection 2 (partialv3.example.com): supports crm.deal.get in v3, but returns UNKNOWNMETHOD on crm.deal.aggregate and crm.quote.productrow.list
		if r.URL.Host == "partialv3.example.com" && (strings.HasSuffix(r.URL.Path, "/rest/api/crm.deal.aggregate") || strings.HasSuffix(r.URL.Path, "/rest/api/crm.quote.productrow.list")) {
			return jsonResponse(404, map[string]any{
				"error": map[string]any{
					"code":    "BITRIX_REST_V3_EXCEPTION_UNKNOWNMETHOD",
					"message": "Method not found in REST 3.0",
				},
			}), nil
		}
		if r.URL.Host == "partialv3.example.com" && strings.HasSuffix(r.URL.Path, "crm.item.list.json") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{"items": []any{}},
				"total":  19,
			}), nil
		}
		if r.URL.Host == "partialv3.example.com" && strings.HasSuffix(r.URL.Path, "crm.item.productrow.list.json") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{"productRows": []any{}},
			}), nil
		}
		if r.URL.Host == "partialv3.example.com" && strings.HasSuffix(r.URL.Path, "/rest/api/crm.deal.get") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"item": map[string]any{"id": 88, "title": "Still REST 3.0 Deal"},
				},
			}), nil
		}

		// Legacy-only crm.type.list
		if strings.HasSuffix(r.URL.Path, "crm.type.list.json") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{"types": []any{}},
			}), nil
		}

		return jsonResponse(200, map[string]any{
			"result": map[string]any{"ok": true},
		}), nil
	})

	cfg, st, _, tm := newTestEnv(t, "", "client-id", rt)
	rc := bitrix.NewRestClient(cfg, tm, config.NewLogger(nil, "silent"), &http.Client{Transport: rt})
	crmSvc := bitrix.NewCrmService(cfg, rc)

	connOnPrem, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
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

	connPartial, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "partialv3.example.com",
		MemberID:       "member-partial-v3",
		BitrixUserID:   "1",
		ClientEndpoint: "https://partialv3.example.com/rest/",
		AccessToken:    "token",
		RefreshToken:   "refresh",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("UpsertConnection failed: %v", err)
	}

	t.Run("auto mode HTML 404 falls back to v2 and caches portal api_version", func(t *testing.T) {
		itemRaw, err := crmSvc.GetItem(ctx, connOnPrem.ID, 2, 77)
		if err != nil {
			t.Fatalf("expected seamless fallback to v2 crm.item.get, got error: %v", err)
		}
		itemMap, ok := itemRaw.(map[string]any)
		if !ok || itemMap["title"] != "Legacy OnPrem Deal" {
			t.Errorf("expected Legacy OnPrem Deal from v2 fallback, got: %+v", itemRaw)
		}

		var cachedVersion string
		found, err := st.GetSchemaCache(ctx, connOnPrem.ID, "api_version", 900, &cachedVersion)
		if err != nil || !found || cachedVersion != "v2" {
			t.Errorf("expected api_version='v2' in schema_cache, found=%v version=%q err=%v", found, cachedVersion, err)
		}

		mu.Lock()
		requestedPaths = nil
		mu.Unlock()

		if _, err := crmSvc.GetItem(ctx, connOnPrem.ID, 2, 77); err != nil {
			t.Fatalf("subsequent GetItem failed: %v", err)
		}
		mu.Lock()
		paths := slices.Clone(requestedPaths)
		mu.Unlock()

		for _, p := range paths {
			if strings.Contains(p, "/rest/api/") {
				t.Errorf("cached v2 connection must not hit /rest/api/, got path %q", p)
			}
		}
	})

	t.Run("partial REST 3.0 portal falls back locally without poisoning portal api_version", func(t *testing.T) {
		total, err := crmSvc.CountItems(ctx, bitrix.CountItemsInput{
			ConnectionID: connPartial.ID,
			EntityTypeID: 2,
		})
		if err != nil || total != 19 {
			t.Fatalf("expected CountItems local fallback to return 19, got %d err=%v", total, err)
		}

		quoteType := bitrix.EntityTypeQuote
		ownerID := 5
		if _, err := crmSvc.ListProductRows(ctx, bitrix.ListProductRowsInput{
			ConnectionID: connPartial.ID,
			EntityTypeID: &quoteType,
			OwnerID:      &ownerID,
		}); err != nil {
			t.Fatalf("expected ListProductRows local fallback on .productrow.list to succeed, got %v", err)
		}

		var cachedPartial string
		foundPartial, _ := st.GetSchemaCache(ctx, connPartial.ID, "api_version", 900, &cachedPartial)
		if foundPartial {
			t.Errorf("expected local-only fallback NOT to cache api_version='v2', got %q", cachedPartial)
		}

		v3Item, err := crmSvc.GetItem(ctx, connPartial.ID, 2, 88)
		if err != nil {
			t.Fatalf("GetItem on connPartial failed: %v", err)
		}
		if m, ok := v3Item.(map[string]any); !ok || m["title"] != "Still REST 3.0 Deal" {
			t.Errorf("expected REST 3.0 deal on connPartial, got: %+v", v3Item)
		}
	})

	t.Run("explicit BITRIX_API_VERSION=v3 fails without v2 fallback except legacy-only methods", func(t *testing.T) {
		cfgV3 := *cfg
		cfgV3.BitrixAPIVersion = config.APIVersionV3
		rcV3 := bitrix.NewRestClient(&cfgV3, tm, config.NewLogger(nil, "silent"), &http.Client{Transport: rt})
		schemaV3 := bitrix.NewSchemaDiscovery(&cfgV3, st, rcV3)
		crmV3 := bitrix.NewCrmService(&cfgV3, rcV3)

		if _, err := schemaV3.ListEntityTypes(ctx, connPartial.ID, true); err != nil {
			t.Fatalf("expected ListEntityTypes (crm.type.list) to route to legacy endpoint even in BITRIX_API_VERSION=v3, got err: %v", err)
		}
		if _, err := crmV3.GetItem(ctx, connOnPrem.ID, 2, 77); err == nil {
			t.Fatalf("expected error in BITRIX_API_VERSION=v3 when /rest/api/crm.deal.get returns 404")
		}
	})
}

func TestSchemaDiscovery_HybridREST3_QuoteAndDictionaries(t *testing.T) {
	ctx := context.Background()

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

		// REST 3.0 crm.deal.field.list
		if strings.HasSuffix(path, "/rest/api/crm.deal.field.list") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"items": []map[string]any{
						{"name": "TITLE", "type": "string", "title": "Deal Title"},
					},
				},
			}), nil
		}

		// REST 3.0 crm.quote.field.list
		if strings.HasSuffix(path, "/rest/api/crm.quote.field.list") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"items": []map[string]any{
						{"name": "TITLE", "type": "string", "title": "Quote Title"},
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

		// REST 3.0 /rest/api/batch for Describe uncached categories (wrapped item result)
		if strings.Contains(path, "/rest/api/batch") {
			return jsonResponse(200, map[string]any{
				"result": []any{
					map[string]any{
						"result": map[string]any{
							"items": []map[string]any{{"statusId": "NEW", "name": "New"}},
						},
					},
					map[string]any{
						"result": map[string]any{
							"items": []map[string]any{{"statusId": "C3:NEW", "name": "VIP New"}},
						},
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

	t.Run("ListEntityTypes merges built-in and dynamic SPA with start -1", func(t *testing.T) {
		types, err := schema.ListEntityTypes(ctx, conn.ID, false)
		if err != nil {
			t.Fatalf("ListEntityTypes failed: %v", err)
		}
		if len(types) != 7 {
			t.Fatalf("expected 7 entity types (6 built-in including Quote + 1 dynamic), got %d", len(types))
		}
		wantMeta := map[int]struct {
			code string
			slug string
			cats bool
			stgs bool
		}{
			1:   {"LEAD", "lead", false, true},
			2:   {"DEAL", "deal", true, true},
			3:   {"CONTACT", "contact", false, false},
			4:   {"COMPANY", "company", false, false},
			7:   {"QUOTE", "quote", false, true},
			31:  {"SMART_INVOICE", "smartinvoice", true, true},
			128: {"DYNAMIC_128", "smartprocess128", true, true},
		}
		for _, et := range types {
			id, _ := et["entityTypeId"].(int)
			if id == 0 {
				if f, ok := et["entityTypeId"].(float64); ok {
					id = int(f)
				}
			}
			exp, ok := wantMeta[id]
			if !ok {
				t.Errorf("unexpected entityTypeId %d in ListEntityTypes: %+v", id, et)
				continue
			}
			if et["code"] != exp.code || et["slug"] != exp.slug || et["supportsCategories"] != exp.cats || et["supportsStages"] != exp.stgs {
				t.Errorf("entity %d metadata mismatch: got %+v, want %+v", id, et, exp)
			}
		}
		mu.Lock()
		firstReq := recordedRequests[0]
		mu.Unlock()
		if firstReq.Body["start"] != -1 && firstReq.Body["start"] != float64(-1) {
			t.Errorf("expected start: -1 in crm.type.list request, got: %v", firstReq.Body["start"])
		}
	})

	t.Run("Fields calls REST 3.0 crm.<slug>.field.list and indexes items by name", func(t *testing.T) {
		fieldsDeal, err := schema.Fields(ctx, conn.ID, 2, false)
		if err != nil {
			t.Fatalf("Fields(2) failed: %v", err)
		}
		if fieldsDeal["TITLE"] == nil {
			t.Errorf("expected TITLE field in fieldsDeal: %+v", fieldsDeal)
		}

		fieldsQuote, err := schema.Fields(ctx, conn.ID, 7, false)
		if err != nil {
			t.Fatalf("Fields(7) failed: %v", err)
		}
		if fieldsQuote["TITLE"] == nil {
			t.Errorf("expected TITLE field in fieldsQuote: %+v", fieldsQuote)
		}
	})

	t.Run("Categories handles category-less entities, REST 3.0 deals, and legacy smart invoices", func(t *testing.T) {
		mu.Lock()
		recordedRequests = nil
		mu.Unlock()

		for _, catLessID := range []int{1, 3, 4, 7} {
			cats, err := schema.Categories(ctx, conn.ID, catLessID, false)
			if err != nil || len(cats) != 0 {
				t.Errorf("expected empty categories without error for %d, got %v err=%v", catLessID, cats, err)
			}
		}
		mu.Lock()
		catLessReqs := slices.Clone(recordedRequests)
		mu.Unlock()
		for _, r := range catLessReqs {
			if strings.Contains(r.URL, "category") {
				t.Errorf("category-less entity unexpectedly requested category endpoint: %s", r.URL)
			}
		}

		catsDeal, err := schema.Categories(ctx, conn.ID, 2, false)
		if err != nil || len(catsDeal) != 2 {
			t.Fatalf("expected 2 deal categories, got %d err=%v", len(catsDeal), err)
		}

		catsInvoice, err := schema.Categories(ctx, conn.ID, 31, false)
		if err != nil || len(catsInvoice) != 1 {
			t.Fatalf("expected 1 invoice category via legacy endpoint, got %d err=%v", len(catsInvoice), err)
		}
	})

	t.Run("Stages routes REST 3.0 deal stages and legacy status dictionaries", func(t *testing.T) {
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

		stagesQuote, err := schema.Stages(ctx, conn.ID, 7, 0, false)
		if err != nil || len(stagesQuote) != 1 {
			t.Fatalf("expected 1 quote stage, got %d err=%v", len(stagesQuote), err)
		}

		stagesLead, err := schema.Stages(ctx, conn.ID, 1, 0, false)
		if err != nil || len(stagesLead) != 1 {
			t.Fatalf("expected 1 lead stage, got %d err=%v", len(stagesLead), err)
		}
	})

	t.Run("Describe batches uncached categories via REST 3.0 batch and unwraps item results", func(t *testing.T) {
		descDeal, err := schema.Describe(ctx, conn.ID, 2, nil, true)
		if err != nil {
			t.Fatalf("Describe(2) failed: %v", err)
		}
		if descDeal["entity"] == nil || descDeal["fields"] == nil || descDeal["categories"] == nil || descDeal["stages"] == nil {
			t.Errorf("incomplete Describe output: %+v", descDeal)
		}
		descJSON, _ := json.Marshal(descDeal["stages"])
		if !strings.Contains(string(descJSON), "C3:NEW") {
			t.Errorf("expected unwrapped batch stage C3:NEW in Describe stages, got %s", string(descJSON))
		}
	})
}

func TestCrmService_REST3_ReadEngine(t *testing.T) {
	ctx := context.Background()

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
			if fRaw, ok := bodyMap["filter"]; ok {
				fStr := fmt.Sprintf("%v", fRaw)
				if strings.Contains(fStr, "10") && strings.Contains(fStr, "20") && !strings.Contains(fStr, "300") {
					return jsonResponse(200, map[string]any{
						"result": map[string]any{
							"items": []map[string]any{
								{"id": 20, "title": "Deal 20"},
								{"id": 10, "title": "Deal 10"},
							},
						},
					}), nil
				}
				if strings.Contains(fStr, "99999") {
					return jsonResponse(200, map[string]any{
						"result": map[string]any{
							"items": []map[string]any{},
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

		// REST 3.0 crm.deal.aggregate (real nested result.result.count.id envelope)
		if strings.HasSuffix(path, "/rest/api/crm.deal.aggregate") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"result": map[string]any{
						"count": map[string]any{"id": 150},
					},
				},
			}), nil
		}

		// REST 3.0 crm.deal.productrow.list
		if strings.HasSuffix(path, "/rest/api/crm.deal.productrow.list") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{
					"items": []map[string]any{
						{
							"id":          101,
							"ownerId":     1,
							"productId":   5,
							"productName": "Widget Pro",
							"price":       99.9,
							"quantity":    2,
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

	t.Run("SearchItems single-call REST 3.0 pagination and curated select", func(t *testing.T) {
		searchRes, err := crm.SearchItems(ctx, bitrix.SearchItemsInput{
			ConnectionID: conn.ID,
			EntityTypeID: 2,
			Limit:        500,
			Page:         2,
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
		if pag, ok := lastSearchReq.Body["pagination"].(map[string]any); !ok || (pag["limit"] != float64(500) && pag["limit"] != 500) || (pag["page"] != float64(2) && pag["page"] != 2) {
			t.Errorf("expected pagination limit 500 and page 2, got: %v", lastSearchReq.Body["pagination"])
		}
		selList, ok := lastSearchReq.Body["select"].([]any)
		if !ok || slices.Contains(selList, any("*")) || !slices.Contains(selList, any("stage.name")) || !slices.Contains(selList, any("company.title")) {
			t.Errorf("expected curated select list with relational fields and without wildcard, got: %v", lastSearchReq.Body["select"])
		}
	})

	t.Run("SearchItems custom select normalizes ufCrm and preserves logical filter group", func(t *testing.T) {
		_, err := crm.SearchItems(ctx, bitrix.SearchItemsInput{
			ConnectionID: conn.ID,
			EntityTypeID: 2,
			Select:       []string{"title", "stage.name", "ufCrmCustomCode"},
			Filter: map[string]any{
				"type":  "filter",
				"logic": "or",
				"conditions": []any{
					[]any{"stageId", "=", "NEW"},
					[]any{"opportunity", ">=", 1000},
				},
			},
			Limit: 25,
		})
		if err != nil {
			t.Fatalf("SearchItems with logical group filter failed: %v", err)
		}
		mu.Lock()
		lastSearchReq := recordedRequests[len(recordedRequests)-1]
		mu.Unlock()
		customSel, _ := lastSearchReq.Body["select"].([]any)
		if len(customSel) == 0 || customSel[0] != "id" || !slices.Contains(customSel, any("UF_CRM_CUSTOMCODE")) || !slices.Contains(customSel, any("stage.name")) {
			t.Errorf("expected ['id', 'title', 'stage.name', 'UF_CRM_CUSTOMCODE'], got: %v", customSel)
		}
		groupFilter, ok := lastSearchReq.Body["filter"].(map[string]any)
		if !ok || groupFilter["type"] != "filter" || groupFilter["logic"] != "or" {
			t.Errorf("expected logical filter group preserved as map in request body, got: %v", lastSearchReq.Body["filter"])
		}
	})

	t.Run("SearchItems substring filter falls back to legacy crm.item.list.json", func(t *testing.T) {
		for _, subFilter := range []any{
			map[string]any{"%TITLE": "Substring"},
			[]any{[]any{"title", "like", "%Substring%"}},
		} {
			subRes, err := crm.SearchItems(ctx, bitrix.SearchItemsInput{
				ConnectionID: conn.ID,
				EntityTypeID: 2,
				Filter:       subFilter,
				Limit:        50,
			})
			if err != nil {
				t.Fatalf("SearchItems with substring filter %v failed: %v", subFilter, err)
			}
			if len(subRes.Items) != 1 || subRes.Items[0]["id"] != float64(999) && subRes.Items[0]["id"] != 999 {
				t.Fatalf("unexpected items in substring searchRes: %+v", subRes.Items)
			}
			mu.Lock()
			lastLegacyReq := recordedRequests[len(recordedRequests)-1]
			mu.Unlock()
			if !strings.Contains(lastLegacyReq.URL, "crm.item.list.json") {
				t.Errorf("expected legacy crm.item.list.json fallback for substring match, got: %s", lastLegacyReq.URL)
			}
			if lastLegacyReq.Body["start"] != -1 && lastLegacyReq.Body["start"] != float64(-1) {
				t.Errorf("expected start: -1 on legacy list query, got: %v", lastLegacyReq.Body["start"])
			}
			legacyFilterMap, ok := lastLegacyReq.Body["filter"].(map[string]any)
			if !ok || (legacyFilterMap["%TITLE"] == nil && legacyFilterMap["%title"] == nil) {
				t.Errorf("expected %%TITLE or %%title in legacy fallback filter, got: %v", lastLegacyReq.Body["filter"])
			}
		}
	})

	t.Run("GetItem and GetItems REST 3.0 ordering, empty slice, and bulk single-request", func(t *testing.T) {
		getItemRes, err := crm.GetItem(ctx, conn.ID, 2, 42, []string{"title", "stage.name", "ufCrmCustomCode"})
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
		if selList, ok := lastGetReq.Body["select"].([]any); !ok || !slices.Contains(selList, any("UF_CRM_CUSTOMCODE")) || !slices.Contains(selList, any("stage.name")) {
			t.Errorf("expected custom select list for GetItem, got: %v", lastGetReq.Body["select"])
		}

		getItemsRes, err := crm.GetItems(ctx, conn.ID, 2, []int{10, 20}, []string{"title", "stage.name"})
		if err != nil {
			t.Fatalf("GetItems failed: %v", err)
		}
		if len(getItemsRes) != 2 {
			t.Fatalf("expected 2 items in GetItems result, got: %+v", getItemsRes)
		}
		if (getItemsRes[0]["id"] != float64(10) && getItemsRes[0]["id"] != 10) || (getItemsRes[1]["id"] != float64(20) && getItemsRes[1]["id"] != 20) {
			t.Errorf("expected GetItems to preserve requested order [10, 20], got: %+v", getItemsRes)
		}

		emptyItemsRes, err := crm.GetItems(ctx, conn.ID, 2, []int{99999})
		if err != nil || len(emptyItemsRes) != 0 {
			t.Errorf("expected empty slice for 0 matching items, got %v err=%v", emptyItemsRes, err)
		}

		manyIDs := make([]int, 300)
		for i := range manyIDs {
			manyIDs[i] = i + 1
		}
		mu.Lock()
		recordedRequests = nil
		mu.Unlock()

		if _, err := crm.FetchItems(ctx, bitrix.GetItemInput{
			ConnectionID:           conn.ID,
			EntityTypeID:           2,
			IDs:                    manyIDs,
			IncludeDetails:         true,
			IncludeDetailsExplicit: false,
		}); err != nil {
			t.Errorf("expected FetchItems in REST 3.0 mode to allow 300 IDs (up to 1000), got err: %v", err)
		}
		mu.Lock()
		bulkReqs := slices.Clone(recordedRequests)
		mu.Unlock()
		for _, r := range bulkReqs {
			if strings.Contains(r.URL, "productrow") || strings.Contains(r.URL, "crm.item.get") {
				t.Errorf("bulk >50 IDs without explicit includeDetails must not trigger N+1 detail calls: %s", r.URL)
			}
		}
	})

	t.Run("CountItems unwraps nested REST 3.0 aggregate count and falls back on substring filter", func(t *testing.T) {
		countRes, err := crm.CountItems(ctx, bitrix.CountItemsInput{
			ConnectionID: conn.ID,
			EntityTypeID: 2,
			Filter:       map[string]any{"=STAGE_ID": "NEW"},
		})
		if err != nil {
			t.Fatalf("CountItems failed: %v", err)
		}
		if countRes != 150 {
			t.Errorf("expected count 150 from nested result.result.count.id, got: %d", countRes)
		}

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
	})

	t.Run("ListProductRows calls REST 3.0 crm.<slug>.productrow.list without ownerType in filter", func(t *testing.T) {
		oID := 1
		dealEntity := bitrix.EntityTypeDeal
		prodRowsRes, err := crm.ListProductRows(ctx, bitrix.ListProductRowsInput{
			ConnectionID: conn.ID,
			OwnerID:      &oID,
			EntityTypeID: &dealEntity,
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
			if strings.Contains(req.URL, "crm.deal.productrow.list") {
				lastProdReq = req
			}
		}
		mu.Unlock()
		if !strings.Contains(lastProdReq.URL, "/rest/api/crm.deal.productrow.list") {
			t.Errorf("expected REST 3.0 crm.deal.productrow.list, got: %s", lastProdReq.URL)
		}
		if strings.Contains(lastProdReq.Raw, "ownerType") {
			t.Errorf("REST 3.0 crm.<slug>.productrow.list filter must NOT include ownerType: %s", lastProdReq.Raw)
		}
		if pag, ok := lastProdReq.Body["pagination"].(map[string]any); !ok || pag["limit"] != float64(1000) && pag["limit"] != 1000 {
			t.Errorf("expected pagination limit 1000 on productrow.list, got: %v", lastProdReq.Body["pagination"])
		}
	})
}

func TestOperationService_REST3_IdempotentWrites(t *testing.T) {
	ctx := context.Background()

	var recordedRequests []struct {
		Method         string
		URL            string
		IdempotencyKey string
		Body           map[string]any
		Raw            string
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
			Method         string
			URL            string
			IdempotencyKey string
			Body           map[string]any
			Raw            string
		}{
			Method:         r.Method,
			URL:            r.URL.String(),
			IdempotencyKey: r.Header.Get("Idempotency-Key"),
			Body:           bodyMap,
			Raw:            rawBody,
		})
		mu.Unlock()

		path := r.URL.Path

		if strings.HasSuffix(path, "/rest/api/crm.deal.add") {
			return jsonResponse(200, map[string]any{"result": map[string]any{"id": 100}}), nil
		}
		if strings.HasSuffix(path, "/rest/api/crm.deal.update") {
			return jsonResponse(200, map[string]any{"result": true}), nil
		}
		if strings.HasSuffix(path, "/rest/api/crm.deal.delete") {
			return jsonResponse(200, map[string]any{"result": true}), nil
		}
		if strings.HasSuffix(path, "crm.item.delete.json") {
			return jsonResponse(200, map[string]any{"result": true}), nil
		}

		return jsonResponse(200, map[string]any{"result": true}), nil
	})

	cfg, st, _, tm := newTestEnv(t, "", "client-id", rt)
	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-ops-rest3",
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
	opSvc := bitrix.NewOperationService(cfg, st, rc)

	t.Run("PrepareCreate and Confirm sends crm.deal.add with Idempotency-Key and normalized ufCrm", func(t *testing.T) {
		createPrep, err := opSvc.PrepareCreate(ctx, conn.ID, 2, map[string]any{
			"title":            "REST 3.0 Deal",
			"ufCrmCustomField": "custom value",
		})
		if err != nil {
			t.Fatalf("PrepareCreate failed: %v", err)
		}
		createOpID := createPrep["operation_id"].(string)

		confirmCreate, err := opSvc.Confirm(ctx, createOpID, "")
		if err != nil {
			t.Fatalf("Confirm create failed: %v", err)
		}
		if confirmCreate["status"] != "completed" {
			t.Fatalf("expected completed create status: %+v", confirmCreate)
		}

		mu.Lock()
		lastReq := recordedRequests[len(recordedRequests)-1]
		mu.Unlock()
		if !strings.Contains(lastReq.URL, "/rest/api/crm.deal.add") {
			t.Errorf("expected REST 3.0 crm.deal.add, got: %s", lastReq.URL)
		}
		if lastReq.IdempotencyKey != createOpID {
			t.Errorf("expected Idempotency-Key header %q, got: %q", createOpID, lastReq.IdempotencyKey)
		}
		fieldsMap, _ := lastReq.Body["fields"].(map[string]any)
		if fieldsMap["UF_CRM_CUSTOMFIELD"] != "custom value" {
			t.Errorf("expected UF_CRM_CUSTOMFIELD in write body, got: %+v", fieldsMap)
		}
	})

	t.Run("PrepareUpdate and Confirm sends crm.deal.update with Idempotency-Key", func(t *testing.T) {
		updatePrep, err := opSvc.PrepareUpdate(ctx, conn.ID, 2, 10, map[string]any{
			"title": "Updated Title",
		})
		if err != nil {
			t.Fatalf("PrepareUpdate failed: %v", err)
		}
		updateOpID := updatePrep["operation_id"].(string)

		if _, err := opSvc.Confirm(ctx, updateOpID, ""); err != nil {
			t.Fatalf("Confirm update failed: %v", err)
		}

		mu.Lock()
		lastReq := recordedRequests[len(recordedRequests)-1]
		mu.Unlock()
		if !strings.Contains(lastReq.URL, "/rest/api/crm.deal.update") {
			t.Errorf("expected REST 3.0 crm.deal.update, got: %s", lastReq.URL)
		}
		if lastReq.IdempotencyKey != updateOpID {
			t.Errorf("expected Idempotency-Key header %q, got: %q", updateOpID, lastReq.IdempotencyKey)
		}
		if lastReq.Body["id"] != float64(10) && lastReq.Body["id"] != 10 {
			t.Errorf("expected id: 10 in update body, got: %v", lastReq.Body["id"])
		}
	})

	t.Run("PrepareMove and Confirm sends crm.deal.update stage move with Idempotency-Key", func(t *testing.T) {
		movePrep, err := opSvc.PrepareMove(ctx, conn.ID, 2, 10, "WON")
		if err != nil {
			t.Fatalf("PrepareMove failed: %v", err)
		}
		moveOpID := movePrep["operation_id"].(string)

		if _, err := opSvc.Confirm(ctx, moveOpID, ""); err != nil {
			t.Fatalf("Confirm move failed: %v", err)
		}

		mu.Lock()
		lastReq := recordedRequests[len(recordedRequests)-1]
		mu.Unlock()
		if !strings.Contains(lastReq.URL, "/rest/api/crm.deal.update") {
			t.Errorf("expected REST 3.0 crm.deal.update for stage move, got: %s", lastReq.URL)
		}
		if lastReq.IdempotencyKey != moveOpID {
			t.Errorf("expected Idempotency-Key header %q, got: %q", moveOpID, lastReq.IdempotencyKey)
		}
	})

	t.Run("PrepareDelete and Confirm sends crm.deal.delete with Idempotency-Key", func(t *testing.T) {
		deletePrep, err := opSvc.PrepareDelete(ctx, conn.ID, 2, 10)
		if err != nil {
			t.Fatalf("PrepareDelete failed: %v", err)
		}
		deleteOpID := deletePrep["operation_id"].(string)

		if _, err := opSvc.Confirm(ctx, deleteOpID, ""); err != nil {
			t.Fatalf("Confirm delete failed: %v", err)
		}

		mu.Lock()
		lastReq := recordedRequests[len(recordedRequests)-1]
		mu.Unlock()
		if !strings.Contains(lastReq.URL, "/rest/api/crm.deal.delete") {
			t.Errorf("expected REST 3.0 crm.deal.delete, got: %s", lastReq.URL)
		}
		if lastReq.IdempotencyKey != deleteOpID {
			t.Errorf("expected Idempotency-Key header %q, got: %q", deleteOpID, lastReq.IdempotencyKey)
		}
		if lastReq.Body["id"] != float64(10) && lastReq.Body["id"] != 10 {
			t.Errorf("expected id: 10 in delete body, got: %v", lastReq.Body["id"])
		}
	})
}




