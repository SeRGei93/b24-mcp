package bitrix

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
)

var hostedBitrix24Pattern = regexp.MustCompile(`(?i)^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+bitrix24\.(?:com|ru|de|eu|ua|by|kz|pl|fr|es|it|br|in|cn|vn|tr|jp|uk|mx|co|id|uz|az|am|ge|kg|lv|lt|ee|com\.br|co\.uk|com\.tr|com\.mx|com\.ua)$`)

type PortalEndpoint struct {
	Domain   string
	Endpoint string
}

type OAuthTokenResponse struct {
	AccessToken    string     `json:"access_token"`
	RefreshToken   string     `json:"refresh_token"`
	ExpiresIn      FlexInt    `json:"expires_in"`
	Scope          FlexScope  `json:"scope"`
	Domain         string     `json:"domain"`
	MemberID       string     `json:"member_id"`
	UserID         FlexString `json:"user_id"`
	ClientEndpoint string     `json:"client_endpoint"`
	Error          string     `json:"error"`
}

type CallbackInput struct {
	Code     string
	State    string
	Domain   string
	MemberID string
	Error    string
}

type CallbackResult struct {
	Connection     *store.Connection
	McpRequestHash string
}

type ContinuationError struct {
	Message        string
	McpRequestHash string
	OAuthErrorCode string
	Cause          error
}

func (e *ContinuationError) Error() string {
	return e.Message
}

func (e *ContinuationError) Unwrap() error {
	return e.Cause
}

func parsePortalHost(input string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(input))
	if trimmed == "" {
		return "", config.NewAppError("Invalid Bitrix24 portal domain", "INVALID_PORTAL_DOMAIN", 400)
	}
	rawURL := trimmed
	if !strings.Contains(trimmed, "://") {
		rawURL = "https://" + trimmed
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return "", config.NewAppError("Invalid Bitrix24 portal domain", "INVALID_PORTAL_DOMAIN", 400)
	}
	if parsed.Scheme != "https" ||
		parsed.User != nil ||
		parsed.Port() != "" ||
		(parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return "", config.NewAppError("Portal must be an HTTPS Bitrix24 domain without a path, port, query, or credentials", "INVALID_PORTAL_DOMAIN", 400)
	}
	host := strings.TrimSuffix(parsed.Hostname(), ".")
	if net.ParseIP(host) != nil || host == "localhost" {
		return "", config.NewAppError("IP addresses and localhost are not valid Bitrix24 portal domains", "INVALID_PORTAL_DOMAIN", 400)
	}
	return host, nil
}

func NormalizePortalDomain(input string, allowedPortalDomains []string) (string, error) {
	host, err := parsePortalHost(input)
	if err != nil {
		return "", err
	}
	if len(allowedPortalDomains) > 0 {
		for _, allowed := range allowedPortalDomains {
			if allowedHost, err := parsePortalHost(allowed); err == nil && allowedHost == host {
				return host, nil
			}
		}
		return "", config.NewAppError("Only hosted or explicitly configured Bitrix24 portal domains are allowed", "INVALID_PORTAL_DOMAIN", 400)
	}
	if !hostedBitrix24Pattern.MatchString(host) {
		return "", config.NewAppError("Only hosted or explicitly configured Bitrix24 portal domains are allowed", "INVALID_PORTAL_DOMAIN", 400)
	}
	return host, nil
}

func ValidateClientEndpoint(value, expectedDomain string, allowedPortalDomains []string) (PortalEndpoint, error) {
	authErr := config.NewAuthenticationError("Bitrix24 returned a REST endpoint that does not match the authorized portal")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return PortalEndpoint{}, authErr
	}
	domain, err := NormalizePortalDomain(parsed.Hostname(), allowedPortalDomains)
	if err != nil {
		return PortalEndpoint{}, authErr
	}
	var expected string
	if expectedDomain != "" {
		expected, err = NormalizePortalDomain(expectedDomain, allowedPortalDomains)
		if err != nil {
			return PortalEndpoint{}, authErr
		}
	}
	if parsed.Scheme == "https" &&
		parsed.User == nil &&
		parsed.Port() == "" &&
		strings.HasPrefix(parsed.Path, "/rest/") &&
		parsed.RawQuery == "" &&
		parsed.Fragment == "" &&
		(expected == "" || domain == expected) {
		endpoint := strings.TrimRight(parsed.String(), "/") + "/"
		return PortalEndpoint{Domain: domain, Endpoint: endpoint}, nil
	}
	return PortalEndpoint{}, authErr
}

type OAuthService struct {
	cfg        *config.Config
	store      *store.Store
	httpClient *http.Client
}

func NewOAuthService(cfg *config.Config, st *store.Store, httpClient *http.Client) *OAuthService {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: time.Duration(cfg.BitrixRequestTimeoutMs) * time.Millisecond}
	}
	return &OAuthService{cfg: cfg, store: st, httpClient: httpClient}
}

func (s *OAuthService) allowedPortalDomains() []string {
	if s.cfg.BitrixPortalDomain != "" {
		return []string{s.cfg.BitrixPortalDomain}
	}
	return nil
}

func (s *OAuthService) CreateAuthorizationURL(
	ctx context.Context,
	domainInput string,
	requestedMode config.ConnectionMode,
	mcpRequestHash string,
) (string, error) {
	domain, err := NormalizePortalDomain(domainInput, s.allowedPortalDomains())
	if err != nil {
		return "", err
	}
	mode := requestedMode
	if mode == "" {
		mode = s.cfg.DefaultMode
	}
	rawStateBytes := make([]byte, 32)
	if _, err := rand.Read(rawStateBytes); err != nil {
		return "", err
	}
	rawState := base64.RawURLEncoding.EncodeToString(rawStateBytes)
	expiresAt := time.Now().Add(time.Duration(s.cfg.OAuthStateTTLSeconds) * time.Second)
	if err := s.store.CreateOAuthState(ctx, rawState, domain, mode, expiresAt, mcpRequestHash); err != nil {
		return "", err
	}
	u, _ := url.Parse(fmt.Sprintf("https://%s/oauth/authorize/", domain))
	q := u.Query()
	q.Set("client_id", s.cfg.BitrixClientID)
	q.Set("state", rawState)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *OAuthService) HandleCallbackWithContext(ctx context.Context, input CallbackInput) (*CallbackResult, error) {
	if input.State == "" {
		return nil, config.NewAuthenticationError("OAuth callback is missing state")
	}
	state, err := s.store.ConsumeOAuthState(ctx, input.State)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, config.NewAuthenticationError("OAuth state is invalid, expired, or already used")
	}

	result, err := s.completeCallback(ctx, input, state)
	if err != nil {
		if state.McpRequestHash != "" {
			errCode := "server_error"
			msg := config.SafeErrorMessage(err)
			if input.Error != "" {
				errCode = "access_denied"
				msg = "Bitrix24 authorization was denied"
			}
			return nil, &ContinuationError{
				Message:        msg,
				McpRequestHash: state.McpRequestHash,
				OAuthErrorCode: errCode,
				Cause:          err,
			}
		}
		return nil, err
	}
	return result, nil
}

func (s *OAuthService) completeCallback(ctx context.Context, input CallbackInput, state *store.OAuthStateRecord) (*CallbackResult, error) {
	if input.Error != "" {
		return nil, config.NewAuthenticationError("Bitrix24 authorization was denied")
	}
	if input.Code == "" {
		return nil, config.NewAuthenticationError("OAuth callback is missing the authorization code")
	}
	if input.Domain != "" {
		normDomain, err := NormalizePortalDomain(input.Domain, s.allowedPortalDomains())
		if err != nil || normDomain != state.Domain {
			return nil, config.NewAuthenticationError("OAuth callback portal does not match the portal that initiated authorization")
		}
	}

	token, err := s.exchangeToken(ctx, map[string]string{
		"grant_type": "authorization_code",
		"code":       input.Code,
	})
	if err != nil {
		return nil, err
	}
	if input.MemberID != "" && input.MemberID != token.MemberID {
		return nil, config.NewAuthenticationError("OAuth token portal identifier does not match the callback portal identifier")
	}
	portal, err := ValidateClientEndpoint(token.ClientEndpoint, state.Domain, s.allowedPortalDomains())
	if err != nil {
		return nil, err
	}
	bitrixUserID, err := s.getProfileUserID(ctx, token.AccessToken, portal.Endpoint)
	if err != nil {
		return nil, err
	}
	if token.UserID != "" && string(token.UserID) != bitrixUserID {
		return nil, config.NewAuthenticationError("Bitrix24 token profile does not match the token response user")
	}

	expiresIn := int(token.ExpiresIn)
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	conn, err := s.store.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:            portal.Domain,
		MemberID:          token.MemberID,
		BitrixUserID:      bitrixUserID,
		ClientEndpoint:    portal.Endpoint,
		AccessToken:       token.AccessToken,
		RefreshToken:      token.RefreshToken,
		Scope:             string(token.Scope),
		ExpiresAt:         time.Now().Add(time.Duration(expiresIn) * time.Second),
		Mode:              state.Mode,
		AdminToolsEnabled: s.cfg.AdminToolsEnabled,
	})
	if err != nil {
		return nil, err
	}
	return &CallbackResult{Connection: conn, McpRequestHash: state.McpRequestHash}, nil
}

func (s *OAuthService) Refresh(ctx context.Context, refreshToken string) (*OAuthTokenResponse, error) {
	return s.exchangeToken(ctx, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
}

func (s *OAuthService) HandleInstallation(ctx context.Context, input map[string]any) (*store.Connection, error) {
	normalized := s.normalizeInstallationPayload(input)
	rawBytes, err := json.Marshal(normalized)
	if err != nil {
		return nil, config.NewAppError("Invalid installation payload", "INVALID_REQUEST", 400)
	}
	var auth OAuthTokenResponse
	if err := json.Unmarshal(rawBytes, &auth); err != nil {
		return nil, config.NewAppError("Invalid installation payload", "INVALID_REQUEST", 400)
	}
	if auth.AccessToken == "" || auth.RefreshToken == "" || auth.Domain == "" || auth.MemberID == "" || auth.ClientEndpoint == "" {
		return nil, config.NewAppError("Missing required installation fields", "INVALID_REQUEST", 400)
	}
	if auth.ExpiresIn <= 0 {
		auth.ExpiresIn = 3600
	}

	installAllowed, err := s.resolveInstallationAllowedDomains(ctx)
	if err != nil {
		return nil, err
	}
	expectedDomain, err := NormalizePortalDomain(auth.Domain, installAllowed)
	if err != nil {
		return nil, err
	}
	portal, err := ValidateClientEndpoint(auth.ClientEndpoint, expectedDomain, installAllowed)
	if err != nil {
		return nil, err
	}

	appCode, err := s.getAppInfoCode(ctx, auth.AccessToken, portal.Endpoint)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(s.cfg.BitrixClientID, "pending-") && appCode != s.cfg.BitrixClientID {
		return nil, config.NewAuthenticationError("Installation token belongs to a different Bitrix24 application")
	}

	bitrixUserID, err := s.getProfileUserID(ctx, auth.AccessToken, portal.Endpoint)
	if err != nil {
		return nil, err
	}
	if auth.UserID != "" && string(auth.UserID) != bitrixUserID {
		return nil, config.NewAuthenticationError("Installation token profile does not match the supplied user")
	}

	return s.store.UpsertConnection(ctx, store.UpsertConnectionInput{
		Domain:            portal.Domain,
		MemberID:          auth.MemberID,
		BitrixUserID:      bitrixUserID,
		ClientEndpoint:    portal.Endpoint,
		AccessToken:       auth.AccessToken,
		RefreshToken:      auth.RefreshToken,
		Scope:             string(auth.Scope),
		ExpiresAt:         time.Now().Add(time.Duration(auth.ExpiresIn) * time.Second),
		Mode:              s.cfg.DefaultMode,
		AdminToolsEnabled: s.cfg.AdminToolsEnabled,
	})
}

func (s *OAuthService) normalizeInstallationPayload(root map[string]any) map[string]any {
	if root == nil {
		root = map[string]any{}
	}
	nestedAuth, _ := root["auth"].(map[string]any)
	queryMap, _ := root["query"].(map[string]any)

	pick := func(keys ...string) any {
		for _, k := range keys {
			for _, m := range []map[string]any{nestedAuth, root, queryMap} {
				if m == nil {
					continue
				}
				if v, ok := m[k]; ok && v != nil && v != "" {
					return v
				}
			}
		}
		return nil
	}

	rawDomain := pick("domain", "DOMAIN")
	if rawDomain == nil && s.cfg.BitrixPortalDomain != "" {
		rawDomain = s.cfg.BitrixPortalDomain
	}
	var normalizedDomain string
	if ds, ok := rawDomain.(string); ok && strings.TrimSpace(ds) != "" {
		if parsedHost, err := parsePortalHost(ds); err == nil {
			normalizedDomain = parsedHost
		} else {
			normalizedDomain = ds
		}
	}

	clientEndpoint := pick("client_endpoint", "CLIENT_ENDPOINT")
	if clientEndpoint == nil && normalizedDomain != "" {
		clientEndpoint = fmt.Sprintf("https://%s/rest/", normalizedDomain)
	}
	expiresIn := pick("expires_in", "AUTH_EXPIRES", "expires")
	if expiresIn == nil {
		expiresIn = 3600
	}
	scope := pick("scope", "SCOPE", "APPLICATION_SCOPE", "application_scope")
	if scope == nil {
		scope = ""
	}
	memberID := pick("member_id", "MEMBER_ID")
	if memberID == nil && normalizedDomain != "" {
		memberID = normalizedDomain
	}

	return map[string]any{
		"access_token":    pick("access_token", "AUTH_ID"),
		"refresh_token":   pick("refresh_token", "REFRESH_ID"),
		"expires_in":      expiresIn,
		"scope":           scope,
		"domain":          normalizedDomain,
		"member_id":       memberID,
		"user_id":         pick("user_id", "USER_ID"),
		"client_endpoint": clientEndpoint,
	}
}

func (s *OAuthService) resolveInstallationAllowedDomains(ctx context.Context) ([]string, error) {
	if allowed := s.allowedPortalDomains(); len(allowed) > 0 {
		return allowed, nil
	}
	conns, err := s.store.ListConnections(ctx)
	if err != nil {
		return nil, err
	}
	var active []string
	for _, c := range conns {
		if c.Status == "active" && !slices.Contains(active, c.Domain) {
			active = append(active, c.Domain)
		}
	}
	return active, nil
}

func (s *OAuthService) getAppInfoCode(ctx context.Context, accessToken, clientEndpoint string) (string, error) {
	var body struct {
		Result struct {
			Code string `json:"CODE"`
		} `json:"result"`
	}
	if err := s.postTokenCheck(ctx, clientEndpoint+"app.info.json", accessToken, &body, "Could not validate the Bitrix24 installation callback", "Bitrix24 installation token validation failed"); err != nil {
		return "", err
	}
	if body.Result.Code == "" {
		return "", config.NewAuthenticationError("Bitrix24 installation token validation failed")
	}
	return body.Result.Code, nil
}

func (s *OAuthService) getProfileUserID(ctx context.Context, accessToken, clientEndpoint string) (string, error) {
	var body struct {
		Result struct {
			ID FlexString `json:"ID"`
		} `json:"result"`
	}
	if err := s.postTokenCheck(ctx, clientEndpoint+"profile.json", accessToken, &body, "Could not verify the authorized Bitrix24 profile", "Bitrix24 profile verification failed"); err != nil {
		return "", err
	}
	if body.Result.ID == "" {
		return "", config.NewAuthenticationError("Bitrix24 profile verification failed")
	}
	return string(body.Result.ID), nil
}

func (s *OAuthService) postTokenCheck(ctx context.Context, targetURL, accessToken string, dest any, networkErrMsg, invalidErrMsg string) error {
	payload, _ := json.Marshal(map[string]string{"auth": accessToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return config.NewAuthenticationError(networkErrMsg)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return config.NewAuthenticationError(networkErrMsg)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return config.NewAuthenticationError(invalidErrMsg)
	}
	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return config.NewAuthenticationError(invalidErrMsg)
	}
	return nil
}

func (s *OAuthService) exchangeToken(ctx context.Context, params map[string]string) (*OAuthTokenResponse, error) {
	u, _ := url.Parse("https://oauth.bitrix.info/oauth/token/")
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	q.Set("client_id", s.cfg.BitrixClientID)
	q.Set("client_secret", s.cfg.BitrixClientSecret)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, newBitrixOAuthError("Could not reach the Bitrix24 OAuth service", "network_error")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, newBitrixOAuthError("Could not reach the Bitrix24 OAuth service", "network_error")
	}
	defer resp.Body.Close()

	var parsed OAuthTokenResponse
	decodeErr := json.NewDecoder(resp.Body).Decode(&parsed)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || parsed.Error != "" {
		code := parsed.Error
		if code == "" {
			code = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return nil, newBitrixOAuthError(fmt.Sprintf("Bitrix24 OAuth request failed (%s)", code), code)
	}
	if decodeErr != nil || parsed.AccessToken == "" || parsed.RefreshToken == "" || parsed.MemberID == "" || parsed.ClientEndpoint == "" {
		return nil, newBitrixOAuthError("Bitrix24 returned an invalid OAuth token response", "invalid_response")
	}
	if parsed.ExpiresIn <= 0 {
		parsed.ExpiresIn = 3600
	}
	return &parsed, nil
}

func newBitrixOAuthError(message, oauthCode string) *config.AppError {
	return &config.AppError{
		Message:    message,
		Code:       "BITRIX_OAUTH_ERROR",
		StatusCode: 502,
		OAuthCode:  oauthCode,
	}
}

type ConnectionWithToken struct {
	Connection  *store.Connection
	AccessToken string
}

type inflightRefresh struct {
	done chan struct{}
	res  *ConnectionWithToken
	err  error
}

type TokenManager struct {
	store    *store.Store
	oauth    *OAuthService
	logger   *config.Logger
	mu       sync.Mutex
	inflight map[string]*inflightRefresh
}

func NewTokenManager(st *store.Store, oauth *OAuthService, logger *config.Logger) *TokenManager {
	return &TokenManager{
		store:    st,
		oauth:    oauth,
		logger:   logger,
		inflight: make(map[string]*inflightRefresh),
	}
}

func isTokenReusable(conn *store.Connection, forceRefresh bool, staleAccessToken string) bool {
	if forceRefresh && staleAccessToken != "" && conn.AccessToken != staleAccessToken {
		return true
	}
	return !forceRefresh && time.Until(conn.ExpiresAt) > time.Minute
}

func (tm *TokenManager) Get(ctx context.Context, connectionID string, forceRefresh bool, staleAccessToken string) (*ConnectionWithToken, error) {
	conn, err := tm.store.GetConnection(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	if conn.Status != "active" {
		return nil, config.NewPolicyError(fmt.Sprintf("Connection '%s' is %s", connectionID, conn.Status))
	}
	if isTokenReusable(conn, forceRefresh, staleAccessToken) {
		return &ConnectionWithToken{Connection: conn, AccessToken: conn.AccessToken}, nil
	}

	tm.mu.Lock()
	if existing, ok := tm.inflight[connectionID]; ok {
		tm.mu.Unlock()
		select {
		case <-existing.done:
			return existing.res, existing.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &inflightRefresh{done: make(chan struct{})}
	tm.inflight[connectionID] = call
	tm.mu.Unlock()

	res, err := tm.refreshLocked(context.WithoutCancel(ctx), connectionID, forceRefresh, staleAccessToken)
	call.res = res
	call.err = err
	close(call.done)

	tm.mu.Lock()
	delete(tm.inflight, connectionID)
	tm.mu.Unlock()

	return res, err
}

func (tm *TokenManager) refreshLocked(ctx context.Context, connectionID string, forceRefresh bool, staleAccessToken string) (*ConnectionWithToken, error) {
	current, err := tm.store.GetConnection(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	if isTokenReusable(current, forceRefresh, staleAccessToken) {
		return &ConnectionWithToken{Connection: current, AccessToken: current.AccessToken}, nil
	}

	response, err := tm.oauth.Refresh(ctx, current.RefreshToken)
	if err != nil {
		var appErr *config.AppError
		if errors.As(err, &appErr) && slices.Contains([]string{"invalid_grant", "invalid_token", "expired_token"}, appErr.OAuthCode) {
			if revokeErr := tm.store.MarkConnectionRevoked(ctx, connectionID); revokeErr != nil && tm.logger != nil {
				tm.logger.Error("Failed to mark Bitrix24 connection revoked", map[string]any{
					"connectionId": connectionID,
					"error":        revokeErr.Error(),
				})
			} else if tm.logger != nil {
				tm.logger.Warn("Bitrix24 connection marked revoked", map[string]any{
					"connectionId": connectionID,
					"oauthCode":    appErr.OAuthCode,
				})
			}
		}
		return nil, err
	}

	if response.MemberID != current.MemberID {
		return nil, newBitrixOAuthError("Refreshed token belongs to a different Bitrix24 portal", "portal_mismatch")
	}
	portal, err := ValidateClientEndpoint(response.ClientEndpoint, "", []string{current.Domain})
	if err != nil {
		return nil, err
	}
	userID := string(response.UserID)
	if userID == "" {
		userID = current.BitrixUserID
	}
	updated, err := tm.store.UpdateConnectionTokens(ctx, connectionID, store.UpdateTokensInput{
		AccessToken:    response.AccessToken,
		RefreshToken:   response.RefreshToken,
		ExpiresAt:      time.Now().Add(time.Duration(response.ExpiresIn) * time.Second),
		Scope:          string(response.Scope),
		Domain:         portal.Domain,
		ClientEndpoint: portal.Endpoint,
		BitrixUserID:   userID,
	})
	if err != nil {
		return nil, err
	}
	if tm.logger != nil {
		tm.logger.Info("Bitrix24 access token refreshed", map[string]any{"connectionId": connectionID})
	}
	return &ConnectionWithToken{Connection: updated, AccessToken: response.AccessToken}, nil
}
