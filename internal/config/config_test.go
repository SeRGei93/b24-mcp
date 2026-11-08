package config_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SeRGei93/b24-mcp/internal/config"
)

func baseEnv(appURL string) map[string]string {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return map[string]string{
		"APP_URL":              appURL,
		"DATABASE_PATH":        "./data/test.sqlite",
		"BITRIX_CLIENT_ID":     "client",
		"BITRIX_CLIENT_SECRET": "secret",
		"MCP_AUTH_TOKEN":       strings.Repeat("a", 32),
		"TOKEN_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(key),
	}
}

func TestLoad_SafeDefaultsAndOriginNormalization(t *testing.T) {
	cfg, err := config.Load(baseEnv("http://localhost:3000/"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.AppURL != "http://localhost:3000" {
		t.Errorf("expected AppURL 'http://localhost:3000', got %q", cfg.AppURL)
	}
	if cfg.DefaultMode != config.ModeReadOnly {
		t.Errorf("expected DefaultMode %q, got %q", config.ModeReadOnly, cfg.DefaultMode)
	}
	if cfg.AdminToolsEnabled {
		t.Errorf("expected AdminToolsEnabled false")
	}
	if cfg.BitrixPortalDomain != "" {
		t.Errorf("expected empty BitrixPortalDomain, got %q", cfg.BitrixPortalDomain)
	}
	if cfg.McpOAuthAccessTokenTTLSeconds != 3600 {
		t.Errorf("expected McpOAuthAccessTokenTTLSeconds 3600, got %d", cfg.McpOAuthAccessTokenTTLSeconds)
	}
	if cfg.DatabasePath != "./data/test.sqlite" {
		t.Errorf("expected DatabasePath './data/test.sqlite', got %q", cfg.DatabasePath)
	}
	if cfg.Host != "0.0.0.0" || cfg.Port != 3000 {
		t.Errorf("unexpected host/port: %s:%d", cfg.Host, cfg.Port)
	}
	if cfg.TrustProxy.Enabled {
		t.Errorf("expected TrustProxy.Enabled false by default")
	}
}

func TestLoad_DatabasePathNormalization(t *testing.T) {
	env := baseEnv("http://127.0.0.1:3000")
	delete(env, "DATABASE_PATH")
	cfg, err := config.Load(env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DatabasePath != "./data/bitrix24-mcp.sqlite" {
		t.Errorf("expected default './data/bitrix24-mcp.sqlite', got %q", cfg.DatabasePath)
	}

	envURL := baseEnv("http://[::1]:3000")
	delete(envURL, "DATABASE_PATH")
	envURL["DATABASE_URL"] = "sqlite://./data/custom.sqlite"
	cfgURL, err := config.Load(envURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfgURL.DatabasePath != "./data/custom.sqlite" {
		t.Errorf("expected './data/custom.sqlite', got %q", cfgURL.DatabasePath)
	}

	cases := map[string]string{
		":memory:":                   ":memory:",
		"file::memory:?cache=shared": ":memory:",
		"sqlite:./data/a.sqlite":     "./data/a.sqlite",
		"file://./data/b.sqlite":     "./data/b.sqlite",
		"file:./data/c.sqlite":       "./data/c.sqlite",
	}
	for raw, want := range cases {
		if got := config.NormalizeSqlitePath(raw); got != want {
			t.Errorf("NormalizeSqlitePath(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestLoad_PortalDomainAndTrustProxy(t *testing.T) {
	env := baseEnv("https://mcp.example.com")
	env["BITRIX_PORTAL_DOMAIN"] = "example.bitrix24.com"
	env["TRUST_PROXY"] = "true"
	cfg, err := config.Load(env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BitrixPortalDomain != "example.bitrix24.com" {
		t.Errorf("expected BitrixPortalDomain 'example.bitrix24.com', got %q", cfg.BitrixPortalDomain)
	}
	if !cfg.TrustProxy.Enabled || cfg.TrustProxy.Hops != 1 {
		t.Errorf("expected TrustProxy enabled with 1 hop, got %+v", cfg.TrustProxy)
	}

	envHops := baseEnv("https://mcp.example.com")
	envHops["TRUST_PROXY"] = "2"
	cfgHops, err := config.Load(envHops)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfgHops.TrustProxy.Enabled || cfgHops.TrustProxy.Hops != 2 {
		t.Errorf("expected 2 hops, got %+v", cfgHops.TrustProxy)
	}

	envList := baseEnv("https://mcp.example.com")
	envList["TRUST_PROXY"] = "10.0.0.1, 172.16.0.1"
	cfgList, err := config.Load(envList)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfgList.TrustProxy.Enabled || len(cfgList.TrustProxy.Addresses) != 2 {
		t.Errorf("expected 2 proxy addresses, got %+v", cfgList.TrustProxy)
	}
}

func TestLoad_RejectsInvalidAppURLAndShortAuthToken(t *testing.T) {
	invalidURLs := []string{
		"http://example.com",
		"http://localhost.evil.example",
		"https://mcp.example.com/a/path",
		"https://mcp.example.com/?query=yes",
		"https://mcp.example.com#fragment",
	}
	for _, u := range invalidURLs {
		_, err := config.Load(baseEnv(u))
		if err == nil || !strings.Contains(err.Error(), "APP_URL") {
			t.Errorf("expected APP_URL error for %q, got %v", u, err)
		}
	}

	shortTokenEnv := baseEnv("http://localhost:3000")
	shortTokenEnv["MCP_AUTH_TOKEN"] = "too-short"
	if _, err := config.Load(shortTokenEnv); err == nil || !strings.Contains(err.Error(), "MCP_AUTH_TOKEN") {
		t.Errorf("expected MCP_AUTH_TOKEN error, got %v", err)
	}

	badKeyEnv := baseEnv("http://localhost:3000")
	badKeyEnv["TOKEN_ENCRYPTION_KEY"] = "not-32-bytes"
	if _, err := config.Load(badKeyEnv); err == nil || !strings.Contains(err.Error(), "TOKEN_ENCRYPTION_KEY") {
		t.Errorf("expected TOKEN_ENCRYPTION_KEY error, got %v", err)
	}
}

func TestParseEncryptionKey(t *testing.T) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)

	b64Key, err := config.ParseEncryptionKey(base64.StdEncoding.EncodeToString(raw))
	if err != nil || len(b64Key) != 32 {
		t.Fatalf("expected 32-byte key from base64, got len=%d err=%v", len(b64Key), err)
	}

	hexKey, err := config.ParseEncryptionKey(hex.EncodeToString(raw))
	if err != nil || len(hexKey) != 32 {
		t.Fatalf("expected 32-byte key from hex, got len=%d err=%v", len(hexKey), err)
	}

	if _, err := config.ParseEncryptionKey("too-short"); err == nil {
		t.Errorf("expected error for short key")
	}
}

func TestLogger_RedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger := config.NewLogger(&buf, "info")
	logger.Debug("should-be-filtered", nil)
	logger.Info("testing redaction", map[string]any{
		"access_token":       "secret-access",
		"refresh_token":      "secret-refresh",
		"authorization":      "Bearer secret",
		"client_secret":      "secret-client",
		"mcpAuthToken":       "secret-mcp",
		"tokenEncryptionKey": "secret-key",
		"code":               "secret-code",
		"code_verifier":      "secret-verifier",
		"token":              "secret-token",
		"nested": map[string]any{
			"accessToken": "nested-secret",
			"safe_field":  "visible-value",
		},
	})

	output := buf.String()
	for _, secret := range []string{
		"secret-access", "secret-refresh", "Bearer secret", "secret-client",
		"secret-mcp", "secret-key", "secret-code", "secret-verifier", "secret-token", "nested-secret",
	} {
		if strings.Contains(output, secret) {
			t.Errorf("expected secret %q to be redacted in log output: %s", secret, output)
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("expected valid JSON log line, got error: %v", err)
	}
	if parsed["access_token"] != "[REDACTED]" {
		t.Errorf("expected access_token to be [REDACTED], got %v", parsed["access_token"])
	}
	nested, ok := parsed["nested"].(map[string]any)
	if !ok || nested["accessToken"] != "[REDACTED]" || nested["safe_field"] != "visible-value" {
		t.Errorf("unexpected nested map in log output: %+v", parsed["nested"])
	}
}

func TestIsValidBearerToken(t *testing.T) {
	token := strings.Repeat("a", 32)
	if !config.IsValidBearerToken("Bearer "+token, token) {
		t.Errorf("expected exact bearer token to be valid")
	}
	for _, invalid := range []string{token, "Bearer " + strings.Repeat("b", 32), "Bearer " + token + "x", ""} {
		if config.IsValidBearerToken(invalid, token) {
			t.Errorf("expected %q to be rejected", invalid)
		}
	}
}

func TestLoad_WebhookOAuthAndAPIVersion(t *testing.T) {
	// 1. Safe defaults for OAuth URL and API Version
	base := baseEnv("https://mcp.example.com")
	cfg, err := config.Load(base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BitrixOAuthURL != "https://oauth.bitrix.info/oauth/token" {
		t.Errorf("expected default OAuth URL, got %q", cfg.BitrixOAuthURL)
	}
	if cfg.BitrixAPIVersion != "auto" {
		t.Errorf("expected default API version 'auto', got %q", cfg.BitrixAPIVersion)
	}

	// 2. Webhook URL relaxes APP_URL, BITRIX_CLIENT_ID, BITRIX_CLIENT_SECRET
	webhookEnv := map[string]string{
		"DATABASE_PATH":        ":memory:",
		"BITRIX_WEBHOOK_URL":   "https://my-portal.bitrix24.com/rest/1/abc123xyz456/",
		"MCP_AUTH_TOKEN":       strings.Repeat("a", 32),
		"TOKEN_ENCRYPTION_KEY": base["TOKEN_ENCRYPTION_KEY"],
	}
	wCfg, err := config.Load(webhookEnv)
	if err != nil {
		t.Fatalf("expected valid load with webhook URL only: %v", err)
	}
	if wCfg.BitrixWebhookURL != "https://my-portal.bitrix24.com/rest/1/abc123xyz456" {
		t.Errorf("expected normalized webhook URL, got %q", wCfg.BitrixWebhookURL)
	}
	if wCfg.BitrixClientID != "" || wCfg.BitrixClientSecret != "" {
		t.Errorf("expected empty client credentials in webhook mode")
	}

	// 3. Custom OAuth URL and API version
	customEnv := baseEnv("https://mcp.example.com")
	customEnv["BITRIX_OAUTH_URL"] = "https://oauth.bitrix24.tech/oauth/token"
	customEnv["BITRIX_API_VERSION"] = "v3"
	cCfg, err := config.Load(customEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cCfg.BitrixOAuthURL != "https://oauth.bitrix24.tech/oauth/token" {
		t.Errorf("expected custom OAuth URL, got %q", cCfg.BitrixOAuthURL)
	}
	if cCfg.BitrixAPIVersion != "v3" {
		t.Errorf("expected API version 'v3', got %q", cCfg.BitrixAPIVersion)
	}

	// 4. Invalid API version rejected
	badVersionEnv := baseEnv("https://mcp.example.com")
	badVersionEnv["BITRIX_API_VERSION"] = "invalid_version"
	if _, err := config.Load(badVersionEnv); err == nil || !strings.Contains(err.Error(), "BITRIX_API_VERSION") {
		t.Errorf("expected BITRIX_API_VERSION validation error, got: %v", err)
	}

	// 5. Invalid Webhook URLs
	badWebhooks := []string{
		"http://insecure.example.com/rest/1/code",         // insecure non-loopback
		"https://portal.bitrix24.com/other/1/code",        // missing /rest/
		"https://portal.bitrix24.com/rest/notdigits/code", // user id not digits
		"https://portal.bitrix24.com/rest/1",              // missing webhook code
	}
	for _, badWH := range badWebhooks {
		env := map[string]string{
			"DATABASE_PATH":        ":memory:",
			"BITRIX_WEBHOOK_URL":   badWH,
			"MCP_AUTH_TOKEN":       strings.Repeat("a", 32),
			"TOKEN_ENCRYPTION_KEY": base["TOKEN_ENCRYPTION_KEY"],
		}
		if _, err := config.Load(env); err == nil || !strings.Contains(err.Error(), "BITRIX_WEBHOOK_URL") {
			t.Errorf("expected BITRIX_WEBHOOK_URL error for %q, got: %v", badWH, err)
		}
	}

	// 6. Webhook URL parser helper
	parsedDomain, parsedUserID, parsedCode, err := config.ParseWebhookURL("https://my-portal.bitrix24.com/rest/42/secret-token-123/")
	if err != nil {
		t.Fatalf("ParseWebhookURL failed: %v", err)
	}
	if parsedDomain != "my-portal.bitrix24.com" || parsedUserID != "42" || parsedCode != "secret-token-123" {
		t.Errorf("unexpected parsed webhook parts: domain=%s user=%s code=%s", parsedDomain, parsedUserID, parsedCode)
	}
}
