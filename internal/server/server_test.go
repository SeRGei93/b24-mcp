package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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

func TestServer_McpOAuthDiscoveryAndFederation(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "oauth.bitrix.info" {
			return jsonResponse(200, map[string]any{
				"access_token":    "b24-access",
				"refresh_token":   "b24-refresh",
				"expires_in":      3600,
				"scope":           "crm",
				"domain":          "oauth.bitrix.info",
				"member_id":       "member-auth",
				"user_id":         "7",
				"client_endpoint": "https://example.bitrix24.com/rest/",
			}), nil
		}
		return jsonResponse(200, map[string]any{
			"result": map[string]any{"ID": "7"},
		}), nil
	})

	cfg, st, handler := setupTestServer(t, rt)

	// Seed two connections (example.bitrix24.com will be the active portal; other.bitrix24.com is disabled so example.bitrix24.com is the single active portal)
	authConn, err := st.UpsertConnection(t.Context(), store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-auth",
		BitrixUserID:   "7",
		ClientEndpoint: "https://example.bitrix24.com/rest/",
		AccessToken:    "seed-access",
		RefreshToken:   "seed-refresh",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("failed to seed authConn: %v", err)
	}
	otherConn, err := st.UpsertConnection(t.Context(), store.UpsertConnectionInput{
		Domain:         "other.bitrix24.com",
		MemberID:       "member-other",
		BitrixUserID:   "8",
		ClientEndpoint: "https://other.bitrix24.com/rest/",
		AccessToken:    "other-access",
		RefreshToken:   "other-refresh",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("failed to seed otherConn: %v", err)
	}
	if err := st.MarkConnectionRevoked(t.Context(), otherConn.ID); err != nil {
		t.Fatalf("failed to mark otherConn revoked: %v", err)
	}

	// 1. Discovery metadata endpoints
	for _, metaPath := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		req := httptest.NewRequest(http.MethodGet, metaPath, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 on %s, got %d", metaPath, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"resource":"https://mcp.example.com/mcp"`) {
			t.Errorf("unexpected protected resource metadata on %s: %s", metaPath, rec.Body.String())
		}
	}

	asMetaReq := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	asMetaRec := httptest.NewRecorder()
	handler.ServeHTTP(asMetaRec, asMetaReq)
	if asMetaRec.Code != http.StatusOK || !strings.Contains(asMetaRec.Body.String(), `"registration_endpoint":"https://mcp.example.com/register"`) {
		t.Fatalf("unexpected authorization server metadata: %d %s", asMetaRec.Code, asMetaRec.Body.String())
	}

	// 2. OPTIONS preflight
	optReq := httptest.NewRequest(http.MethodOptions, "/register", nil)
	optRec := httptest.NewRecorder()
	handler.ServeHTTP(optRec, optReq)
	if optRec.Code != http.StatusNoContent || optRec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("expected 204 CORS response on OPTIONS /register, got %d", optRec.Code)
	}

	// 3. Dynamic Client Registration (POST /register)
	redirectURI := "http://127.0.0.1:43123/callback"
	regPayload, _ := json.Marshal(map[string]any{
		"redirect_uris":              []string{redirectURI},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"client_name":                "Codex test client",
	})
	regReq := httptest.NewRequest(http.MethodPost, "/register", bytes.NewReader(regPayload))
	regReq.Header.Set("Content-Type", "application/json")
	regRec := httptest.NewRecorder()
	handler.ServeHTTP(regRec, regReq)
	if regRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 on POST /register, got %d: %s", regRec.Code, regRec.Body.String())
	}
	var regResp struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(regRec.Body.Bytes(), &regResp); err != nil || regResp.ClientID == "" {
		t.Fatalf("failed to parse client_id: %v (%s)", err, regRec.Body.String())
	}

	// 4. GET /authorize -> renders consent screen with CSRF cookie & security headers
	verifier := strings.Repeat("v", 64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	authQuery := url.Values{
		"response_type":         {"code"},
		"client_id":             {regResp.ClientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"state-123"},
		"scope":                 {"mcp:tools"},
		"resource":              {"https://mcp.example.com/mcp"},
	}
	getConsent := func() (string, string, string) {
		req := httptest.NewRequest(http.MethodGet, "/authorize?"+authQuery.Encode(), nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 on GET /authorize, got %d: %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("missing security headers on consent page: %v", rec.Header())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Codex test client") {
			t.Fatalf("expected client name in consent page: %s", body)
		}
		reqTokMatch := regexp.MustCompile(`name="request_token" value="([^"]+)"`).FindStringSubmatch(body)
		csrfMatch := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(body)
		cookieHeader := rec.Header().Get("Set-Cookie")
		cookiePair := strings.Split(cookieHeader, ";")[0]
		if len(reqTokMatch) < 2 || len(csrfMatch) < 2 || !strings.HasPrefix(cookiePair, "mcp_oauth_csrf=") {
			t.Fatalf("missing tokens/cookie in consent response: body=%s cookie=%s", body, cookieHeader)
		}
		return reqTokMatch[1], csrfMatch[1], cookiePair
	}

	reqTok1, _, _ := getConsent()

	// 5. POST /authorize with wrong CSRF token -> 403
	badCsrfForm := url.Values{
		"request_token": {reqTok1},
		"csrf_token":    {"wrong-csrf"},
		"decision":      {"approve"},
	}
	badCsrfReq := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(badCsrfForm.Encode()))
	badCsrfReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badCsrfRec := httptest.NewRecorder()
	handler.ServeHTTP(badCsrfRec, badCsrfReq)
	if badCsrfRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on wrong CSRF token, got %d: %s", badCsrfRec.Code, badCsrfRec.Body.String())
	}

	// 6. POST /authorize with decision=deny -> redirects to client with error=access_denied
	reqTokDeny, csrfDeny, cookieDeny := getConsent()
	denyForm := url.Values{
		"request_token": {reqTokDeny},
		"csrf_token":    {csrfDeny},
		"decision":      {"deny"},
	}
	denyReq := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(denyForm.Encode()))
	denyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	denyReq.Header.Set("Cookie", cookieDeny)
	denyRec := httptest.NewRecorder()
	handler.ServeHTTP(denyRec, denyReq)
	if denyRec.Code != http.StatusFound || !strings.Contains(denyRec.Header().Get("Location"), "error=access_denied") {
		t.Fatalf("expected 302 error=access_denied on deny, got %d loc=%s", denyRec.Code, denyRec.Header().Get("Location"))
	}

	// 7. POST /authorize with decision=approve -> redirects to Bitrix24 OAuth
	reqTokApprove, csrfApprove, cookieApprove := getConsent()
	approveForm := url.Values{
		"request_token": {reqTokApprove},
		"csrf_token":    {csrfApprove},
		"decision":      {"approve"},
	}
	approveReq := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(approveForm.Encode()))
	approveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	approveReq.Header.Set("Cookie", cookieApprove)
	approveRec := httptest.NewRecorder()
	handler.ServeHTTP(approveRec, approveReq)
	if approveRec.Code != http.StatusFound || !strings.Contains(approveRec.Header().Get("Location"), "example.bitrix24.com/oauth/authorize/") {
		t.Fatalf("expected 302 redirect to Bitrix24, got %d loc=%s", approveRec.Code, approveRec.Header().Get("Location"))
	}
	bitrixLoc, _ := url.Parse(approveRec.Header().Get("Location"))
	bitrixState := bitrixLoc.Query().Get("state")

	// 7b. GET /authorize with pre-authenticated MCP_AUTH_TOKEN skips consent screen
	preAuthReq := httptest.NewRequest(http.MethodGet, "/authorize?"+authQuery.Encode(), nil)
	preAuthReq.Header.Set("Authorization", "Bearer "+cfg.McpAuthToken)
	preAuthRec := httptest.NewRecorder()
	handler.ServeHTTP(preAuthRec, preAuthReq)
	if preAuthRec.Code != http.StatusFound || !strings.Contains(preAuthRec.Header().Get("Location"), "example.bitrix24.com/oauth/authorize/") {
		t.Fatalf("expected 302 direct redirect when pre-authenticated with MCP_AUTH_TOKEN, got %d", preAuthRec.Code)
	}

	// 8. GET /oauth/bitrix/callback -> redirects back to MCP client redirect_uri with code & state
	cbReq := httptest.NewRequest(http.MethodGet, "/oauth/bitrix/callback?code=bitrix-code&state="+url.QueryEscape(bitrixState)+"&domain=example.bitrix24.com", nil)
	cbRec := httptest.NewRecorder()
	handler.ServeHTTP(cbRec, cbReq)
	if cbRec.Code != http.StatusFound {
		t.Fatalf("expected 302 on federated Bitrix callback, got %d: %s", cbRec.Code, cbRec.Body.String())
	}
	clientCbURL, err := url.Parse(cbRec.Header().Get("Location"))
	if err != nil || clientCbURL.Query().Get("state") != "state-123" || clientCbURL.Query().Get("code") == "" {
		t.Fatalf("unexpected client callback redirect: %s", cbRec.Header().Get("Location"))
	}
	mcpCode := clientCbURL.Query().Get("code")

	// 9. POST /token (authorization_code exchange)
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {regResp.ClientID},
		"code":          {mcpCode},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
		"resource":      {"https://mcp.example.com/mcp"},
	}
	tokenReq := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(tokenForm.Encode()))
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRec := httptest.NewRecorder()
	handler.ServeHTTP(tokenRec, tokenReq)
	if tokenRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on POST /token authorization_code, got %d: %s", tokenRec.Code, tokenRec.Body.String())
	}
	var firstTokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(tokenRec.Body.Bytes(), &firstTokens); err != nil || firstTokens.AccessToken == "" || firstTokens.RefreshToken == "" {
		t.Fatalf("invalid token response: %s", tokenRec.Body.String())
	}

	// 10. POST /token (refresh_token rotation) & reuse rejection
	refreshForm := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {regResp.ClientID},
		"refresh_token": {firstTokens.RefreshToken},
		"resource":      {"https://mcp.example.com/mcp"},
	}
	refreshReq := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(refreshForm.Encode()))
	refreshReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	refreshRec := httptest.NewRecorder()
	handler.ServeHTTP(refreshRec, refreshReq)
	if refreshRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on POST /token refresh_token, got %d: %s", refreshRec.Code, refreshRec.Body.String())
	}
	var secondTokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(refreshRec.Body.Bytes(), &secondTokens); err != nil || secondTokens.AccessToken == "" || secondTokens.AccessToken == firstTokens.AccessToken {
		t.Fatalf("expected rotated access token, got %+v", secondTokens)
	}

	// Reusing firstTokens.RefreshToken must fail with 400 invalid_grant
	reuseReq := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(refreshForm.Encode()))
	reuseReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reuseRec := httptest.NewRecorder()
	handler.ServeHTTP(reuseRec, reuseReq)
	if reuseRec.Code != http.StatusBadRequest || !strings.Contains(reuseRec.Body.String(), `"invalid_grant"`) {
		t.Fatalf("expected 400 invalid_grant on refresh token reuse, got %d: %s", reuseRec.Code, reuseRec.Body.String())
	}

	// 11. Cross-connection isolation with connection-scoped OAuth token
	scopedAuth := "Bearer " + secondTokens.AccessToken

	// 11a. GET /oauth/bitrix/start is forbidden for connection-scoped OAuth token
	startScopedReq := httptest.NewRequest(http.MethodGet, "/oauth/bitrix/start?domain=example.bitrix24.com", nil)
	startScopedReq.Header.Set("Authorization", scopedAuth)
	startScopedRec := httptest.NewRecorder()
	handler.ServeHTTP(startScopedRec, startScopedReq)
	if startScopedRec.Code != http.StatusForbidden || !strings.Contains(startScopedRec.Body.String(), "CONNECTION_ACCESS_DENIED") {
		t.Fatalf("expected 403 CONNECTION_ACCESS_DENIED on /oauth/bitrix/start with scoped token, got %d: %s", startScopedRec.Code, startScopedRec.Body.String())
	}

	// 11b. POST /oauth/bitrix/refresh for another connection is forbidden
	refreshOtherBody, _ := json.Marshal(map[string]string{"connection_id": otherConn.ID})
	refreshOtherReq := httptest.NewRequest(http.MethodPost, "/oauth/bitrix/refresh", bytes.NewReader(refreshOtherBody))
	refreshOtherReq.Header.Set("Authorization", scopedAuth)
	refreshOtherReq.Header.Set("Content-Type", "application/json")
	refreshOtherRec := httptest.NewRecorder()
	handler.ServeHTTP(refreshOtherRec, refreshOtherReq)
	if refreshOtherRec.Code != http.StatusForbidden || !strings.Contains(refreshOtherRec.Body.String(), "CONNECTION_ACCESS_DENIED") {
		t.Fatalf("expected 403 CONNECTION_ACCESS_DENIED on /oauth/bitrix/refresh for other connection, got %d: %s", refreshOtherRec.Code, refreshOtherRec.Body.String())
	}

	// 11c. POST /mcp bitrix_list_connections only returns authConn, not otherConn
	listToolsCall := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"bitrix_list_connections","arguments":{}}}`
	mcpCallReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(listToolsCall))
	mcpCallReq.Header.Set("Authorization", scopedAuth)
	mcpCallReq.Header.Set("Content-Type", "application/json")
	mcpCallRec := httptest.NewRecorder()
	handler.ServeHTTP(mcpCallRec, mcpCallReq)
	if mcpCallRec.Code != http.StatusOK || !strings.Contains(mcpCallRec.Body.String(), authConn.ID) || strings.Contains(mcpCallRec.Body.String(), otherConn.ID) {
		t.Fatalf("expected scoped POST /mcp to only list %s, got %d: %s", authConn.ID, mcpCallRec.Code, mcpCallRec.Body.String())
	}

	// 12. POST /revoke -> invalidates secondTokens.AccessToken
	revokeForm := url.Values{"token": {secondTokens.AccessToken}}
	revokeReq := httptest.NewRequest(http.MethodPost, "/revoke", strings.NewReader(revokeForm.Encode()))
	revokeReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	revokeRec := httptest.NewRecorder()
	handler.ServeHTTP(revokeRec, revokeReq)
	if revokeRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on POST /revoke, got %d: %s", revokeRec.Code, revokeRec.Body.String())
	}

	afterRevokeReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(listToolsCall))
	afterRevokeReq.Header.Set("Authorization", scopedAuth)
	afterRevokeReq.Header.Set("Content-Type", "application/json")
	afterRevokeRec := httptest.NewRecorder()
	handler.ServeHTTP(afterRevokeRec, afterRevokeReq)
	if afterRevokeRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after token revocation, got %d", afterRevokeRec.Code)
	}
}

func TestServer_RateLimiterPerIPAndTrustProxy(t *testing.T) {
	// 1. With TRUST_PROXY disabled (default), X-Forwarded-For cannot spoof source IP,
	// and exhausting one IP's /register bucket (20 requests) does not block another IP.
	_, _, handlerNoProxy := setupTestServer(t, nil)
	regPayload := `{"redirect_uris":["http://127.0.0.1:3000/cb"]}`

	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(regPayload))
		req.RemoteAddr = "198.51.100.10:12345"
		req.Header.Set("X-Forwarded-For", "203.0.113.1")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handlerNoProxy.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201 on request %d, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	// 21st request from same RemoteAddr (even with different X-Forwarded-For) -> 429
	blockedReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(regPayload))
	blockedReq.RemoteAddr = "198.51.100.10:12345"
	blockedReq.Header.Set("X-Forwarded-For", "203.0.113.99")
	blockedReq.Header.Set("Content-Type", "application/json")
	blockedRec := httptest.NewRecorder()
	handlerNoProxy.ServeHTTP(blockedRec, blockedReq)
	if blockedRec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for exhausted IP, got %d: %s", blockedRec.Code, blockedRec.Body.String())
	}

	// Different RemoteAddr still succeeds -> proves per-IP check does not exhaust global bucket
	otherIPReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(regPayload))
	otherIPReq.RemoteAddr = "198.51.100.20:12345"
	otherIPReq.Header.Set("Content-Type", "application/json")
	otherIPRec := httptest.NewRecorder()
	handlerNoProxy.ServeHTTP(otherIPRec, otherIPReq)
	if otherIPRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for second IP, got %d: %s", otherIPRec.Code, otherIPRec.Body.String())
	}

	// 2. With TRUST_PROXY=true, X-Forwarded-For separates client buckets behind a proxy
	cfgProxy, err := config.Load(map[string]string{
		"APP_URL":              "https://mcp.example.com",
		"DATABASE_PATH":        ":memory:",
		"BITRIX_CLIENT_ID":     "pending-bitrix-app",
		"BITRIX_CLIENT_SECRET": "secret",
		"MCP_AUTH_TOKEN":       strings.Repeat("a", 32),
		"TOKEN_ENCRYPTION_KEY": testServerKeyHex,
		"TRUST_PROXY":          "true",
	})
	if err != nil {
		t.Fatalf("failed to load proxy config: %v", err)
	}
	stProxy, err := store.NewStore(cfgProxy.DatabasePath, cfgProxy.TokenEncryptionKey)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer stProxy.Close()
	_ = stProxy.Migrate(t.Context(), nil)
	oauthProxy := bitrix.NewOAuthService(cfgProxy, stProxy, nil)
	tmProxy := bitrix.NewTokenManager(stProxy, oauthProxy, nil)
	handlerProxy := server.NewHandler(cfgProxy, stProxy, oauthProxy, tmProxy, nil)

	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(regPayload))
		req.RemoteAddr = "10.0.0.1:12345"
		req.Header.Set("X-Forwarded-For", "203.0.113.10")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handlerProxy.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201 on proxy request %d, got %d", i+1, rec.Code)
		}
	}

	// Another client IP via X-Forwarded-For from the same proxy RemoteAddr succeeds!
	secondForwardedReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(regPayload))
	secondForwardedReq.RemoteAddr = "10.0.0.1:12345"
	secondForwardedReq.Header.Set("X-Forwarded-For", "203.0.113.20")
	secondForwardedReq.Header.Set("Content-Type", "application/json")
	secondForwardedRec := httptest.NewRecorder()
	handlerProxy.ServeHTTP(secondForwardedRec, secondForwardedReq)
	if secondForwardedRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for distinct X-Forwarded-For client when TRUST_PROXY=true, got %d", secondForwardedRec.Code)
	}
}
