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

	conns, err := st.ListConnections(ctx)
	if err != nil || len(conns) != 0 {
		t.Fatalf("expected empty connections list on fresh migrated store, got %d (err=%v)", len(conns), err)
	}

	// Re-open the existing SQLite database file and verify Migrate is a no-op that preserves data
	st2, err := store.NewStore(dbPath, testKeyHex)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	defer st2.Close()
	if err := st2.Migrate(ctx, logger); err != nil {
		t.Fatalf("migration on reopened database failed: %v", err)
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

func TestStore_McpOAuthLifecycleAndPruning(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewStore(":memory:", testKeyHex)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-oauth",
		BitrixUserID:   "7",
		ClientEndpoint: "https://example.bitrix24.com/rest/",
		AccessToken:    "enc-access",
		RefreshToken:   "enc-refresh",
		Scope:          "crm",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("UpsertConnection failed: %v", err)
	}

	// Register an unused client created > 30 days ago to verify pruning on next registration
	staleClient := store.McpOAuthClient{
		ClientID:                "stale-client",
		RedirectURIs:            []string{"http://127.0.0.1:3000/callback"},
		TokenEndpointAuthMethod: "none",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		ClientName:              "Stale Client",
		Scope:                   "mcp:tools",
		CreatedAt:               time.Now().Add(-31 * 24 * time.Hour),
	}
	if err := st.RegisterOAuthClient(ctx, staleClient); err != nil {
		t.Fatalf("RegisterOAuthClient (stale) failed: %v", err)
	}

	// Register active client -> should prune stale-client
	activeClient := store.McpOAuthClient{
		ClientID:                "client-1",
		RedirectURIs:            []string{"http://127.0.0.1:3000/callback"},
		TokenEndpointAuthMethod: "none",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		ClientName:              "SQLite Client",
		Scope:                   "mcp:tools",
		CreatedAt:               time.Now(),
	}
	if err := st.RegisterOAuthClient(ctx, activeClient); err != nil {
		t.Fatalf("RegisterOAuthClient (active) failed: %v", err)
	}

	pruned, err := st.GetOAuthClient(ctx, "stale-client")
	if err != nil || pruned != nil {
		t.Errorf("expected stale-client to be pruned, got %+v err=%v", pruned, err)
	}

	gotClient, err := st.GetOAuthClient(ctx, "client-1")
	if err != nil || gotClient == nil || len(gotClient.RedirectURIs) != 1 || gotClient.RedirectURIs[0] != "http://127.0.0.1:3000/callback" {
		t.Fatalf("unexpected client-1: %+v err=%v", gotClient, err)
	}

	// Create authorization request
	err = st.CreateAuthorizationRequest(ctx, store.McpOAuthAuthorizationRequest{
		RequestHash:   "req-hash-1",
		ClientID:      "client-1",
		RedirectURI:   "http://127.0.0.1:3000/callback",
		State:         "state-1",
		CodeChallenge: "challenge-1",
		Resource:      "https://mcp.example.com/mcp",
		Scope:         "mcp:tools",
		ExpiresAt:     time.Now().Add(time.Minute),
		Attempts:      0,
	})
	if err != nil {
		t.Fatalf("CreateAuthorizationRequest failed: %v", err)
	}

	consumedReq, err := st.ConsumeAuthorizationRequestAndCreateCode(
		ctx,
		"req-hash-1",
		"code-hash-1",
		time.Now().Add(time.Minute),
		conn.ID,
	)
	if err != nil || consumedReq == nil || consumedReq.ClientID != "client-1" || consumedReq.State != "state-1" {
		t.Fatalf("unexpected consumedReq: %+v err=%v", consumedReq, err)
	}

	// Single-use authorization code consumption
	consumedCode, err := st.ConsumeAuthorizationCode(ctx, "code-hash-1")
	if err != nil || consumedCode == nil || consumedCode.ConnectionID != conn.ID {
		t.Fatalf("unexpected consumedCode: %+v err=%v", consumedCode, err)
	}
	secondCode, err := st.ConsumeAuthorizationCode(ctx, "code-hash-1")
	if err != nil || secondCode != nil {
		t.Errorf("expected second ConsumeAuthorizationCode to return nil, got %+v err=%v", secondCode, err)
	}

	// Issue token pair, find, rotate, and revoke
	err = st.IssueTokenPair(ctx, store.NewMcpOAuthTokenRecord{
		AccessTokenHash:  "access-1",
		RefreshTokenHash: "refresh-1",
		ClientID:         "client-1",
		ConnectionID:     conn.ID,
		Resource:         "https://mcp.example.com/mcp",
		Scope:            "mcp:tools",
		AccessExpiresAt:  time.Now().Add(time.Hour),
		RefreshExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("IssueTokenPair failed: %v", err)
	}

	foundAccess, err := st.FindAccessToken(ctx, "access-1")
	if err != nil || foundAccess == nil || foundAccess.ConnectionID != conn.ID {
		t.Fatalf("expected access-1 to be found, got %+v err=%v", foundAccess, err)
	}

	rotated, err := st.RotateRefreshToken(ctx, "refresh-1", "client-1", "https://mcp.example.com/mcp", store.NewMcpOAuthTokenRecord{
		AccessTokenHash:  "access-2",
		RefreshTokenHash: "refresh-2",
		ClientID:         "client-1",
		ConnectionID:     conn.ID,
		Resource:         "https://mcp.example.com/mcp",
		Scope:            "mcp:tools",
		AccessExpiresAt:  time.Now().Add(time.Hour),
		RefreshExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil || rotated == nil || rotated.AccessTokenHash != "access-1" {
		t.Fatalf("expected refresh-1 to rotate, got %+v err=%v", rotated, err)
	}

	// Old access token and old refresh token must now be invalid
	oldAccess, err := st.FindAccessToken(ctx, "access-1")
	if err != nil || oldAccess != nil {
		t.Errorf("expected old access-1 to be revoked after rotation, got %+v err=%v", oldAccess, err)
	}
	reusedRefresh, err := st.FindRefreshToken(ctx, "refresh-1", "client-1", "https://mcp.example.com/mcp")
	if err != nil || reusedRefresh != nil {
		t.Errorf("expected old refresh-1 to be unavailable after rotation, got %+v err=%v", reusedRefresh, err)
	}

	// New access token must be valid until revoked
	newAccess, err := st.FindAccessToken(ctx, "access-2")
	if err != nil || newAccess == nil || newAccess.AccessTokenHash != "access-2" {
		t.Fatalf("expected access-2 to be valid, got %+v err=%v", newAccess, err)
	}
	if err := st.RevokeToken(ctx, "access-2"); err != nil {
		t.Fatalf("RevokeToken failed: %v", err)
	}
	revokedAccess, err := st.FindAccessToken(ctx, "access-2")
	if err != nil || revokedAccess != nil {
		t.Errorf("expected access-2 to be nil after revoke, got %+v err=%v", revokedAccess, err)
	}
}

func TestStore_SchemaCache(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewStore(":memory:", testKeyHex)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	conn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "example.bitrix24.com",
		MemberID:       "member-cache",
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

	payload := []map[string]any{{"entityTypeId": float64(2), "title": "Deal"}}
	if err := st.SetSchemaCache(ctx, conn.ID, "entity-types", payload); err != nil {
		t.Fatalf("SetSchemaCache failed: %v", err)
	}

	var got []map[string]any
	found, err := st.GetSchemaCache(ctx, conn.ID, "entity-types", 60, &got)
	if err != nil || !found || len(got) != 1 || got[0]["title"] != "Deal" {
		t.Fatalf("expected cached entity-types, found=%v got=%+v err=%v", found, got, err)
	}

	cleared, err := st.ClearSchemaCache(ctx, conn.ID)
	if err != nil || cleared != 1 {
		t.Fatalf("expected 1 cleared entry, got %d err=%v", cleared, err)
	}

	foundAfter, err := st.GetSchemaCache(ctx, conn.ID, "entity-types", 60, &got)
	if err != nil || foundAfter {
		t.Errorf("expected cache miss after ClearSchemaCache, found=%v err=%v", foundAfter, err)
	}
}

func TestStore_PendingOperationsAndAuditLogs(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewStore(":memory:", testKeyHex)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

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

	// 1. Create pending operation
	op, err := st.CreatePendingOperation(ctx, store.CreatePendingOperationInput{
		ConnectionID:  conn.ID,
		OperationType: "create_item",
		Method:        "crm.item.add",
		Params:        map[string]any{"entityTypeId": 2, "fields": map[string]any{"title": "Deal 1"}},
		HumanSummary:  "Create Deal 1",
		ExpiresAt:     time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreatePendingOperation failed: %v", err)
	}
	if op.Status != "pending" || op.Method != "crm.item.add" {
		t.Fatalf("unexpected op: %+v", op)
	}

	// 2. Claim pending operation
	claimed, err := st.ClaimPendingOperation(ctx, op.ID)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimPendingOperation failed: %v", err)
	}
	if claimed.Status != "executing" {
		t.Errorf("expected status 'executing', got %s", claimed.Status)
	}
	// Re-claim must return nil
	reclaimed, err := st.ClaimPendingOperation(ctx, op.ID)
	if err != nil || reclaimed != nil {
		t.Errorf("expected re-claim to return nil, got %+v err=%v", reclaimed, err)
	}

	// 3. Complete pending operation
	if err := st.CompletePendingOperation(ctx, op.ID, map[string]any{"id": 42}); err != nil {
		t.Fatalf("CompletePendingOperation failed: %v", err)
	}
	completed, err := st.GetPendingOperation(ctx, op.ID)
	if err != nil || completed.Status != "completed" {
		t.Fatalf("expected completed op, got %+v err=%v", completed, err)
	}
	if completed.Params["entityTypeId"] != float64(2) {
		t.Errorf("expected decoded Params[entityTypeId]=2, got %+v", completed.Params)
	}
	resMap, ok := completed.Result.(map[string]any)
	if !ok || resMap["id"] != float64(42) {
		t.Errorf("expected decoded Result={id:42}, got %+v", completed.Result)
	}

	// 4. Cancel pending operation
	op2, err := st.CreatePendingOperation(ctx, store.CreatePendingOperationInput{
		ConnectionID:  conn.ID,
		OperationType: "update_item",
		Method:        "crm.item.update",
		Params:        map[string]any{"entityTypeId": 2, "id": 42},
		HumanSummary:  "Update Deal",
		ExpiresAt:     time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreatePendingOperation 2 failed: %v", err)
	}
	cancelled, err := st.CancelPendingOperation(ctx, op2.ID)
	if err != nil || cancelled == nil || cancelled.Status != "cancelled" {
		t.Fatalf("expected cancelled op, got %+v err=%v", cancelled, err)
	}

	// 5. Audit logs
	if err := st.RecordAuditLog(ctx, store.RecordAuditLogInput{
		ConnectionID: conn.ID,
		OperationID:  op.ID,
		Action:       "create_item",
		Outcome:      "completed",
		Details:      map[string]any{"method": "crm.item.add"},
	}); err != nil {
		t.Fatalf("RecordAuditLog failed: %v", err)
	}

	// 6. List pending operations
	listed, err := st.ListPendingOperations(ctx, &conn.ID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("expected 2 listed ops, got %d err=%v", len(listed), err)
	}
}

func TestStore_EnsureWebhookConnectionAndIsWebhook(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewStore(":memory:", testKeyHex)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatalf("failed to migrate store: %v", err)
	}

	cfg := &config.Config{
		BitrixWebhookURL:   "https://webhook-portal.bitrix24.com/rest/7/secret-hook-code/",
		BitrixPortalDomain: "webhook-portal.bitrix24.com",
		DefaultMode:        config.ModeWriteWithConfirmation,
		AdminToolsEnabled:  true,
	}

	// 1. First seeding creates the connection
	conn, err := st.EnsureWebhookConnection(ctx, cfg)
	if err != nil {
		t.Fatalf("EnsureWebhookConnection failed: %v", err)
	}
	if conn == nil {
		t.Fatalf("expected non-nil connection")
	}
	if !conn.IsWebhook() {
		t.Errorf("expected conn.IsWebhook() to be true")
	}
	if conn.Domain != "webhook-portal.bitrix24.com" {
		t.Errorf("expected domain 'webhook-portal.bitrix24.com', got %q", conn.Domain)
	}
	if conn.BitrixUserID != "7" {
		t.Errorf("expected BitrixUserID '7', got %q", conn.BitrixUserID)
	}
	if conn.Mode != config.ModeWriteWithConfirmation {
		t.Errorf("expected mode %q, got %q", config.ModeWriteWithConfirmation, conn.Mode)
	}
	if !conn.AdminToolsEnabled {
		t.Errorf("expected AdminToolsEnabled true")
	}
	if conn.Status != store.StatusActive {
		t.Errorf("expected status 'active', got %q", conn.Status)
	}
	if conn.AccessToken != "secret-hook-code" {
		t.Errorf("expected AccessToken 'secret-hook-code', got %q", conn.AccessToken)
	}

	// 2. Second seeding is idempotent and updates if config changes
	cfg.DefaultMode = config.ModeReadOnly
	updatedConn, err := st.EnsureWebhookConnection(ctx, cfg)
	if err != nil {
		t.Fatalf("EnsureWebhookConnection second call failed: %v", err)
	}
	if updatedConn.ID != conn.ID {
		t.Errorf("expected same connection ID, got %s vs %s", updatedConn.ID, conn.ID)
	}
	if updatedConn.Mode != config.ModeReadOnly {
		t.Errorf("expected updated mode %q, got %q", config.ModeReadOnly, updatedConn.Mode)
	}

	// 3. Regular OAuth connection returns IsWebhook() == false
	oauthConn, err := st.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:         "oauth-portal.bitrix24.com",
		MemberID:       "oauth-member-1",
		BitrixUserID:   "1",
		ClientEndpoint: "https://oauth-portal.bitrix24.com/rest/",
		AccessToken:    "token",
		RefreshToken:   "refresh",
		ExpiresAt:      time.Now().Add(time.Hour),
		Mode:           config.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("UpsertConnection failed: %v", err)
	}
	if oauthConn.IsWebhook() {
		t.Errorf("expected oauthConn.IsWebhook() to be false")
	}

	// 4. EnsureWebhookConnection with empty webhook URL returns nil, nil
	noHookCfg := &config.Config{}
	none, err := st.EnsureWebhookConnection(ctx, noHookCfg)
	if err != nil || none != nil {
		t.Fatalf("expected nil, nil for empty webhook URL, got %+v err=%v", none, err)
	}
}
