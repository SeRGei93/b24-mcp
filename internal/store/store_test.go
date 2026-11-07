package store_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
)

const testKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestTokenCipher_RoundTripNodeCompatibilityAndTamperRejection(t *testing.T) {
	cipher, err := store.NewTokenCipher(testKeyHex)
	if err != nil {
		t.Fatalf("unexpected error creating cipher: %v", err)
	}

	// 1. Decrypt known Node.js crypto.createCipheriv("aes-256-gcm") vector
	nodeVector := "v1:qrvM3e7_ABEiM0RV:_6muzvtBRnf9znkUsMlvuQ:BaYmwgy6rLTto1qmbsVVlN9hyRTIzQA"
	decryptedNode, err := cipher.Decrypt(nodeVector)
	if err != nil {
		t.Fatalf("failed to decrypt Node.js vector: %v", err)
	}
	if decryptedNode != "bitrix-secret-token-123" {
		t.Errorf("expected 'bitrix-secret-token-123', got %q", decryptedNode)
	}

	// 2. Round-trip Go encryption -> decryption
	encrypted, err := cipher.Encrypt("sensitive-token")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	if strings.Contains(encrypted, "sensitive-token") {
		t.Errorf("encrypted payload must not contain plaintext: %s", encrypted)
	}
	if !strings.HasPrefix(encrypted, "v1:") || len(strings.Split(encrypted, ":")) != 4 {
		t.Errorf("unexpected format: %s", encrypted)
	}
	decrypted, err := cipher.Decrypt(encrypted)
	if err != nil || decrypted != "sensitive-token" {
		t.Errorf("round-trip failed: got %q, err=%v", decrypted, err)
	}

	// 3. Reject tampered ciphertext
	parts := strings.Split(encrypted, ":")
	ct := parts[3]
	if strings.HasPrefix(ct, "A") {
		parts[3] = "B" + ct[1:]
	} else {
		parts[3] = "A" + ct[1:]
	}
	if _, err := cipher.Decrypt(strings.Join(parts, ":")); err == nil {
		t.Errorf("expected error for tampered ciphertext")
	}

	// 4. Reject invalid format (including empty ciphertext part)
	for _, invalid := range []string{"v2:a:b:c", "v1:only-two:parts", "v1:a:b:c:extra", "v1:qrvM3e7_ABEiM0RV:_6muzvtBRnf9znkUsMlvuQ:"} {
		if _, err := cipher.Decrypt(invalid); err == nil {
			t.Errorf("expected error for invalid payload %q", invalid)
		}
	}
}

func TestStore_IdempotentMigrationsAndConnectivity(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "nested", "bitrix24-mcp.sqlite")
	logger := config.NewLogger(nil, "silent")

	st, err := store.NewStore(dbPath, testKeyHex)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	// Run migrations twice to verify idempotency
	if err := st.Migrate(ctx, logger); err != nil {
		t.Fatalf("first migration failed: %v", err)
	}
	if err := st.Migrate(ctx, logger); err != nil {
		t.Fatalf("second (idempotent) migration failed: %v", err)
	}

	if err := st.Ping(ctx); err != nil {
		t.Fatalf("expected Ping to succeed: %v", err)
	}

	version, err := st.DBVersion(ctx)
	if err != nil || version != 1 {
		t.Fatalf("expected goose DBVersion 1, got %d (err=%v)", version, err)
	}

	applied, err := st.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("expected AppliedMigrations to succeed: %v", err)
	}
	if len(applied) != 1 || applied[0] != "001_initial_sqlite" {
		t.Errorf("expected ['001_initial_sqlite'], got %v", applied)
	}

	// After Close(), Ping should fail
	_ = st.Close()
	if err := st.Ping(ctx); err == nil {
		t.Errorf("expected Ping to fail after Close")
	}
}

func TestStore_ConnectionsAndOAuthStates(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewStore(":memory:", testKeyHex)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	created, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:            "example.bitrix24.com",
		MemberID:          "member-1",
		BitrixUserID:      "42",
		ClientEndpoint:    "https://example.bitrix24.com/rest/",
		AccessToken:       "plain-access-1",
		RefreshToken:      "plain-refresh-1",
		Scope:             "crm",
		ExpiresAt:         time.Now().Add(time.Hour),
		Mode:              config.ModeReadOnly,
		AdminToolsEnabled: false,
	})
	if err != nil {
		t.Fatalf("UpsertConnection failed: %v", err)
	}
	if created.ID == "" || created.AccessToken != "plain-access-1" || created.Status != "active" {
		t.Fatalf("unexpected created connection: %+v", created)
	}

	// Update same member_id + bitrix_user_id on same domain
	updated, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:            "example.bitrix24.com",
		MemberID:          "member-1",
		BitrixUserID:      "42",
		ClientEndpoint:    "https://example.bitrix24.com/rest/",
		AccessToken:       "plain-access-2",
		RefreshToken:      "plain-refresh-2",
		Scope:             "crm,user",
		ExpiresAt:         time.Now().Add(2 * time.Hour),
		Mode:              config.ModeWriteWithConfirmation,
		AdminToolsEnabled: true,
	})
	if err != nil {
		t.Fatalf("UpsertConnection update failed: %v", err)
	}
	if updated.ID != created.ID || updated.AccessToken != "plain-access-2" || updated.Mode != config.ModeWriteWithConfirmation || !updated.AdminToolsEnabled {
		t.Errorf("unexpected updated connection: %+v", updated)
	}

	// Reject cross-domain overwrite for same (member_id, bitrix_user_id)
	_, err = st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:            "other.bitrix24.com",
		MemberID:          "member-1",
		BitrixUserID:      "42",
		ClientEndpoint:    "https://other.bitrix24.com/rest/",
		AccessToken:       "plain-access-3",
		RefreshToken:      "plain-refresh-3",
		Scope:             "crm",
		ExpiresAt:         time.Now().Add(time.Hour),
		Mode:              config.ModeReadOnly,
		AdminToolsEnabled: false,
	})
	if err == nil || !strings.Contains(err.Error(), "different portal domain") {
		t.Errorf("expected cross-domain conflict error, got %v", err)
	}

	// OAuth state create + single-use consume
	if err := st.CreateOAuthState(ctx, "raw-state-1", "example.bitrix24.com", config.ModeReadOnly, time.Now().Add(time.Minute), ""); err != nil {
		t.Fatalf("CreateOAuthState failed: %v", err)
	}
	state, err := st.ConsumeOAuthState(ctx, "raw-state-1")
	if err != nil || state == nil {
		t.Fatalf("expected state to be consumed, got state=%+v err=%v", state, err)
	}
	if state.Domain != "example.bitrix24.com" || state.Mode != config.ModeReadOnly || state.McpRequestHash != "" {
		t.Errorf("unexpected state: %+v", state)
	}
	again, err := st.ConsumeOAuthState(ctx, "raw-state-1")
	if err != nil || again != nil {
		t.Errorf("expected second consume to return nil, got %+v err=%v", again, err)
	}
}

