package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
)

const McpOAuthScope = "mcp:tools"

var (
	pkceVerifierPattern  = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
	pkceChallengePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

type ProtocolError struct {
	ErrorCode       string
	Message         string
	StatusCode      int
	SafeRedirectURI string
	State           string
}

func (e *ProtocolError) Error() string {
	return e.Message
}

func NewProtocolError(errorCode, message string, statusCode int) *ProtocolError {
	if statusCode == 0 {
		statusCode = http.StatusBadRequest
	}
	return &ProtocolError{
		ErrorCode:  errorCode,
		Message:    message,
		StatusCode: statusCode,
	}
}

type ClientRegistrationInput struct {
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
}

type ClientRegistrationResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
	Scope                   string   `json:"scope"`
	SoftwareID              string   `json:"software_id,omitempty"`
	SoftwareVersion         string   `json:"software_version,omitempty"`
}

type AuthorizationInput struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	State               string
	Scope               string
	Resource            string
}

type PendingAuthorizationInfo struct {
	RequestToken string
	RequestHash  string
	ClientID     string
	ClientName   string
	RedirectURI  string
	Scope        string
}

type AuthorizationCodeExchangeInput struct {
	ClientID     string
	Code         string
	CodeVerifier string
	RedirectURI  string
	Resource     string
}

type RefreshTokenExchangeInput struct {
	ClientID     string
	RefreshToken string
	Scope        string
	Resource     string
}

type OAuthTokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

type AccessInfo struct {
	ClientID          string
	ConnectionID      string
	Scopes            []string
	Resource          string
	ExpiresAt         int64
	LegacyStaticToken bool
}

func normPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func RedirectURIMatches(requested, registered string) bool {
	if requested == registered {
		return true
	}
	reqURL, err1 := url.Parse(requested)
	regURL, err2 := url.Parse(registered)
	if err1 != nil || err2 != nil || reqURL.Scheme == "" || regURL.Scheme == "" || reqURL.Host == "" || regURL.Host == "" {
		return false
	}
	if !config.IsLoopbackHost(reqURL.Hostname()) || !config.IsLoopbackHost(regURL.Hostname()) {
		return false
	}
	return reqURL.Scheme == regURL.Scheme &&
		reqURL.Hostname() == regURL.Hostname() &&
		normPath(reqURL.Path) == normPath(regURL.Path) &&
		reqURL.RawQuery == regURL.RawQuery
}

func IsSafeOAuthRedirectURI(value string) bool {
	if strings.Contains(value, "#") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Hostname() == "" || u.Fragment != "" || u.User != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && config.IsLoopbackHost(u.Hostname())
}

type OAuthService struct {
	cfg                          *config.Config
	store                        *store.Store
	IssuerURL                    string
	ResourceURL                  string
	ProtectedResourceMetadataURL string
}

func NewOAuthService(cfg *config.Config, st *store.Store) *OAuthService {
	issuer := strings.TrimRight(cfg.AppURL, "/")
	if issuer == "" {
		host := "localhost"
		if cfg != nil && cfg.Host == "127.0.0.1" {
			host = "127.0.0.1"
		}
		port := 3000
		if cfg != nil && cfg.Port > 0 {
			port = cfg.Port
		}
		issuer = fmt.Sprintf("http://%s:%d", host, port)
	}
	return &OAuthService{
		cfg:                          cfg,
		store:                        st,
		IssuerURL:                    issuer,
		ResourceURL:                  issuer + "/mcp",
		ProtectedResourceMetadataURL: issuer + "/.well-known/oauth-protected-resource/mcp",
	}
}

func (s *OAuthService) ProtectedResourceMetadata() map[string]any {
	return map[string]any{
		"resource":                 s.ResourceURL,
		"authorization_servers":    []string{s.IssuerURL},
		"scopes_supported":         []string{McpOAuthScope},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Bitrix24 MCP Server",
		"resource_documentation":   s.IssuerURL + "/",
	}
}

func (s *OAuthService) AuthorizationServerMetadata() map[string]any {
	return map[string]any{
		"issuer":                                     s.IssuerURL,
		"authorization_endpoint":                     s.IssuerURL + "/authorize",
		"token_endpoint":                             s.IssuerURL + "/token",
		"registration_endpoint":                      s.IssuerURL + "/register",
		"revocation_endpoint":                        s.IssuerURL + "/revoke",
		"response_types_supported":                   []string{"code"},
		"grant_types_supported":                      []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":      []string{"none"},
		"revocation_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":           []string{"S256"},
		"scopes_supported":                           []string{McpOAuthScope},
		"service_documentation":                      s.IssuerURL + "/",
	}
}

func (s *OAuthService) Challenge(errCode string) string {
	parts := []string{
		fmt.Sprintf(`Bearer resource_metadata="%s"`, s.ProtectedResourceMetadataURL),
		fmt.Sprintf(`scope="%s"`, McpOAuthScope),
	}
	if errCode != "" {
		parts = append(parts, fmt.Sprintf(`error="%s"`, errCode))
	}
	return strings.Join(parts, ", ")
}

func (s *OAuthService) RegisterClient(ctx context.Context, input ClientRegistrationInput) (*ClientRegistrationResponse, error) {
	if len(input.RedirectURIs) == 0 || len(input.RedirectURIs) > 10 {
		return nil, NewProtocolError("invalid_client_metadata", "Provide between one and ten redirect URIs", http.StatusBadRequest)
	}
	for _, uri := range input.RedirectURIs {
		if !IsSafeOAuthRedirectURI(uri) {
			return nil, NewProtocolError("invalid_redirect_uri", "Redirect URIs must use HTTPS, except for HTTP loopback callbacks", http.StatusBadRequest)
		}
	}
	tokenMethod := input.TokenEndpointAuthMethod
	if tokenMethod == "" {
		tokenMethod = "none"
	}
	if tokenMethod != "none" {
		return nil, NewProtocolError("invalid_client_metadata", "Only public PKCE clients are supported", http.StatusBadRequest)
	}
	grantTypes := input.GrantTypes
	if grantTypes == nil {
		grantTypes = []string{"authorization_code", "refresh_token"}
	}
	if !slices.Contains(grantTypes, "authorization_code") {
		return nil, NewProtocolError("invalid_client_metadata", "Unsupported OAuth grant type", http.StatusBadRequest)
	}
	for _, g := range grantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			return nil, NewProtocolError("invalid_client_metadata", "Unsupported OAuth grant type", http.StatusBadRequest)
		}
	}
	responseTypes := input.ResponseTypes
	if responseTypes == nil {
		responseTypes = []string{"code"}
	}
	if !slices.Contains(responseTypes, "code") {
		return nil, NewProtocolError("invalid_client_metadata", "Unsupported OAuth response type", http.StatusBadRequest)
	}
	for _, rt := range responseTypes {
		if rt != "code" {
			return nil, NewProtocolError("invalid_client_metadata", "Unsupported OAuth response type", http.StatusBadRequest)
		}
	}
	if input.Scope != "" && !slices.Contains(parseScope(input.Scope), McpOAuthScope) {
		return nil, NewProtocolError("invalid_client_metadata", fmt.Sprintf("Client scope must include %s", McpOAuthScope), http.StatusBadRequest)
	}

	var uniqueRedirects []string
	for _, u := range input.RedirectURIs {
		if !slices.Contains(uniqueRedirects, u) {
			uniqueRedirects = append(uniqueRedirects, u)
		}
	}

	createdAt := time.Now().UTC()
	client := store.McpOAuthClient{
		ClientID:                "mcp_" + randomToken(24),
		RedirectURIs:            uniqueRedirects,
		TokenEndpointAuthMethod: "none",
		GrantTypes:              grantTypes,
		ResponseTypes:           responseTypes,
		ClientName:              truncateStr(input.ClientName, 200),
		ClientURI:               input.ClientURI,
		LogoURI:                 input.LogoURI,
		Scope:                   McpOAuthScope,
		SoftwareID:              truncateStr(input.SoftwareID, 200),
		SoftwareVersion:         truncateStr(input.SoftwareVersion, 100),
		CreatedAt:               createdAt,
	}
	if err := s.store.RegisterOAuthClient(ctx, client); err != nil {
		return nil, err
	}
	return &ClientRegistrationResponse{
		ClientID:                client.ClientID,
		ClientIDIssuedAt:        client.CreatedAt.Unix(),
		RedirectURIs:            client.RedirectURIs,
		TokenEndpointAuthMethod: client.TokenEndpointAuthMethod,
		GrantTypes:              client.GrantTypes,
		ResponseTypes:           client.ResponseTypes,
		ClientName:              client.ClientName,
		ClientURI:               client.ClientURI,
		LogoURI:                 client.LogoURI,
		Scope:                   client.Scope,
		SoftwareID:              client.SoftwareID,
		SoftwareVersion:         client.SoftwareVersion,
	}, nil
}

func (s *OAuthService) BeginAuthorization(ctx context.Context, input AuthorizationInput) (*PendingAuthorizationInfo, error) {
	client, err := s.store.GetOAuthClient(ctx, input.ClientID)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, NewProtocolError("invalid_client", "Unknown OAuth client", http.StatusBadRequest)
	}

	redirectURI := input.RedirectURI
	if redirectURI == "" && len(client.RedirectURIs) == 1 {
		redirectURI = client.RedirectURIs[0]
	}
	matched := false
	if redirectURI != "" {
		for _, reg := range client.RedirectURIs {
			if RedirectURIMatches(redirectURI, reg) {
				matched = true
				break
			}
		}
	}
	if !matched {
		return nil, NewProtocolError("invalid_request", "Unregistered redirect_uri", http.StatusBadRequest)
	}

	fail := func(code, message string) (*PendingAuthorizationInfo, error) {
		return nil, &ProtocolError{
			ErrorCode:       code,
			Message:         message,
			StatusCode:      http.StatusBadRequest,
			SafeRedirectURI: redirectURI,
			State:           input.State,
		}
	}

	if input.ResponseType != "code" {
		return fail("unsupported_response_type", "Only response_type=code is supported")
	}
	if input.CodeChallengeMethod != "S256" || !pkceChallengePattern.MatchString(input.CodeChallenge) {
		return fail("invalid_request", "A valid S256 PKCE code challenge is required")
	}
	resource := input.Resource
	if resource == "" {
		resource = s.ResourceURL
	}
	if resource != s.ResourceURL {
		return fail("invalid_target", "The OAuth resource does not match this MCP server")
	}
	scopeStr := input.Scope
	if scopeStr == "" {
		scopeStr = McpOAuthScope
	}
	scopes := parseScope(scopeStr)
	if len(scopes) == 0 {
		return fail("invalid_scope", fmt.Sprintf("Only %s is supported", McpOAuthScope))
	}
	for _, sc := range scopes {
		if sc != McpOAuthScope {
			return fail("invalid_scope", fmt.Sprintf("Only %s is supported", McpOAuthScope))
		}
	}

	requestToken := randomToken(32)
	req := store.McpOAuthAuthorizationRequest{
		RequestHash:   store.HashSecret(requestToken),
		ClientID:      client.ClientID,
		RedirectURI:   redirectURI,
		State:         input.State,
		CodeChallenge: input.CodeChallenge,
		Resource:      resource,
		Scope:         McpOAuthScope,
		ExpiresAt:     time.Now().Add(time.Duration(s.cfg.McpOAuthRequestTTLSeconds) * time.Second),
		Attempts:      0,
	}
	if err := s.store.CreateAuthorizationRequest(ctx, req); err != nil {
		return nil, err
	}
	return &PendingAuthorizationInfo{
		RequestToken: requestToken,
		RequestHash:  req.RequestHash,
		ClientID:     client.ClientID,
		ClientName:   client.ClientName,
		RedirectURI:  redirectURI,
		Scope:        McpOAuthScope,
	}, nil
}

func (s *OAuthService) GetPendingAuthorization(ctx context.Context, requestToken string) (*store.McpOAuthAuthorizationRequest, error) {
	req, err := s.store.GetAuthorizationRequest(ctx, store.HashSecret(requestToken))
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, NewProtocolError("invalid_request", "Authorization request expired or invalid", http.StatusBadRequest)
	}
	return req, nil
}

func (s *OAuthService) CompleteBitrixAuthorization(ctx context.Context, requestHash, connectionID string) (string, error) {
	rawCode := randomToken(32)
	consumed, err := s.store.ConsumeAuthorizationRequestAndCreateCode(
		ctx,
		requestHash,
		store.HashSecret(rawCode),
		time.Now().Add(time.Duration(s.cfg.McpOAuthCodeTTLSeconds)*time.Second),
		connectionID,
	)
	if err != nil {
		return "", err
	}
	if consumed == nil {
		return "", NewProtocolError("invalid_request", "Authorization request was already used", http.StatusBadRequest)
	}
	redirect, err := url.Parse(consumed.RedirectURI)
	if err != nil {
		return "", err
	}
	q := redirect.Query()
	q.Set("code", rawCode)
	if consumed.State != "" {
		q.Set("state", consumed.State)
	}
	redirect.RawQuery = q.Encode()
	return redirect.String(), nil
}

func (s *OAuthService) FailBitrixAuthorization(ctx context.Context, requestHash, errorCode, description string) (string, error) {
	if errorCode == "" {
		errorCode = "access_denied"
	}
	if description == "" {
		description = "Bitrix24 authorization was not completed"
	}
	req, err := s.store.ConsumeAuthorizationRequest(ctx, requestHash)
	if err != nil {
		return "", err
	}
	if req == nil {
		return "", NewProtocolError("invalid_request", "Authorization request expired or invalid", http.StatusBadRequest)
	}
	redirect, err := url.Parse(req.RedirectURI)
	if err != nil {
		return "", err
	}
	q := redirect.Query()
	q.Set("error", errorCode)
	q.Set("error_description", description)
	if req.State != "" {
		q.Set("state", req.State)
	}
	redirect.RawQuery = q.Encode()
	return redirect.String(), nil
}

func (s *OAuthService) ExchangeAuthorizationCode(ctx context.Context, input AuthorizationCodeExchangeInput) (*OAuthTokens, error) {
	client, err := s.store.GetOAuthClient(ctx, input.ClientID)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, NewProtocolError("invalid_client", "Unknown OAuth client", http.StatusUnauthorized)
	}
	code, err := s.store.ConsumeAuthorizationCode(ctx, store.HashSecret(input.Code))
	if err != nil {
		return nil, err
	}
	if code == nil || code.ClientID != client.ClientID {
		return nil, NewProtocolError("invalid_grant", "Authorization code is invalid or expired", http.StatusBadRequest)
	}
	if input.RedirectURI != "" && !RedirectURIMatches(input.RedirectURI, code.RedirectURI) {
		return nil, NewProtocolError("invalid_grant", "redirect_uri does not match the authorization request", http.StatusBadRequest)
	}
	if input.Resource != "" && input.Resource != code.Resource {
		return nil, NewProtocolError("invalid_target", "The resource does not match the authorization request", http.StatusBadRequest)
	}
	if !verifyPKCE(input.CodeVerifier, code.CodeChallenge) {
		return nil, NewProtocolError("invalid_grant", "PKCE code_verifier does not match", http.StatusBadRequest)
	}
	return s.issueTokens(ctx, client.ClientID, code.ConnectionID, code.Resource, code.Scope)
}

func (s *OAuthService) ExchangeRefreshToken(ctx context.Context, input RefreshTokenExchangeInput) (*OAuthTokens, error) {
	client, err := s.store.GetOAuthClient(ctx, input.ClientID)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, NewProtocolError("invalid_client", "Unknown OAuth client", http.StatusUnauthorized)
	}
	resource := input.Resource
	if resource == "" {
		resource = s.ResourceURL
	}
	if resource != s.ResourceURL {
		return nil, NewProtocolError("invalid_target", "Invalid MCP resource", http.StatusBadRequest)
	}
	if input.Scope != "" {
		for _, sc := range parseScope(input.Scope) {
			if sc != McpOAuthScope {
				return nil, NewProtocolError("invalid_scope", fmt.Sprintf("Only %s is supported", McpOAuthScope), http.StatusBadRequest)
			}
		}
	}

	refreshTokenHash := store.HashSecret(input.RefreshToken)
	current, err := s.store.FindRefreshToken(ctx, refreshTokenHash, client.ClientID, resource)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, NewProtocolError("invalid_grant", "Refresh token is invalid, expired, or already used", http.StatusBadRequest)
	}

	tokens, replacement := s.generateTokenPair(client.ClientID, current.ConnectionID, resource, McpOAuthScope)
	previous, err := s.store.RotateRefreshToken(ctx, refreshTokenHash, client.ClientID, resource, replacement)
	if err != nil {
		return nil, err
	}
	if previous == nil {
		return nil, NewProtocolError("invalid_grant", "Refresh token is invalid, expired, or already used", http.StatusBadRequest)
	}
	return tokens, nil
}

func (s *OAuthService) VerifyAuthorizationHeader(ctx context.Context, authorization string) (*AccessInfo, error) {
	if !strings.HasPrefix(authorization, "Bearer ") {
		return nil, nil
	}
	token := authorization[len("Bearer "):]
	if token == "" {
		return nil, nil
	}
	if config.IsValidSecret(token, s.cfg.McpAuthToken) {
		return &AccessInfo{
			ClientID:          "legacy-static-token",
			Scopes:            []string{McpOAuthScope},
			Resource:          s.ResourceURL,
			LegacyStaticToken: true,
		}, nil
	}
	record, err := s.store.FindAccessToken(ctx, store.HashSecret(token))
	if err != nil || record == nil || record.Resource != s.ResourceURL {
		return nil, err
	}
	scopes := parseScope(record.Scope)
	if !slices.Contains(scopes, McpOAuthScope) {
		return nil, nil
	}
	return &AccessInfo{
		ClientID:     record.ClientID,
		ConnectionID: record.ConnectionID,
		Scopes:       scopes,
		Resource:     record.Resource,
		ExpiresAt:    record.AccessExpiresAt.Unix(),
	}, nil
}

func (s *OAuthService) RevokeToken(ctx context.Context, token string) error {
	return s.store.RevokeToken(ctx, store.HashSecret(token))
}

func (s *OAuthService) issueTokens(ctx context.Context, clientID, connectionID, resource, scope string) (*OAuthTokens, error) {
	tokens, rec := s.generateTokenPair(clientID, connectionID, resource, scope)
	if err := s.store.IssueTokenPair(ctx, rec); err != nil {
		return nil, err
	}
	return tokens, nil
}

func (s *OAuthService) generateTokenPair(clientID, connectionID, resource, scope string) (*OAuthTokens, store.NewMcpOAuthTokenRecord) {
	accessToken := randomToken(32)
	refreshToken := randomToken(48)
	now := time.Now()
	rec := store.NewMcpOAuthTokenRecord{
		AccessTokenHash:  store.HashSecret(accessToken),
		RefreshTokenHash: store.HashSecret(refreshToken),
		ClientID:         clientID,
		ConnectionID:     connectionID,
		Resource:         resource,
		Scope:            scope,
		AccessExpiresAt:  now.Add(time.Duration(s.cfg.McpOAuthAccessTokenTTLSeconds) * time.Second),
		RefreshExpiresAt: now.Add(time.Duration(s.cfg.McpOAuthRefreshTokenTTLSeconds) * time.Second),
	}
	tokens := &OAuthTokens{
		AccessToken:  accessToken,
		TokenType:    "Bearer",
		ExpiresIn:    s.cfg.McpOAuthAccessTokenTTLSeconds,
		RefreshToken: refreshToken,
		Scope:        scope,
	}
	return tokens, rec
}

func parseScope(scope string) []string {
	var out []string
	for _, f := range strings.Fields(scope) {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

func randomToken(numBytes int) string {
	b := make([]byte, numBytes)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func verifyPKCE(verifier, expectedChallenge string) bool {
	if !pkceVerifierPattern.MatchString(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	actual := base64.RawURLEncoding.EncodeToString(sum[:])
	return config.IsValidSecret(actual, expectedChallenge)
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
