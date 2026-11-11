package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/google/uuid"
)

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
