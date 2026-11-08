package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

const (
	cipherVersion = "v1"
	gcmTagSize    = 16
	gcmNonceSize  = 12
	isoTimeFormat = "2006-01-02T15:04:05.000Z"
)

type TokenCipher struct {
	aead cipher.AEAD
}

func NewTokenCipher(rawKey string) (*TokenCipher, error) {
	key, err := config.ParseEncryptionKey(rawKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &TokenCipher{aead: aead}, nil
}

func (c *TokenCipher) Encrypt(plaintext string) (string, error) {
	iv := make([]byte, gcmNonceSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nil, iv, []byte(plaintext), nil)
	if len(sealed) < gcmTagSize {
		return "", errors.New("unexpected sealed payload length")
	}
	ciphertext := sealed[:len(sealed)-gcmTagSize]
	tag := sealed[len(sealed)-gcmTagSize:]

	return strings.Join([]string{
		cipherVersion,
		base64.RawURLEncoding.EncodeToString(iv),
		base64.RawURLEncoding.EncodeToString(tag),
		base64.RawURLEncoding.EncodeToString(ciphertext),
	}, ":"), nil
}

func (c *TokenCipher) Decrypt(payload string) (string, error) {
	parts := strings.Split(payload, ":")
	if len(parts) != 4 || parts[0] != cipherVersion || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return "", errors.New("Encrypted token has an unsupported or invalid format")
	}
	iv, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(iv) != gcmNonceSize {
		return "", errors.New("Encrypted token has an unsupported or invalid format")
	}
	tag, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(tag) != gcmTagSize {
		return "", errors.New("Encrypted token has an unsupported or invalid format")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return "", errors.New("Encrypted token has an unsupported or invalid format")
	}

	combined := make([]byte, 0, len(ciphertext)+len(tag))
	combined = append(combined, ciphertext...)
	combined = append(combined, tag...)

	plaintext, err := c.aead.Open(nil, iv, combined, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt token: %w", err)
	}
	return string(plaintext), nil
}

func HashSecret(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

type ConnectionStatus string

const (
	StatusActive   ConnectionStatus = "active"
	StatusDisabled ConnectionStatus = "disabled"
	StatusRevoked  ConnectionStatus = "revoked"
)

type OperationType string

const (
	OpCreateItem    OperationType = "create_item"
	OpUpdateItem    OperationType = "update_item"
	OpMoveItemStage OperationType = "move_item_stage"
	OpDeleteItem    OperationType = "delete_item"
)

type OperationStatus string

const (
	OpStatusPending   OperationStatus = "pending"
	OpStatusExecuting OperationStatus = "executing"
	OpStatusCompleted OperationStatus = "completed"
	OpStatusCancelled OperationStatus = "cancelled"
	OpStatusExpired   OperationStatus = "expired"
	OpStatusFailed    OperationStatus = "failed"
)

type Connection struct {
	ID                string
	Domain            string
	MemberID          string
	BitrixUserID      string
	ClientEndpoint    string
	AccessToken       string
	RefreshToken      string
	Scope             string
	ExpiresAt         time.Time
	Mode              config.ConnectionMode
	AdminToolsEnabled bool
	Status            ConnectionStatus
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type PublicConnection struct {
	ID                string                `json:"id"`
	Domain            string                `json:"domain"`
	MemberID          string                `json:"memberId"`
	BitrixUserID      string                `json:"bitrixUserId"`
	Scope             string                `json:"scope"`
	ExpiresAt         string                `json:"expiresAt"`
	Mode              config.ConnectionMode `json:"mode"`
	AdminToolsEnabled bool                  `json:"adminToolsEnabled"`
	Status            ConnectionStatus      `json:"status"`
	CreatedAt         string                `json:"createdAt"`
	UpdatedAt         string                `json:"updatedAt"`
}

func (c Connection) ToPublic() PublicConnection {
	return PublicConnection{
		ID:                c.ID,
		Domain:            c.Domain,
		MemberID:          c.MemberID,
		BitrixUserID:      c.BitrixUserID,
		Scope:             c.Scope,
		ExpiresAt:         c.ExpiresAt.UTC().Format(isoTimeFormat),
		Mode:              c.Mode,
		AdminToolsEnabled: c.AdminToolsEnabled,
		Status:            c.Status,
		CreatedAt:         c.CreatedAt.UTC().Format(isoTimeFormat),
		UpdatedAt:         c.UpdatedAt.UTC().Format(isoTimeFormat),
	}
}

func (c *Connection) IsWebhook() bool {
	if c == nil {
		return false
	}
	if strings.HasPrefix(c.MemberID, "webhook") {
		return true
	}
	if u, err := url.Parse(c.ClientEndpoint); err == nil {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i := 0; i+2 < len(parts); i++ {
			if parts[i] == "rest" && parts[i+1] != "api" && parts[i+1] != "" {
				return true
			}
		}
	}
	return false
}

type UpsertConnectionInput struct {
	Domain            string
	MemberID          string
	BitrixUserID      string
	ClientEndpoint    string
	AccessToken       string
	RefreshToken      string
	Scope             string
	ExpiresAt         time.Time
	Mode              config.ConnectionMode
	AdminToolsEnabled bool
}

type UpdateTokensInput struct {
	AccessToken    string
	RefreshToken   string
	ExpiresAt      time.Time
	Scope          string
	Domain         string
	ClientEndpoint string
	BitrixUserID   string
}

type OAuthStateRecord struct {
	Domain         string
	Mode           config.ConnectionMode
	McpRequestHash string
}

type McpOAuthClient struct {
	ClientID                string
	RedirectURIs            []string
	TokenEndpointAuthMethod string
	GrantTypes              []string
	ResponseTypes           []string
	ClientName              string
	ClientURI               string
	LogoURI                 string
	Scope                   string
	SoftwareID              string
	SoftwareVersion         string
	CreatedAt               time.Time
}

type McpOAuthAuthorizationRequest struct {
	RequestHash   string
	ClientID      string
	RedirectURI   string
	State         string
	CodeChallenge string
	Resource      string
	Scope         string
	ExpiresAt     time.Time
	Attempts      int
}

type McpOAuthAuthorizationCode struct {
	CodeHash      string
	ClientID      string
	ConnectionID  string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	Scope         string
	ExpiresAt     time.Time
}

type McpOAuthTokenRecord struct {
	ID               string
	AccessTokenHash  string
	RefreshTokenHash string
	ClientID         string
	ConnectionID     string
	Resource         string
	Scope            string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
	RevokedAt        *time.Time
	CreatedAt        time.Time
}

type NewMcpOAuthTokenRecord struct {
	AccessTokenHash  string
	RefreshTokenHash string
	ClientID         string
	ConnectionID     string
	Resource         string
	Scope            string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

type PendingOperation struct {
	ID            string          `json:"id"`
	ConnectionID  string          `json:"connectionId"`
	OperationType OperationType   `json:"operationType"`
	Method        string          `json:"method"`
	ParamsJSON    string          `json:"-"`
	Params        map[string]any  `json:"params"`
	HumanSummary  string          `json:"humanSummary"`
	Status        OperationStatus `json:"status"`
	ResultJSON    string          `json:"-"`
	Result        any             `json:"result"`
	ErrorMessage  string          `json:"errorMessage,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
	ExpiresAt     time.Time       `json:"expiresAt"`
	ConfirmedAt   *time.Time      `json:"confirmedAt,omitempty"`
	CompletedAt   *time.Time      `json:"completedAt,omitempty"`
}

type CreatePendingOperationInput struct {
	ConnectionID  string
	OperationType OperationType
	Method        string
	Params        map[string]any
	HumanSummary  string
	ExpiresAt     time.Time
}

type RecordAuditLogInput struct {
	ConnectionID string
	OperationID  string
	Action       string
	Outcome      string
	Details      map[string]any
}

type Store struct {
	db     *sql.DB
	cipher *TokenCipher
}

func NewStore(databasePath, rawEncryptionKey string) (*Store, error) {
	cipher, err := NewTokenCipher(rawEncryptionKey)
	if err != nil {
		return nil, err
	}

	normalized := config.NormalizeSqlitePath(databasePath)
	dsn := normalized
	if normalized != ":memory:" {
		absPath, err := filepath.Abs(normalized)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			return nil, err
		}
		dsn = absPath
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	if normalized != ":memory:" {
		if _, err := db.Exec("PRAGMA journal_mode = WAL;"); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db, cipher: cipher}, nil
}

func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func (s *Store) Migrate(ctx context.Context, logger *config.Logger) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			id TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		return err
	}

	const migrationID = "001_initial_sqlite"
	var existing string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM schema_migrations WHERE id = ?1", migrationID).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	sqlBytes, err := embeddedMigrations.ReadFile("migrations/001_initial_sqlite.sql")
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
		return err
	}
	now := time.Now().UTC().Format(isoTimeFormat)
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (id, applied_at) VALUES (?1, ?2)", migrationID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if logger != nil {
		logger.Info("Database migration applied", map[string]any{
			"migrationId": migrationID,
		})
	}
	return nil
}

func (s *Store) ListConnections(ctx context.Context) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, domain, member_id, bitrix_user_id, client_endpoint,
		       access_token_encrypted, refresh_token_encrypted, scope,
		       expires_at, mode, admin_tools_enabled, status, created_at, updated_at
		FROM bitrix_connections
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []Connection
	for rows.Next() {
		conn, err := s.scanConnection(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *conn)
	}
	return result, rows.Err()
}

func (s *Store) GetConnection(ctx context.Context, id string) (*Connection, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, domain, member_id, bitrix_user_id, client_endpoint,
		       access_token_encrypted, refresh_token_encrypted, scope,
		       expires_at, mode, admin_tools_enabled, status, created_at, updated_at
		FROM bitrix_connections
		WHERE id = ?1
	`, id)
	conn, err := s.scanConnection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, config.NewNotFoundError(fmt.Sprintf("Bitrix connection '%s' was not found", id))
	}
	return conn, err
}

func (s *Store) UpsertConnection(ctx context.Context, input UpsertConnectionInput) (*Connection, error) {
	encAccess, err := s.cipher.Encrypt(input.AccessToken)
	if err != nil {
		return nil, err
	}
	var encRefresh string
	if input.RefreshToken != "" {
		encRefresh, err = s.cipher.Encrypt(input.RefreshToken)
		if err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC().Format(isoTimeFormat)
	adminInt := 0
	if input.AdminToolsEnabled {
		adminInt = 1
	}

	row := s.db.QueryRowContext(ctx, `
		INSERT INTO bitrix_connections (
			id, domain, member_id, bitrix_user_id, client_endpoint, access_token_encrypted,
			refresh_token_encrypted, scope, expires_at, mode, admin_tools_enabled, status,
			created_at, updated_at
		) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,'active',?12,?12)
		ON CONFLICT (member_id, bitrix_user_id) DO UPDATE SET
			domain = excluded.domain,
			bitrix_user_id = excluded.bitrix_user_id,
			client_endpoint = excluded.client_endpoint,
			access_token_encrypted = excluded.access_token_encrypted,
			refresh_token_encrypted = excluded.refresh_token_encrypted,
			scope = excluded.scope,
			expires_at = excluded.expires_at,
			mode = excluded.mode,
			admin_tools_enabled = excluded.admin_tools_enabled,
			status = 'active',
			updated_at = excluded.updated_at
		WHERE bitrix_connections.domain = excluded.domain
		RETURNING id, domain, member_id, bitrix_user_id, client_endpoint,
		          access_token_encrypted, refresh_token_encrypted, scope,
		          expires_at, mode, admin_tools_enabled, status, created_at, updated_at
	`,
		uuid.NewString(),
		input.Domain,
		input.MemberID,
		input.BitrixUserID,
		input.ClientEndpoint,
		encAccess,
		encRefresh,
		input.Scope,
		input.ExpiresAt.UTC().Format(isoTimeFormat),
		string(input.Mode),
		adminInt,
		now,
	)

	conn, err := s.scanConnection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, config.NewPolicyError("Bitrix24 member_id is already bound to a different portal domain")
	}
	return conn, err
}

func (s *Store) EnsureWebhookConnection(ctx context.Context, cfg *config.Config) (*Connection, error) {
	if cfg == nil || strings.TrimSpace(cfg.BitrixWebhookURL) == "" {
		return nil, nil
	}
	domain, userID, code, err := config.ParseWebhookURL(cfg.BitrixWebhookURL)
	if err != nil {
		return nil, err
	}
	if cfg.BitrixPortalDomain != "" && !strings.EqualFold(cfg.BitrixPortalDomain, domain) {
		domain = cfg.BitrixPortalDomain
	}

	scheme := "https"
	if strings.HasPrefix(cfg.BitrixWebhookURL, "http://") {
		scheme = "http"
	}
	clientEndpoint := fmt.Sprintf("%s://%s/rest/%s/%s/", scheme, domain, userID, code)

	return s.UpsertConnection(ctx, UpsertConnectionInput{
		Domain:            domain,
		MemberID:          "webhook:" + domain,
		BitrixUserID:      userID,
		ClientEndpoint:    clientEndpoint,
		AccessToken:       code,
		RefreshToken:      "",
		Scope:             "all",
		ExpiresAt:         time.Now().Add(100 * 365 * 24 * time.Hour),
		Mode:              cfg.DefaultMode,
		AdminToolsEnabled: cfg.AdminToolsEnabled,
	})
}

func (s *Store) UpdateConnectionTokens(ctx context.Context, id string, input UpdateTokensInput) (*Connection, error) {
	encAccess, err := s.cipher.Encrypt(input.AccessToken)
	if err != nil {
		return nil, err
	}
	var encRefresh string
	if input.RefreshToken != "" {
		encRefresh, err = s.cipher.Encrypt(input.RefreshToken)
		if err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC().Format(isoTimeFormat)

	row := s.db.QueryRowContext(ctx, `
		UPDATE bitrix_connections SET
			access_token_encrypted = ?2,
			refresh_token_encrypted = ?3,
			expires_at = ?4,
			scope = ?5,
			domain = ?6,
			client_endpoint = ?7,
			bitrix_user_id = ?8,
			status = 'active',
			updated_at = ?9
		WHERE id = ?1
		RETURNING id, domain, member_id, bitrix_user_id, client_endpoint,
		          access_token_encrypted, refresh_token_encrypted, scope,
		          expires_at, mode, admin_tools_enabled, status, created_at, updated_at
	`,
		id,
		encAccess,
		encRefresh,
		input.ExpiresAt.UTC().Format(isoTimeFormat),
		input.Scope,
		input.Domain,
		input.ClientEndpoint,
		input.BitrixUserID,
		now,
	)

	conn, err := s.scanConnection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, config.NewNotFoundError(fmt.Sprintf("Bitrix connection '%s' was not found", id))
	}
	return conn, err
}

func (s *Store) MarkConnectionRevoked(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(isoTimeFormat)
	_, err := s.db.ExecContext(ctx, "UPDATE bitrix_connections SET status = 'revoked', updated_at = ?2 WHERE id = ?1", id, now)
	return err
}

func (s *Store) CreateOAuthState(
	ctx context.Context,
	rawState, domain string,
	mode config.ConnectionMode,
	expiresAt time.Time,
	mcpRequestHash string,
) error {
	now := time.Now().UTC().Format(isoTimeFormat)
	if _, err := s.db.ExecContext(ctx, "DELETE FROM oauth_states WHERE expires_at <= ?1", now); err != nil {
		return err
	}
	var reqHash any
	if mcpRequestHash != "" {
		reqHash = mcpRequestHash
	}
	_, err := s.db.ExecContext(
		ctx,
		"INSERT INTO oauth_states (state_hash, domain, mode, expires_at, created_at, mcp_request_hash) VALUES (?1,?2,?3,?4,?5,?6)",
		HashSecret(rawState),
		domain,
		string(mode),
		expiresAt.UTC().Format(isoTimeFormat),
		now,
		reqHash,
	)
	return err
}

func (s *Store) ConsumeOAuthState(ctx context.Context, rawState string) (*OAuthStateRecord, error) {
	now := time.Now().UTC().Format(isoTimeFormat)
	var domain, mode string
	var mcpReqHash sql.NullString
	err := s.db.QueryRowContext(ctx, `
		DELETE FROM oauth_states
		WHERE state_hash = ?1 AND expires_at > ?2
		RETURNING domain, mode, mcp_request_hash
	`, HashSecret(rawState), now).Scan(&domain, &mode, &mcpReqHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &OAuthStateRecord{
		Domain:         domain,
		Mode:           config.ConnectionMode(mode),
		McpRequestHash: mcpReqHash.String,
	}, nil
}

func (s *Store) GetSchemaCache(ctx context.Context, connectionID, key string, maxAgeSeconds int, dest any) (bool, error) {
	minFetchedAt := time.Now().UTC().Add(-time.Duration(maxAgeSeconds) * time.Second).Format(isoTimeFormat)
	var payload string
	err := s.db.QueryRowContext(ctx, `
		SELECT payload FROM schema_cache
		WHERE connection_id = ?1 AND cache_key = ?2
		  AND fetched_at > ?3
	`, connectionID, key, minFetchedAt).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(payload), dest); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) SetSchemaCache(ctx context.Context, connectionID, key string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(isoTimeFormat)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO schema_cache (connection_id, cache_key, payload, fetched_at)
		VALUES (?1,?2,?3,?4)
		ON CONFLICT (connection_id, cache_key) DO UPDATE SET
			payload = excluded.payload,
			fetched_at = excluded.fetched_at
	`, connectionID, key, string(encoded), now)
	return err
}

func (s *Store) ClearSchemaCache(ctx context.Context, connectionID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM schema_cache WHERE connection_id = ?1", connectionID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) CreatePendingOperation(ctx context.Context, input CreatePendingOperationInput) (*PendingOperation, error) {
	paramsBytes, err := json.Marshal(input.Params)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	now := time.Now().UTC().Format(isoTimeFormat)
	exp := input.ExpiresAt.UTC().Format(isoTimeFormat)

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO pending_operations (
			id, connection_id, operation_type, method, params_json, human_summary,
			status, created_at, expires_at
		) VALUES (?1,?2,?3,?4,?5,?6,'pending',?7,?8)
	`,
		id,
		input.ConnectionID,
		string(input.OperationType),
		input.Method,
		string(paramsBytes),
		input.HumanSummary,
		now,
		exp,
	)
	if err != nil {
		return nil, err
	}
	return s.GetPendingOperation(ctx, id)
}

func (s *Store) GetPendingOperation(ctx context.Context, id string) (*PendingOperation, error) {
	_ = s.expireStalePendingOperations(ctx)
	row := s.db.QueryRowContext(ctx, `
		SELECT id, connection_id, operation_type, method, params_json, human_summary,
		       status, result_json, error_message, created_at, expires_at, confirmed_at, completed_at
		FROM pending_operations
		WHERE id = ?1
	`, id)
	op, err := scanPendingOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, config.NewNotFoundError("Pending operation was not found")
	}
	return op, err
}

func (s *Store) ListPendingOperations(ctx context.Context, connectionID *string) ([]PendingOperation, error) {
	_ = s.expireStalePendingOperations(ctx)
	var rows *sql.Rows
	var err error
	if connectionID != nil && *connectionID != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, connection_id, operation_type, method, params_json, human_summary,
			       status, result_json, error_message, created_at, expires_at, confirmed_at, completed_at
			FROM pending_operations
			WHERE connection_id = ?1
			ORDER BY created_at DESC
			LIMIT 100
		`, *connectionID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, connection_id, operation_type, method, params_json, human_summary,
			       status, result_json, error_message, created_at, expires_at, confirmed_at, completed_at
			FROM pending_operations
			ORDER BY created_at DESC
			LIMIT 100
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []PendingOperation
	for rows.Next() {
		op, err := scanPendingOperation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *op)
	}
	return result, rows.Err()
}

func (s *Store) ClaimPendingOperation(ctx context.Context, id string) (*PendingOperation, error) {
	now := time.Now().UTC().Format(isoTimeFormat)
	row := s.db.QueryRowContext(ctx, `
		UPDATE pending_operations
		SET status = 'executing', confirmed_at = ?2
		WHERE id = ?1 AND status = 'pending' AND expires_at > ?2
		RETURNING id, connection_id, operation_type, method, params_json, human_summary,
		          status, result_json, error_message, created_at, expires_at, confirmed_at, completed_at
	`, id, now)
	op, err := scanPendingOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return op, err
}

func (s *Store) CompletePendingOperation(ctx context.Context, id string, result any) error {
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(isoTimeFormat)
	_, err = s.db.ExecContext(ctx, `
		UPDATE pending_operations
		SET status = 'completed', result_json = ?2, completed_at = ?3
		WHERE id = ?1 AND status = 'executing'
	`, id, string(resultBytes), now)
	return err
}

func (s *Store) FailPendingOperation(ctx context.Context, id string, errorMessage string) error {
	now := time.Now().UTC().Format(isoTimeFormat)
	if len(errorMessage) > 1000 {
		errorMessage = errorMessage[:1000]
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE pending_operations
		SET status = 'failed', error_message = ?2, completed_at = ?3
		WHERE id = ?1 AND status = 'executing'
	`, id, errorMessage, now)
	return err
}

func (s *Store) CancelPendingOperation(ctx context.Context, id string) (*PendingOperation, error) {
	now := time.Now().UTC().Format(isoTimeFormat)
	row := s.db.QueryRowContext(ctx, `
		UPDATE pending_operations
		SET status = 'cancelled', completed_at = ?2
		WHERE id = ?1 AND status = 'pending'
		RETURNING id, connection_id, operation_type, method, params_json, human_summary,
		          status, result_json, error_message, created_at, expires_at, confirmed_at, completed_at
	`, id, now)
	op, err := scanPendingOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return op, err
}

func (s *Store) RecordAuditLog(ctx context.Context, input RecordAuditLogInput) error {
	detailsJSON, err := json.Marshal(input.Details)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(isoTimeFormat)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO audit_logs (id, connection_id, operation_id, action, outcome, details_json, created_at)
		VALUES (?1,?2,?3,?4,?5,?6,?7)
	`,
		uuid.NewString(),
		nullableString(input.ConnectionID),
		nullableString(input.OperationID),
		input.Action,
		input.Outcome,
		string(detailsJSON),
		now,
	)
	return err
}

func (s *Store) expireStalePendingOperations(ctx context.Context) error {
	now := time.Now().UTC().Format(isoTimeFormat)
	_, err := s.db.ExecContext(ctx, `
		UPDATE pending_operations
		SET status = 'expired', completed_at = ?1
		WHERE status = 'pending' AND expires_at <= ?1
	`, now)
	return err
}

func scanPendingOperation(scanner rowScanner) (*PendingOperation, error) {
	var (
		op                                               PendingOperation
		opTypeStr, statusStr                             string
		resultJSON, errorMsg, confirmedStr, completedStr sql.NullString
		createdStr, expiresStr                           string
	)
	err := scanner.Scan(
		&op.ID,
		&op.ConnectionID,
		&opTypeStr,
		&op.Method,
		&op.ParamsJSON,
		&op.HumanSummary,
		&statusStr,
		&resultJSON,
		&errorMsg,
		&createdStr,
		&expiresStr,
		&confirmedStr,
		&completedStr,
	)
	if err != nil {
		return nil, err
	}
	op.OperationType = OperationType(opTypeStr)
	op.Status = OperationStatus(statusStr)
	op.ResultJSON = resultJSON.String
	if op.ParamsJSON != "" {
		_ = json.Unmarshal([]byte(op.ParamsJSON), &op.Params)
	}
	if op.Params == nil {
		op.Params = map[string]any{}
	}
	if resultJSON.Valid && resultJSON.String != "" {
		var parsedResult any
		if err := json.Unmarshal([]byte(resultJSON.String), &parsedResult); err == nil {
			op.Result = parsedResult
		}
	}
	op.ErrorMessage = errorMsg.String
	op.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	op.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expiresStr)
	if confirmedStr.Valid && confirmedStr.String != "" {
		t, _ := time.Parse(time.RFC3339Nano, confirmedStr.String)
		op.ConfirmedAt = &t
	}
	if completedStr.Valid && completedStr.String != "" {
		t, _ := time.Parse(time.RFC3339Nano, completedStr.String)
		op.CompletedAt = &t
	}
	return &op, nil
}

func (s *Store) RegisterOAuthClient(ctx context.Context, client McpOAuthClient) error {
	if err := s.cleanupExpiredOAuth(ctx); err != nil {
		return err
	}
	redirectsJSON, err := json.Marshal(client.RedirectURIs)
	if err != nil {
		return err
	}
	grantsJSON, err := json.Marshal(client.GrantTypes)
	if err != nil {
		return err
	}
	responsesJSON, err := json.Marshal(client.ResponseTypes)
	if err != nil {
		return err
	}
	createdAt := client.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mcp_oauth_clients (
			client_id, redirect_uris, token_endpoint_auth_method, grant_types, response_types,
			client_name, client_uri, logo_uri, scope, software_id, software_version, created_at
		) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12)
	`,
		client.ClientID,
		string(redirectsJSON),
		client.TokenEndpointAuthMethod,
		string(grantsJSON),
		string(responsesJSON),
		nullableString(client.ClientName),
		nullableString(client.ClientURI),
		nullableString(client.LogoURI),
		client.Scope,
		nullableString(client.SoftwareID),
		nullableString(client.SoftwareVersion),
		createdAt.UTC().Format(isoTimeFormat),
	)
	return err
}

func (s *Store) GetOAuthClient(ctx context.Context, clientID string) (*McpOAuthClient, error) {
	var (
		c                                                           McpOAuthClient
		redirectsRaw, grantsRaw, responsesRaw, createdStr           string
		clientName, clientURI, logoURI, softwareID, softwareVersion sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT client_id, redirect_uris, token_endpoint_auth_method, grant_types, response_types,
		       client_name, client_uri, logo_uri, scope, software_id, software_version, created_at
		FROM mcp_oauth_clients
		WHERE client_id = ?1
	`, clientID).Scan(
		&c.ClientID,
		&redirectsRaw,
		&c.TokenEndpointAuthMethod,
		&grantsRaw,
		&responsesRaw,
		&clientName,
		&clientURI,
		&logoURI,
		&c.Scope,
		&softwareID,
		&softwareVersion,
		&createdStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(redirectsRaw), &c.RedirectURIs)
	_ = json.Unmarshal([]byte(grantsRaw), &c.GrantTypes)
	_ = json.Unmarshal([]byte(responsesRaw), &c.ResponseTypes)
	c.ClientName = clientName.String
	c.ClientURI = clientURI.String
	c.LogoURI = logoURI.String
	c.SoftwareID = softwareID.String
	c.SoftwareVersion = softwareVersion.String
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return &c, nil
}

func (s *Store) CreateAuthorizationRequest(ctx context.Context, req McpOAuthAuthorizationRequest) error {
	if err := s.cleanupExpiredOAuth(ctx); err != nil {
		return err
	}
	now := time.Now().UTC().Format(isoTimeFormat)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mcp_oauth_authorization_requests (
			request_hash, client_id, redirect_uri, state, code_challenge, resource,
			scope, expires_at, attempts, created_at
		) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)
	`,
		req.RequestHash,
		req.ClientID,
		req.RedirectURI,
		nullableString(req.State),
		req.CodeChallenge,
		req.Resource,
		req.Scope,
		req.ExpiresAt.UTC().Format(isoTimeFormat),
		req.Attempts,
		now,
	)
	return err
}

func (s *Store) GetAuthorizationRequest(ctx context.Context, requestHash string) (*McpOAuthAuthorizationRequest, error) {
	now := time.Now().UTC().Format(isoTimeFormat)
	row := s.db.QueryRowContext(ctx, `
		SELECT request_hash, client_id, redirect_uri, state, code_challenge, resource,
		       scope, expires_at, attempts
		FROM mcp_oauth_authorization_requests
		WHERE request_hash = ?1 AND expires_at > ?2 AND attempts < 5
	`, requestHash, now)
	req, err := scanAuthorizationRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return req, err
}

func (s *Store) ConsumeAuthorizationRequestAndCreateCode(
	ctx context.Context,
	requestHash, codeHash string,
	expiresAt time.Time,
	connectionID string,
) (*McpOAuthAuthorizationRequest, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(isoTimeFormat)
	row := tx.QueryRowContext(ctx, `
		DELETE FROM mcp_oauth_authorization_requests
		WHERE request_hash = ?1 AND expires_at > ?2 AND attempts < 5
		RETURNING request_hash, client_id, redirect_uri, state, code_challenge, resource,
		          scope, expires_at, attempts
	`, requestHash, now)
	req, err := scanAuthorizationRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO mcp_oauth_authorization_codes (
			code_hash, client_id, connection_id, redirect_uri, code_challenge, resource, scope, expires_at, created_at
		) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9)
	`,
		codeHash,
		req.ClientID,
		connectionID,
		req.RedirectURI,
		req.CodeChallenge,
		req.Resource,
		req.Scope,
		expiresAt.UTC().Format(isoTimeFormat),
		now,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return req, nil
}

func (s *Store) ConsumeAuthorizationRequest(ctx context.Context, requestHash string) (*McpOAuthAuthorizationRequest, error) {
	now := time.Now().UTC().Format(isoTimeFormat)
	row := s.db.QueryRowContext(ctx, `
		DELETE FROM mcp_oauth_authorization_requests
		WHERE request_hash = ?1 AND expires_at > ?2
		RETURNING request_hash, client_id, redirect_uri, state, code_challenge, resource,
		          scope, expires_at, attempts
	`, requestHash, now)
	req, err := scanAuthorizationRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return req, err
}

func (s *Store) ConsumeAuthorizationCode(ctx context.Context, codeHash string) (*McpOAuthAuthorizationCode, error) {
	now := time.Now().UTC().Format(isoTimeFormat)
	var (
		c          McpOAuthAuthorizationCode
		expiresStr string
	)
	err := s.db.QueryRowContext(ctx, `
		DELETE FROM mcp_oauth_authorization_codes
		WHERE code_hash = ?1 AND expires_at > ?2
		RETURNING code_hash, client_id, connection_id, redirect_uri, code_challenge, resource, scope, expires_at
	`, codeHash, now).Scan(
		&c.CodeHash,
		&c.ClientID,
		&c.ConnectionID,
		&c.RedirectURI,
		&c.CodeChallenge,
		&c.Resource,
		&c.Scope,
		&expiresStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expiresStr)
	return &c, nil
}

type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertTokenRecord(ctx context.Context, exec sqlExecer, token NewMcpOAuthTokenRecord, now string) error {
	_, err := exec.ExecContext(ctx, `
		INSERT INTO mcp_oauth_tokens (
			id, access_token_hash, refresh_token_hash, client_id, connection_id, resource, scope,
			access_expires_at, refresh_expires_at, created_at
		) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)
	`,
		uuid.NewString(),
		token.AccessTokenHash,
		token.RefreshTokenHash,
		token.ClientID,
		token.ConnectionID,
		token.Resource,
		token.Scope,
		token.AccessExpiresAt.UTC().Format(isoTimeFormat),
		token.RefreshExpiresAt.UTC().Format(isoTimeFormat),
		now,
	)
	return err
}

func (s *Store) IssueTokenPair(ctx context.Context, token NewMcpOAuthTokenRecord) error {
	return insertTokenRecord(ctx, s.db, token, time.Now().UTC().Format(isoTimeFormat))
}

func (s *Store) RotateRefreshToken(
	ctx context.Context,
	refreshTokenHash, clientID, resource string,
	replacement NewMcpOAuthTokenRecord,
) (*McpOAuthTokenRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(isoTimeFormat)
	row := tx.QueryRowContext(ctx, `
		UPDATE mcp_oauth_tokens
		SET revoked_at = ?4
		WHERE refresh_token_hash = ?1
		  AND client_id = ?2
		  AND resource = ?3
		  AND revoked_at IS NULL
		  AND refresh_expires_at > ?4
		RETURNING id, access_token_hash, refresh_token_hash, client_id, connection_id,
		          resource, scope, access_expires_at, refresh_expires_at, revoked_at, created_at
	`, refreshTokenHash, clientID, resource, now)
	current, err := scanTokenRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if err := insertTokenRecord(ctx, tx, replacement, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return current, nil
}

func (s *Store) FindAccessToken(ctx context.Context, accessTokenHash string) (*McpOAuthTokenRecord, error) {
	now := time.Now().UTC().Format(isoTimeFormat)
	row := s.db.QueryRowContext(ctx, `
		SELECT id, access_token_hash, refresh_token_hash, client_id, connection_id,
		       resource, scope, access_expires_at, refresh_expires_at, revoked_at, created_at
		FROM mcp_oauth_tokens
		WHERE access_token_hash = ?1
		  AND revoked_at IS NULL
		  AND access_expires_at > ?2
	`, accessTokenHash, now)
	tok, err := scanTokenRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return tok, err
}

func (s *Store) FindRefreshToken(ctx context.Context, refreshTokenHash, clientID, resource string) (*McpOAuthTokenRecord, error) {
	now := time.Now().UTC().Format(isoTimeFormat)
	row := s.db.QueryRowContext(ctx, `
		SELECT id, access_token_hash, refresh_token_hash, client_id, connection_id,
		       resource, scope, access_expires_at, refresh_expires_at, revoked_at, created_at
		FROM mcp_oauth_tokens
		WHERE refresh_token_hash = ?1
		  AND client_id = ?2
		  AND resource = ?3
		  AND revoked_at IS NULL
		  AND refresh_expires_at > ?4
	`, refreshTokenHash, clientID, resource, now)
	tok, err := scanTokenRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return tok, err
}

func (s *Store) RevokeToken(ctx context.Context, tokenHash string) error {
	now := time.Now().UTC().Format(isoTimeFormat)
	_, err := s.db.ExecContext(ctx, `
		UPDATE mcp_oauth_tokens
		SET revoked_at = COALESCE(revoked_at, ?2)
		WHERE access_token_hash = ?1 OR refresh_token_hash = ?1
	`, tokenHash, now)
	return err
}

func (s *Store) cleanupExpiredOAuth(ctx context.Context) error {
	now := time.Now().UTC()
	nowStr := now.Format(isoTimeFormat)
	sevenDaysAgo := now.Add(-7 * 24 * time.Hour).Format(isoTimeFormat)
	thirtyDaysAgo := now.Add(-30 * 24 * time.Hour).Format(isoTimeFormat)

	if _, err := s.db.ExecContext(ctx, "DELETE FROM mcp_oauth_authorization_requests WHERE expires_at <= ?1", nowStr); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM mcp_oauth_authorization_codes WHERE expires_at <= ?1", nowStr); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM mcp_oauth_tokens
		WHERE refresh_expires_at <= ?1 OR (revoked_at IS NOT NULL AND revoked_at < ?2)
	`, nowStr, sevenDaysAgo); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM mcp_oauth_clients
		WHERE created_at < ?1
		  AND NOT EXISTS (SELECT 1 FROM mcp_oauth_tokens WHERE mcp_oauth_tokens.client_id = mcp_oauth_clients.client_id)
		  AND NOT EXISTS (SELECT 1 FROM mcp_oauth_authorization_codes WHERE mcp_oauth_authorization_codes.client_id = mcp_oauth_clients.client_id)
		  AND NOT EXISTS (SELECT 1 FROM mcp_oauth_authorization_requests WHERE mcp_oauth_authorization_requests.client_id = mcp_oauth_clients.client_id)
	`, thirtyDaysAgo)
	return err
}

func scanAuthorizationRequest(scanner rowScanner) (*McpOAuthAuthorizationRequest, error) {
	var (
		r          McpOAuthAuthorizationRequest
		state      sql.NullString
		expiresStr string
	)
	err := scanner.Scan(
		&r.RequestHash,
		&r.ClientID,
		&r.RedirectURI,
		&state,
		&r.CodeChallenge,
		&r.Resource,
		&r.Scope,
		&expiresStr,
		&r.Attempts,
	)
	if err != nil {
		return nil, err
	}
	r.State = state.String
	r.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expiresStr)
	return &r, nil
}

func scanTokenRecord(scanner rowScanner) (*McpOAuthTokenRecord, error) {
	var (
		t                                         McpOAuthTokenRecord
		accessExpStr, refreshExpStr, createdAtStr string
		revokedAt                                 sql.NullString
	)
	err := scanner.Scan(
		&t.ID,
		&t.AccessTokenHash,
		&t.RefreshTokenHash,
		&t.ClientID,
		&t.ConnectionID,
		&t.Resource,
		&t.Scope,
		&accessExpStr,
		&refreshExpStr,
		&revokedAt,
		&createdAtStr,
	)
	if err != nil {
		return nil, err
	}
	t.AccessExpiresAt, _ = time.Parse(time.RFC3339Nano, accessExpStr)
	t.RefreshExpiresAt, _ = time.Parse(time.RFC3339Nano, refreshExpStr)
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAtStr)
	if revokedAt.Valid && revokedAt.String != "" {
		parsed, _ := time.Parse(time.RFC3339Nano, revokedAt.String)
		t.RevokedAt = &parsed
	}
	return &t, nil
}

func nullableString(val string) any {
	if val == "" {
		return nil
	}
	return val
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanConnection(scanner rowScanner) (*Connection, error) {
	var (
		c                            Connection
		encAccess, encRefresh, modeS string
		statusS                      string
		expiresStr, createdStr       string
		updatedStr                   string
		adminInt                     int
	)
	err := scanner.Scan(
		&c.ID,
		&c.Domain,
		&c.MemberID,
		&c.BitrixUserID,
		&c.ClientEndpoint,
		&encAccess,
		&encRefresh,
		&c.Scope,
		&expiresStr,
		&modeS,
		&adminInt,
		&statusS,
		&createdStr,
		&updatedStr,
	)
	if err != nil {
		return nil, err
	}
	var access string
	if encAccess != "" {
		access, err = s.cipher.Decrypt(encAccess)
		if err != nil {
			return nil, err
		}
	}
	var refresh string
	if encRefresh != "" {
		refresh, err = s.cipher.Decrypt(encRefresh)
		if err != nil {
			return nil, err
		}
	}
	c.AccessToken = access
	c.RefreshToken = refresh
	c.Mode = config.ConnectionMode(modeS)
	c.Status = ConnectionStatus(statusS)
	c.AdminToolsEnabled = adminInt != 0
	c.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expiresStr)
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
	return &c, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}
