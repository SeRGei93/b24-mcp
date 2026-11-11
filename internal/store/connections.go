package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/google/uuid"
)

type ConnectionStatus string

const (
	StatusActive   ConnectionStatus = "active"
	StatusDisabled ConnectionStatus = "disabled"
	StatusRevoked  ConnectionStatus = "revoked"

	WebhookMemberIDPrefix = "webhook:"
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
	if strings.HasPrefix(c.MemberID, WebhookMemberIDPrefix) {
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
	domain, userID, code, err := config.ParseWebhookURL(cfg.BitrixWebhookURL, cfg.BitrixPortalDomain)
	if err != nil {
		return nil, err
	}

	scheme := "https"
	if strings.HasPrefix(strings.ToLower(cfg.BitrixWebhookURL), "http://") {
		scheme = "http"
	}
	clientEndpoint := fmt.Sprintf("%s://%s/rest/%s/%s/", scheme, domain, userID, code)

	return s.UpsertConnection(ctx, UpsertConnectionInput{
		Domain:            domain,
		MemberID:          WebhookMemberIDPrefix + domain,
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
