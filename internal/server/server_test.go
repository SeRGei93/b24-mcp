package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/server"
	"github.com/SeRGei93/b24-mcp/internal/store"
)

const testServerKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestServer_RootMetadataAndHealth(t *testing.T) {
	cfg, err := config.Load(map[string]string{
		"APP_URL":              "http://localhost:3000",
		"DATABASE_PATH":        ":memory:",
		"BITRIX_CLIENT_ID":     "client",
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
	defer st.Close()

	logger := config.NewLogger(nil, "silent")
	if err := st.Migrate(t.Context(), logger); err != nil {
		t.Fatalf("failed to migrate store: %v", err)
	}

	handler := server.NewHandler(cfg, st, logger)

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
	var downBody map[string]string
	if err := json.Unmarshal(downRec.Body.Bytes(), &downBody); err != nil {
		t.Fatalf("invalid JSON on 503 GET /health: %v", err)
	}
	if downBody["status"] != "unavailable" || downBody["database"] != "disconnected" {
		t.Errorf("unexpected 503 health response: %+v", downBody)
	}
}
