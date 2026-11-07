package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/server"
	"github.com/SeRGei93/b24-mcp/internal/store"
)

const testServerKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

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

func setupTestServer(t *testing.T, rt http.RoundTripper) (*config.Config, *store.Store, http.Handler) {
	t.Helper()
	cfg, err := config.Load(map[string]string{
		"APP_URL":              "https://mcp.example.com",
		"DATABASE_PATH":        ":memory:",
		"BITRIX_CLIENT_ID":     "pending-bitrix-app",
		"BITRIX_CLIENT_SECRET": "secret",
		"MCP_AUTH_TOKEN":       strings.Repeat("a", 32),
		"TOKEN_ENCRYPTION_KEY": testServerKeyHex,
	})
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	st, err := store.NewStore(cfg.DatabasePath, cfg.TokenEncryptionKey)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logger := config.NewLogger(nil, "silent")
	if err := st.Migrate(t.Context(), logger); err != nil {
		t.Fatalf("failed to migrate store: %v", err)
	}

	httpClient := &http.Client{Transport: rt, Timeout: 2 * time.Second}
	oauth := bitrix.NewOAuthService(cfg, st, httpClient)
	tm := bitrix.NewTokenManager(st, oauth, logger)

	handler := server.NewHandler(cfg, st, oauth, tm, logger)
	return cfg, st, handler
}

func TestServer_RootMetadataAndHealth(t *testing.T) {
	_, st, handler := setupTestServer(t, nil)

	// 1. GET /
	rootReq := httptest.NewRequest(http.MethodGet, "/", nil)
	rootRec := httptest.NewRecorder()
	handler.ServeHTTP(rootRec, rootReq)
	if rootRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET /, got %d", rootRec.Code)
	}
	var rootBody map[string]string
	if err := json.Unmarshal(rootRec.Body.Bytes(), &rootBody); err != nil {
		t.Fatalf("invalid JSON on GET /: %v", err)
	}
	if rootBody["name"] != "bitrix24-mcp-server" || rootBody["mcp_endpoint"] != "/mcp" {
		t.Errorf("unexpected root metadata: %+v", rootBody)
	}

	// 2. GET /health (connected)
	healthReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	healthRec := httptest.NewRecorder()
	handler.ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET /health, got %d", healthRec.Code)
	}
	var healthBody map[string]string
	if err := json.Unmarshal(healthRec.Body.Bytes(), &healthBody); err != nil {
		t.Fatalf("invalid JSON on GET /health: %v", err)
	}
	if healthBody["status"] != "ok" || healthBody["database"] != "connected" {
		t.Errorf("unexpected health response: %+v", healthBody)
	}

	// 3. GET /health (disconnected -> 503)
	_ = st.Close()
	downReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	downRec := httptest.NewRecorder()
	handler.ServeHTTP(downRec, downReq)
	if downRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on GET /health when DB closed, got %d", downRec.Code)
	}
}

func TestServer_OAuthRoutesFormParsingAndMCPEndpoint(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "oauth.bitrix.info" {
			return jsonResponse(200, map[string]any{
				"access_token":    "new-access",
				"refresh_token":   "new-refresh",
				"expires_in":      3600,
				"scope":           "crm",
				"domain":          "oauth.bitrix.info",
				"member_id":       "member-1",
				"user_id":         "7",
				"client_endpoint": "https://example.bitrix24.com/rest/",
			}), nil
		}
		if strings.HasSuffix(r.URL.Path, "app.info.json") {
			return jsonResponse(200, map[string]any{
				"result": map[string]any{"CODE": "local.app", "INSTALLED": true},
			}), nil
		}
		return jsonResponse(200, map[string]any{
			"result": map[string]any{"ID": "7"},
		}), nil
	})

	cfg, _, handler := setupTestServer(t, rt)
	authHeader := "Bearer " + cfg.McpAuthToken

	// 1. Unauthenticated GET /oauth/bitrix/start -> 401 + WWW-Authenticate
	unauthReq := httptest.NewRequest(http.MethodGet, "/oauth/bitrix/start?domain=example.bitrix24.com", nil)
	unauthRec := httptest.NewRecorder()
	handler.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthRec.Code)
	}
	if !strings.Contains(unauthRec.Header().Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp") {
		t.Errorf("missing expected WWW-Authenticate header: %q", unauthRec.Header().Get("WWW-Authenticate"))
	}

	// 2. Cross-origin authenticated request -> 403 ORIGIN_NOT_ALLOWED
	crossReq := httptest.NewRequest(http.MethodGet, "/oauth/bitrix/start?domain=example.bitrix24.com", nil)
	crossReq.Header.Set("Authorization", authHeader)
	crossReq.Header.Set("Origin", "https://evil.example")
	crossRec := httptest.NewRecorder()
	handler.ServeHTTP(crossRec, crossReq)
	if crossRec.Code != http.StatusForbidden || !strings.Contains(crossRec.Body.String(), "ORIGIN_NOT_ALLOWED") {
		t.Fatalf("expected 403 ORIGIN_NOT_ALLOWED, got %d: %s", crossRec.Code, crossRec.Body.String())
	}

	// 3. Valid GET /oauth/bitrix/start -> 302 redirect, followed by GET /oauth/bitrix/callback
	startReq := httptest.NewRequest(http.MethodGet, "/oauth/bitrix/start?domain=example.bitrix24.com", nil)
	startReq.Header.Set("Authorization", authHeader)
	startRec := httptest.NewRecorder()
	handler.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusFound {
		t.Fatalf("expected 302 on /oauth/bitrix/start, got %d: %s", startRec.Code, startRec.Body.String())
	}
	loc, err := url.Parse(startRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("invalid redirect Location: %v", err)
	}
	state := loc.Query().Get("state")

	cbReq := httptest.NewRequest(http.MethodGet, "/oauth/bitrix/callback?code=code-1&state="+url.QueryEscape(state), nil)
	cbRec := httptest.NewRecorder()
	handler.ServeHTTP(cbRec, cbReq)
	if cbRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on callback, got %d: %s", cbRec.Code, cbRec.Body.String())
	}

	// 4. POST /oauth/bitrix/install with application/x-www-form-urlencoded auth[...]
	form := url.Values{
		"auth[access_token]":  {"form-access"},
		"auth[refresh_token]": {"form-refresh"},
		"auth[domain]":        {"example.bitrix24.com"},
		"auth[member_id]":     {"member-1"},
	}
	installReq := httptest.NewRequest(http.MethodPost, "/oauth/bitrix/install", strings.NewReader(form.Encode()))
	installReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	installRec := httptest.NewRecorder()
	handler.ServeHTTP(installRec, installReq)
	if installRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on form install, got %d: %s", installRec.Code, installRec.Body.String())
	}
	var installResp struct {
		Connected  bool                   `json:"connected"`
		Connection store.PublicConnection `json:"connection"`
	}
	if err := json.Unmarshal(installRec.Body.Bytes(), &installResp); err != nil || !installResp.Connected {
		t.Fatalf("unexpected install response: %s (err=%v)", installRec.Body.String(), err)
	}

	// 4b. POST /oauth/bitrix/callback with JSON installation payload
	jsonInstallBody := `{"DOMAIN":"example.bitrix24.com","AUTH_ID":"json-access","REFRESH_ID":"json-refresh","member_id":"member-1"}`
	cbPostReq := httptest.NewRequest(http.MethodPost, "/oauth/bitrix/callback", strings.NewReader(jsonInstallBody))
	cbPostReq.Header.Set("Content-Type", "application/json")
	cbPostRec := httptest.NewRecorder()
	handler.ServeHTTP(cbPostRec, cbPostReq)
	if cbPostRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on POST /oauth/bitrix/callback, got %d: %s", cbPostRec.Code, cbPostRec.Body.String())
	}

	// 5. POST /oauth/bitrix/refresh
	refreshBody, _ := json.Marshal(map[string]string{"connection_id": installResp.Connection.ID})
	refreshReq := httptest.NewRequest(http.MethodPost, "/oauth/bitrix/refresh", bytes.NewReader(refreshBody))
	refreshReq.Header.Set("Authorization", authHeader)
	refreshReq.Header.Set("Content-Type", "application/json")
	refreshRec := httptest.NewRecorder()
	handler.ServeHTTP(refreshRec, refreshReq)
	if refreshRec.Code != http.StatusOK || !strings.Contains(refreshRec.Body.String(), `"refreshed":true`) {
		t.Fatalf("expected 200 refreshed:true, got %d: %s", refreshRec.Code, refreshRec.Body.String())
	}

	// 6. GET /mcp and DELETE /mcp -> 405 Method Not Allowed
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		mcpReq := httptest.NewRequest(method, "/mcp", nil)
		mcpReq.Header.Set("Authorization", authHeader)
		mcpRec := httptest.NewRecorder()
		handler.ServeHTTP(mcpRec, mcpReq)
		if mcpRec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405 on %s /mcp, got %d", method, mcpRec.Code)
		}
	}

	// 7. POST /mcp -> Streamable HTTP initialize
	initPayload := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`
	postMcpReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initPayload))
	postMcpReq.Header.Set("Authorization", authHeader)
	postMcpReq.Header.Set("Content-Type", "application/json")
	postMcpRec := httptest.NewRecorder()
	handler.ServeHTTP(postMcpRec, postMcpReq)
	if postMcpRec.Code != http.StatusOK || !strings.Contains(postMcpRec.Body.String(), `"bitrix24-mcp-server"`) {
		t.Fatalf("expected 200 initialize response on POST /mcp, got %d: %s", postMcpRec.Code, postMcpRec.Body.String())
	}
}
