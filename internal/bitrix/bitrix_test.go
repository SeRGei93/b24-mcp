package bitrix_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
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
		"Example.BITRIX24.com":         "example.bitrix24.com",
		"https://example.bitrix24.com/": "example.bitrix24.com",
		"example.bitrix24.com.br":      "example.bitrix24.com.br",
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

