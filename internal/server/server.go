package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/mcp"
	"github.com/SeRGei93/b24-mcp/internal/store"
	"github.com/google/uuid"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

const (
	maxBodyBytes      = 1048576
	consentCSRFCookie = "mcp_oauth_csrf"
)

var (
	authKeyPattern = regexp.MustCompile(`^auth\[([^\]]+)\]$`)
	unsafeKeys     = map[string]struct{}{
		"__proto__":   {},
		"constructor": {},
		"prototype":   {},
	}
)

type Server struct {
	cfg           *config.Config
	store         *store.Store
	oauth         *bitrix.OAuthService
	tokens        *bitrix.TokenManager
	mcpOAuth      *mcp.OAuthService
	schema        *bitrix.SchemaDiscovery
	crm           *bitrix.CrmService
	ops           *bitrix.OperationService
	limiter       *WindowRateLimiter
	logger        *config.Logger
	allowedOrigin string
	mux           *http.ServeMux
}

func NewHandler(
	cfg *config.Config,
	st *store.Store,
	oauth *bitrix.OAuthService,
	tokens *bitrix.TokenManager,
	logger *config.Logger,
) http.Handler {
	var httpClient *http.Client
	if oauth != nil {
		httpClient = oauth.HTTPClient()
	}
	rc := bitrix.NewRestClient(cfg, tokens, logger, httpClient)
	schema := bitrix.NewSchemaDiscovery(cfg, st, rc)
	crm := bitrix.NewCrmService(cfg, rc)
	guard := bitrix.NewPolicyGuard()
	ops := bitrix.NewOperationService(cfg, st, guard, rc)
	mcpOAuth := mcp.NewOAuthService(cfg, st)

	return NewHandlerWithServices(cfg, st, oauth, tokens, mcpOAuth, schema, crm, ops, logger)
}

func NewHandlerWithServices(
	cfg *config.Config,
	st *store.Store,
	oauth *bitrix.OAuthService,
	tokens *bitrix.TokenManager,
	mcpOAuth *mcp.OAuthService,
	schema *bitrix.SchemaDiscovery,
	crm *bitrix.CrmService,
	ops *bitrix.OperationService,
	logger *config.Logger,
) http.Handler {
	parsedOrigin, _ := url.Parse(cfg.AppURL)
	allowedOrigin := ""
	if parsedOrigin != nil && parsedOrigin.Scheme != "" && parsedOrigin.Host != "" {
		allowedOrigin = parsedOrigin.Scheme + "://" + parsedOrigin.Host
	}
	if mcpOAuth == nil {
		mcpOAuth = mcp.NewOAuthService(cfg, st)
	}

	s := &Server{
		cfg:           cfg,
		store:         st,
		oauth:         oauth,
		tokens:        tokens,
		mcpOAuth:      mcpOAuth,
		schema:        schema,
		crm:           crm,
		ops:           ops,
		limiter:       NewWindowRateLimiter(8192),
		logger:        logger,
		allowedOrigin: allowedOrigin,
		mux:           http.NewServeMux(),
	}
	s.registerRoutes()
	return s.mux
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /{$}", s.handleRoot)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)
	s.mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.handleProtectedResourceMetadata)
	s.mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleAuthorizationServerMetadata)
	s.mux.HandleFunc("OPTIONS /register", s.handleOAuthOptions)
	s.mux.HandleFunc("OPTIONS /token", s.handleOAuthOptions)
	s.mux.HandleFunc("OPTIONS /revoke", s.handleOAuthOptions)
	s.mux.HandleFunc("POST /register", s.handleRegister)
	s.mux.HandleFunc("GET /authorize", s.handleAuthorizeGet)
	s.mux.HandleFunc("POST /authorize", s.handleAuthorizePost)
	s.mux.HandleFunc("POST /token", s.handleToken)
	s.mux.HandleFunc("POST /revoke", s.handleRevoke)
	s.mux.HandleFunc("GET /oauth/bitrix/start", s.handleOAuthStart)
	s.mux.HandleFunc("GET /oauth/bitrix/callback", s.handleOAuthCallback)
	s.mux.HandleFunc("GET /oauth/bitrix/install", s.handleOAuthInstallGet)
	s.mux.HandleFunc("POST /oauth/bitrix/install", s.handleOAuthInstallPost)
	s.mux.HandleFunc("POST /oauth/bitrix/callback", s.handleOAuthInstallPost)
	s.mux.HandleFunc("POST /oauth/bitrix/refresh", s.handleOAuthRefresh)
	s.mux.HandleFunc("POST /mcp", s.handleMCPPost)
	s.mux.HandleFunc("GET /mcp", s.handleMCPMethodNotAllowed)
	s.mux.HandleFunc("DELETE /mcp", s.handleMCPMethodNotAllowed)
}

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"name":                  "bitrix24-mcp-server",
		"version":               "0.1",
		"mcp_endpoint":          "/mcp",
		"oauth_start":           "/oauth/bitrix/start?domain=your-portal.bitrix24.com",
		"bitrix_oauth_callback": "/oauth/bitrix/callback",
		"oauth_metadata":        "/.well-known/oauth-authorization-server",
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status":   "unavailable",
			"database": "disconnected",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "ok",
		"database": "connected",
	})
}

func (s *Server) authenticate(r *http.Request) (*mcp.AccessInfo, error) {
	access, err := s.mcpOAuth.VerifyAuthorizationHeader(r.Context(), r.Header.Get("Authorization"))
	if err != nil || access == nil {
		return nil, config.NewAuthenticationError("Unauthorized")
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.allowedOrigin {
		return nil, config.NewAppError("Request origin is not allowed", "ORIGIN_NOT_ALLOWED", http.StatusForbidden)
	}
	return access, nil
}

func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, s.mcpOAuth.ProtectedResourceMetadata())
}

func (s *Server) handleAuthorizationServerMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, s.mcpOAuth.AuthorizationServerMetadata())
}

func (s *Server) handleOAuthOptions(w http.ResponseWriter, _ *http.Request) {
	setCORSHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	setOAuthHeaders(w)
	if err := s.enforceRateLimit(r, "register", 20, 60*time.Minute); err != nil {
		s.writeOAuthError(w, normalizeOAuthError(err))
		return
	}
	var body struct {
		RedirectURIs            []string `json:"redirect_uris"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		ClientName              string   `json:"client_name"`
		ClientURI               string   `json:"client_uri"`
		LogoURI                 string   `json:"logo_uri"`
		Scope                   string   `json:"scope"`
		SoftwareID              string   `json:"software_id"`
		SoftwareVersion         string   `json:"software_version"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&body); err != nil {
		s.writeOAuthError(w, mcp.NewProtocolError("invalid_client_metadata", "OAuth request validation failed", http.StatusBadRequest))
		return
	}
	if len(body.ClientName) > 200 || len(body.SoftwareID) > 200 || len(body.SoftwareVersion) > 100 {
		s.writeOAuthError(w, mcp.NewProtocolError("invalid_client_metadata", "OAuth request validation failed", http.StatusBadRequest))
		return
	}
	if body.ClientURI != "" && !isValidHTTPOrHTTPSURL(body.ClientURI) {
		s.writeOAuthError(w, mcp.NewProtocolError("invalid_client_metadata", "OAuth request validation failed", http.StatusBadRequest))
		return
	}
	if body.LogoURI != "" && !isValidHTTPOrHTTPSURL(body.LogoURI) {
		s.writeOAuthError(w, mcp.NewProtocolError("invalid_client_metadata", "OAuth request validation failed", http.StatusBadRequest))
		return
	}
	registered, err := s.mcpOAuth.RegisterClient(r.Context(), mcp.ClientRegistrationInput{
		RedirectURIs:            body.RedirectURIs,
		TokenEndpointAuthMethod: body.TokenEndpointAuthMethod,
		GrantTypes:              body.GrantTypes,
		ResponseTypes:           body.ResponseTypes,
		ClientName:              body.ClientName,
		ClientURI:               body.ClientURI,
		LogoURI:                 body.LogoURI,
		Scope:                   body.Scope,
		SoftwareID:              body.SoftwareID,
		SoftwareVersion:         body.SoftwareVersion,
	})
	if err != nil {
		s.writeOAuthError(w, normalizeOAuthError(err))
		return
	}
	writeJSON(w, http.StatusCreated, registered)
}

func (s *Server) handleAuthorizeGet(w http.ResponseWriter, r *http.Request) {
	setOAuthHeaders(w)
	if err := s.enforceRateLimit(r, "authorize", 100, 15*time.Minute); err != nil {
		s.writeOAuthError(w, normalizeOAuthError(err))
		return
	}
	q := r.URL.Query()
	clientID := q.Get("client_id")
	codeChallenge := q.Get("code_challenge")
	codeChallengeMethod := q.Get("code_challenge_method")
	if clientID == "" || codeChallenge == "" || codeChallengeMethod == "" {
		s.writeOAuthError(w, mcp.NewProtocolError("invalid_request", "OAuth request validation failed", http.StatusBadRequest))
		return
	}
	pending, err := s.mcpOAuth.BeginAuthorization(r.Context(), mcp.AuthorizationInput{
		ResponseType:        q.Get("response_type"),
		ClientID:            clientID,
		RedirectURI:         q.Get("redirect_uri"),
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		State:               q.Get("state"),
		Scope:               q.Get("scope"),
		Resource:            q.Get("resource"),
	})
	if err != nil {
		normalized := normalizeOAuthError(err)
		if normalized.SafeRedirectURI != "" {
			http.Redirect(w, r, oauthErrorRedirect(normalized), http.StatusFound)
			return
		}
		s.writeOAuthError(w, normalized)
		return
	}
	if config.IsValidBearerToken(r.Header.Get("Authorization"), s.cfg.McpAuthToken) {
		s.redirectToBitrixAuthorization(w, r, pending.RequestHash)
		return
	}
	s.sendAuthorizationConsentPage(w, pending)
}

func (s *Server) handleAuthorizePost(w http.ResponseWriter, r *http.Request) {
	setOAuthHeaders(w)
	if err := s.enforceRateLimit(r, "authorize", 100, 15*time.Minute); err != nil {
		s.writeOAuthError(w, normalizeOAuthError(err))
		return
	}
	bodyMap, err := parseRequestBodyMap(r)
	if err != nil {
		s.writeOAuthError(w, mcp.NewProtocolError("invalid_request", "OAuth request validation failed", http.StatusBadRequest))
		return
	}
	requestToken, _ := bodyMap["request_token"].(string)
	csrfToken, _ := bodyMap["csrf_token"].(string)
	decision, _ := bodyMap["decision"].(string)
	if requestToken == "" || csrfToken == "" || (decision != "approve" && decision != "deny") {
		s.writeOAuthError(w, mcp.NewProtocolError("invalid_request", "OAuth request validation failed", http.StatusBadRequest))
		return
	}

	csrfCookie := readCookie(r.Header.Get("Cookie"), consentCSRFCookie)
	s.clearConsentCookie(w)
	if csrfCookie == "" || !config.IsValidSecret(csrfToken, csrfCookie) {
		s.writeOAuthError(w, mcp.NewProtocolError("invalid_request", "Invalid or missing OAuth consent CSRF token", http.StatusForbidden))
		return
	}

	pending, err := s.mcpOAuth.GetPendingAuthorization(r.Context(), requestToken)
	if err != nil {
		normalized := normalizeOAuthError(err)
		if normalized.SafeRedirectURI != "" {
			http.Redirect(w, r, oauthErrorRedirect(normalized), http.StatusFound)
			return
		}
		s.writeOAuthError(w, normalized)
		return
	}
	if decision != "approve" {
		location, err := s.mcpOAuth.FailBitrixAuthorization(
			r.Context(),
			pending.RequestHash,
			"access_denied",
			"The user denied the authorization request",
		)
		if err != nil {
			s.writeOAuthError(w, normalizeOAuthError(err))
			return
		}
		http.Redirect(w, r, location, http.StatusFound)
		return
	}
	s.redirectToBitrixAuthorization(w, r, pending.RequestHash)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	setOAuthHeaders(w)
	if err := s.enforceRateLimit(r, "token", 50, 15*time.Minute); err != nil {
		s.writeOAuthError(w, normalizeOAuthError(err))
		return
	}
	bodyMap, err := parseRequestBodyMap(r)
	if err != nil {
		s.writeOAuthError(w, mcp.NewProtocolError("invalid_request", "OAuth request validation failed", http.StatusBadRequest))
		return
	}
	grantType, _ := bodyMap["grant_type"].(string)
	if grantType == "" {
		s.writeOAuthError(w, mcp.NewProtocolError("invalid_request", "OAuth request validation failed", http.StatusBadRequest))
		return
	}

	switch grantType {
	case "authorization_code":
		clientID, _ := bodyMap["client_id"].(string)
		code, _ := bodyMap["code"].(string)
		codeVerifier, _ := bodyMap["code_verifier"].(string)
		redirectURI, _ := bodyMap["redirect_uri"].(string)
		resource, _ := bodyMap["resource"].(string)
		if clientID == "" || code == "" || codeVerifier == "" {
			s.writeOAuthError(w, mcp.NewProtocolError("invalid_request", "OAuth request validation failed", http.StatusBadRequest))
			return
		}
		tokens, err := s.mcpOAuth.ExchangeAuthorizationCode(r.Context(), mcp.AuthorizationCodeExchangeInput{
			ClientID:     clientID,
			Code:         code,
			CodeVerifier: codeVerifier,
			RedirectURI:  redirectURI,
			Resource:     resource,
		})
		if err != nil {
			s.writeOAuthError(w, normalizeOAuthError(err))
			return
		}
		writeJSON(w, http.StatusOK, tokens)

	case "refresh_token":
		clientID, _ := bodyMap["client_id"].(string)
		refreshToken, _ := bodyMap["refresh_token"].(string)
		scope, _ := bodyMap["scope"].(string)
		resource, _ := bodyMap["resource"].(string)
		if clientID == "" || refreshToken == "" {
			s.writeOAuthError(w, mcp.NewProtocolError("invalid_request", "OAuth request validation failed", http.StatusBadRequest))
			return
		}
		tokens, err := s.mcpOAuth.ExchangeRefreshToken(r.Context(), mcp.RefreshTokenExchangeInput{
			ClientID:     clientID,
			RefreshToken: refreshToken,
			Scope:        scope,
			Resource:     resource,
		})
		if err != nil {
			s.writeOAuthError(w, normalizeOAuthError(err))
			return
		}
		writeJSON(w, http.StatusOK, tokens)

	default:
		s.writeOAuthError(w, mcp.NewProtocolError("unsupported_grant_type", "Unsupported OAuth grant type", http.StatusBadRequest))
	}
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	setOAuthHeaders(w)
	if err := s.enforceRateLimit(r, "revoke", 50, 15*time.Minute); err != nil {
		s.writeOAuthError(w, normalizeOAuthError(err))
		return
	}
	bodyMap, err := parseRequestBodyMap(r)
	if err != nil {
		s.writeOAuthError(w, mcp.NewProtocolError("invalid_request", "OAuth request validation failed", http.StatusBadRequest))
		return
	}
	token, _ := bodyMap["token"].(string)
	if token == "" {
		s.writeOAuthError(w, mcp.NewProtocolError("invalid_request", "OAuth request validation failed", http.StatusBadRequest))
		return
	}
	if err := s.mcpOAuth.RevokeToken(r.Context(), token); err != nil {
		s.writeOAuthError(w, normalizeOAuthError(err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	access, err := s.authenticate(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if access.ConnectionID != "" {
		s.writeError(w, config.NewAppError("This MCP token cannot initiate standalone Bitrix24 OAuth flows", "CONNECTION_ACCESS_DENIED", http.StatusForbidden))
		return
	}
	domain := strings.TrimSpace(r.URL.Query().Get("domain"))
	if domain == "" {
		s.writeError(w, config.NewAppError("Request validation failed: domain is required", "INVALID_REQUEST", http.StatusBadRequest))
		return
	}
	modeStr := strings.TrimSpace(r.URL.Query().Get("mode"))
	var mode config.ConnectionMode
	if modeStr != "" {
		parsedMode, err := config.ParseConnectionMode(modeStr)
		if err != nil {
			s.writeError(w, config.NewAppError("Request validation failed: invalid mode", "INVALID_REQUEST", http.StatusBadRequest))
			return
		}
		mode = parsedMode
	}
	location, err := s.oauth.CreateAuthorizationURL(r.Context(), domain, mode, "")
	if err != nil {
		s.writeError(w, err)
		return
	}
	http.Redirect(w, r, location, http.StatusFound)
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	result, err := s.oauth.HandleCallbackWithContext(r.Context(), bitrix.CallbackInput{
		Code:     q.Get("code"),
		State:    q.Get("state"),
		Domain:   q.Get("domain"),
		MemberID: q.Get("member_id"),
		Error:    q.Get("error"),
	})
	if err != nil {
		var contErr *bitrix.ContinuationError
		if errors.As(err, &contErr) {
			location, failErr := s.mcpOAuth.FailBitrixAuthorization(
				r.Context(),
				contErr.McpRequestHash,
				contErr.OAuthErrorCode,
				contErr.Message,
			)
			if failErr != nil {
				s.writeError(w, failErr)
				return
			}
			http.Redirect(w, r, location, http.StatusFound)
			return
		}
		s.writeError(w, err)
		return
	}
	if result.McpRequestHash != "" {
		location, err := s.mcpOAuth.CompleteBitrixAuthorization(
			r.Context(),
			result.McpRequestHash,
			result.Connection.ID,
		)
		if err != nil {
			s.writeError(w, err)
			return
		}
		http.Redirect(w, r, location, http.StatusFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connected":  true,
		"connection": result.Connection.ToPublic(),
		"next":       "Use this connection.id with the Bitrix24 MCP tools.",
	})
}

func (s *Server) handleOAuthInstallGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "Bitrix24 installation endpoint is ready (expects POST from Bitrix24).",
	})
}

func (s *Server) handleOAuthInstallPost(w http.ResponseWriter, r *http.Request) {
	bodyMap, err := parseRequestBodyMap(r)
	if err != nil {
		s.writeError(w, config.NewAppError("Invalid request body", "INVALID_REQUEST", http.StatusBadRequest))
		return
	}
	queryMap := make(map[string]any, len(r.URL.Query()))
	for k, vals := range r.URL.Query() {
		if len(vals) > 0 {
			queryMap[k] = vals[0]
		}
	}
	referer := r.Header.Get("Origin")
	if referer == "" {
		referer = r.Header.Get("Referer")
	}
	var refererDomain string
	if referer != "" {
		if parsed, err := url.Parse(referer); err == nil {
			refererDomain = parsed.Hostname()
		}
	}

	var payload map[string]any
	if authObj, ok := bodyMap["auth"].(map[string]any); ok && len(queryMap) == 0 && refererDomain == "" {
		payload = authObj
	} else {
		payload = make(map[string]any)
		if refererDomain != "" {
			payload["domain"] = refererDomain
		}
		for k, v := range queryMap {
			payload[k] = v
		}
		for k, v := range bodyMap {
			payload[k] = v
		}
	}

	conn, err := s.oauth.HandleInstallation(r.Context(), payload)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><script src="https://api.bitrix24.com/api/v1/"></script></head><body><p>Bitrix24 connected (%s).</p><script>if(window.BX24){BX24.init(function(){BX24.installFinish();});}</script></body></html>`, escapeHTML(conn.Domain))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "success",
		"connected":  true,
		"connection": conn.ToPublic(),
	})
}

func (s *Server) handleOAuthRefresh(w http.ResponseWriter, r *http.Request) {
	access, err := s.authenticate(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var body struct {
		ConnectionID string `json:"connection_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&body); err != nil {
		s.writeError(w, config.NewAppError("Request validation failed: invalid JSON body", "INVALID_REQUEST", http.StatusBadRequest))
		return
	}
	if _, err := uuid.Parse(body.ConnectionID); err != nil {
		s.writeError(w, config.NewAppError("Request validation failed: connection_id (Invalid UUID)", "INVALID_REQUEST", http.StatusBadRequest))
		return
	}
	if access.ConnectionID != "" && access.ConnectionID != body.ConnectionID {
		s.writeError(w, config.NewAppError("This MCP token is not authorized for that Bitrix24 connection", "CONNECTION_ACCESS_DENIED", http.StatusForbidden))
		return
	}
	refreshed, err := s.tokens.Get(r.Context(), body.ConnectionID, true, "")
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"refreshed":  true,
		"connection": refreshed.Connection.ToPublic(),
	})
}

func (s *Server) handleMCPPost(w http.ResponseWriter, r *http.Request) {
	access, err := s.authenticate(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	mcpSrv := mcp.NewServer(s.store, s.schema, s.crm, s.ops, access.ConnectionID)
	streamSrv := mcpserver.NewStreamableHTTPServer(
		mcpSrv,
		mcpserver.WithStateLess(true),
		mcpserver.WithDisableStreaming(true),
	)
	streamSrv.ServeHTTP(w, r)
}

func (s *Server) handleMCPMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticate(r); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
		"jsonrpc": "2.0",
		"error": map[string]any{
			"code":    -32000,
			"message": "Method not allowed",
		},
		"id": nil,
	})
}

func parseRequestBodyMap(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/x-www-form-urlencoded") {
		return parseFormURLEncoded(string(raw))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func parseFormURLEncoded(raw string) (map[string]any, error) {
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	parsed := make(map[string]any)
	auth := make(map[string]any)
	for k, v := range vals {
		if _, bad := unsafeKeys[k]; bad || len(v) == 0 {
			continue
		}
		val := v[len(v)-1]
		if m := authKeyPattern.FindStringSubmatch(k); len(m) == 2 && m[1] != "" {
			if _, badNested := unsafeKeys[m[1]]; !badNested {
				auth[m[1]] = val
			}
		} else {
			parsed[k] = val
		}
	}
	if len(auth) > 0 {
		parsed["auth"] = auth
	}
	return parsed, nil
}

func (s *Server) redirectToBitrixAuthorization(w http.ResponseWriter, r *http.Request, requestHash string) {
	location, err := s.oauth.CreateMcpAuthorizationURL(r.Context(), requestHash)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("Could not start Bitrix24 authorization", map[string]any{"error": err.Error()})
		}
		failLoc, failErr := s.mcpOAuth.FailBitrixAuthorization(
			r.Context(),
			requestHash,
			"temporarily_unavailable",
			"Bitrix24 authorization is not available for this MCP server",
		)
		if failErr != nil {
			s.writeOAuthError(w, normalizeOAuthError(failErr))
			return
		}
		http.Redirect(w, r, failLoc, http.StatusFound)
		return
	}
	http.Redirect(w, r, location, http.StatusFound)
}

func (s *Server) sendAuthorizationConsentPage(w http.ResponseWriter, pending *mcp.PendingAuthorizationInfo) {
	csrfBytes := make([]byte, 32)
	_, _ = rand.Read(csrfBytes)
	csrfToken := base64.RawURLEncoding.EncodeToString(csrfBytes)
	w.Header().Set("Set-Cookie", s.formatConsentCookie(csrfToken, s.cfg.McpOAuthRequestTTLSeconds))
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	clientLabel := pending.ClientName
	if clientLabel == "" {
		clientLabel = pending.ClientID
	}
	redirectOrigin := ""
	if parsedRedir, err := url.Parse(pending.RedirectURI); err == nil && parsedRedir.Scheme != "" && parsedRedir.Host != "" {
		redirectOrigin = parsedRedir.Scheme + "://" + parsedRedir.Host
	}

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(
		w,
		`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Authorize MCP Client</title>`+
			`<style>body{font-family:system-ui,-apple-system,sans-serif;max-width:480px;margin:48px auto;padding:0 16px;color:#111}card{display:block;border:1px solid #ddd;border-radius:8px;padding:24px}code{background:#f4f4f5;padding:2px 6px;border-radius:4px;word-break:break-all}.actions{display:flex;gap:12px;margin-top:20px}button{padding:10px 18px;border-radius:6px;border:1px solid #ccc;cursor:pointer;font-weight:600}button[value="approve"]{background:#111;color:#fff;border-color:#111}</style></head>`+
			`<body><card><h1>Authorize Bitrix24 MCP Access</h1>`+
			`<p>Client <strong>%s</strong> (<code>%s</code>) is requesting access to your Bitrix24 MCP server.</p>`+
			`<p>Redirect origin: <code>%s</code><br>Callback URI: <code>%s</code><br>Scope: <code>%s</code></p>`+
			`<form method="post" action="/authorize">`+
			`<input type="hidden" name="request_token" value="%s">`+
			`<input type="hidden" name="csrf_token" value="%s">`+
			`<div class="actions"><button type="submit" name="decision" value="approve">Разрешить</button>`+
			`<button type="submit" name="decision" value="deny">Отклонить</button></div>`+
			`</form></card></body></html>`,
		escapeHTML(clientLabel),
		escapeHTML(pending.ClientID),
		escapeHTML(redirectOrigin),
		escapeHTML(pending.RedirectURI),
		escapeHTML(pending.Scope),
		escapeHTML(pending.RequestToken),
		escapeHTML(csrfToken),
	)
}

func (s *Server) clearConsentCookie(w http.ResponseWriter) {
	w.Header().Set("Set-Cookie", s.formatConsentCookie("", 0))
}

func (s *Server) formatConsentCookie(value string, maxAge int) string {
	secureAttr := ""
	if strings.HasPrefix(s.cfg.AppURL, "https://") {
		secureAttr = "; Secure"
	}
	return fmt.Sprintf("%s=%s; HttpOnly; SameSite=Lax; Path=/authorize; Max-Age=%d%s", consentCSRFCookie, value, maxAge, secureAttr)
}

func readCookie(cookieHeader, name string) string {
	if cookieHeader == "" {
		return ""
	}
	for _, part := range strings.Split(cookieHeader, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && k == name {
			return v
		}
	}
	return ""
}

func escapeHTML(val string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	return r.Replace(val)
}

func isValidHTTPOrHTTPSURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func setOAuthHeaders(w http.ResponseWriter) {
	setCORSHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
}

func normalizeOAuthError(err error) *mcp.ProtocolError {
	var protoErr *mcp.ProtocolError
	if errors.As(err, &protoErr) {
		return protoErr
	}
	return mcp.NewProtocolError("server_error", "The authorization server could not process the request", http.StatusInternalServerError)
}

func (s *Server) writeOAuthError(w http.ResponseWriter, protoErr *mcp.ProtocolError) {
	setOAuthHeaders(w)
	writeJSON(w, protoErr.StatusCode, map[string]string{
		"error":             protoErr.ErrorCode,
		"error_description": protoErr.Message,
	})
}

func oauthErrorRedirect(protoErr *mcp.ProtocolError) string {
	target, err := url.Parse(protoErr.SafeRedirectURI)
	if err != nil {
		return protoErr.SafeRedirectURI
	}
	q := target.Query()
	q.Set("error", protoErr.ErrorCode)
	q.Set("error_description", protoErr.Message)
	if protoErr.State != "" {
		q.Set("state", protoErr.State)
	}
	target.RawQuery = q.Encode()
	return target.String()
}

type rateLimitEntry struct {
	count   int
	resetAt time.Time
}

type WindowRateLimiter struct {
	mu         sync.Mutex
	maxEntries int
	entries    map[string]*rateLimitEntry
}

func NewWindowRateLimiter(maxEntries int) *WindowRateLimiter {
	if maxEntries <= 0 {
		maxEntries = 8192
	}
	return &WindowRateLimiter{
		maxEntries: maxEntries,
		entries:    make(map[string]*rateLimitEntry),
	}
}

func (l *WindowRateLimiter) Consume(key string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cur, ok := l.entries[key]
	if !ok || !cur.resetAt.After(now) {
		if !ok && len(l.entries) >= l.maxEntries {
			for k, entry := range l.entries {
				if !entry.resetAt.After(now) {
					delete(l.entries, k)
				}
			}
			if len(l.entries) >= l.maxEntries {
				return false
			}
		}
		l.entries[key] = &rateLimitEntry{
			count:   1,
			resetAt: now.Add(window),
		}
		return true
	}
	cur.count++
	return cur.count <= limit
}

func (s *Server) enforceRateLimit(r *http.Request, bucket string, limit int, window time.Duration) error {
	clientIP := resolveClientIP(r, s.cfg.TrustProxy)
	if !s.limiter.Consume(fmt.Sprintf("%s:source:%s", bucket, clientIP), limit, window) {
		return mcp.NewProtocolError("temporarily_unavailable", "Too many OAuth requests", http.StatusTooManyRequests)
	}
	globalLimit := max(limit*25, 100)
	if !s.limiter.Consume(fmt.Sprintf("%s:global", bucket), globalLimit, window) {
		return mcp.NewProtocolError("temporarily_unavailable", "Too many OAuth requests", http.StatusTooManyRequests)
	}
	return nil
}

func resolveClientIP(r *http.Request, trustProxy config.TrustProxyConfig) string {
	remoteIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		remoteIP = host
	}
	if !trustProxy.Enabled {
		return remoteIP
	}
	if len(trustProxy.Addresses) > 0 && !isTrustedProxyAddr(remoteIP, trustProxy.Addresses) {
		return remoteIP
	}
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff == "" {
		return remoteIP
	}
	var parts []string
	for _, p := range strings.Split(xff, ",") {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	if len(parts) == 0 {
		return remoteIP
	}
	if len(trustProxy.Addresses) > 0 {
		for i := len(parts) - 1; i >= 0; i-- {
			if !isTrustedProxyAddr(parts[i], trustProxy.Addresses) {
				return parts[i]
			}
		}
		return parts[0]
	}
	hops := trustProxy.Hops
	if hops <= 0 {
		hops = 1
	}
	idx := len(parts) - hops
	if idx < 0 {
		idx = 0
	}
	return parts[idx]
}

func isTrustedProxyAddr(ipStr string, trusted []string) bool {
	parsedIP := net.ParseIP(ipStr)
	for _, entry := range trusted {
		if entry == ipStr {
			return true
		}
		if parsedIP != nil {
			if _, cidr, err := net.ParseCIDR(entry); err == nil && cidr.Contains(parsedIP) {
				return true
			}
		}
	}
	return false
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	statusCode := http.StatusInternalServerError
	code := "INTERNAL_ERROR"
	var appErr *config.AppError
	if errors.As(err, &appErr) {
		statusCode = appErr.StatusCode
		code = appErr.Code
	}
	if s.logger != nil {
		s.logger.Error("Request failed", map[string]any{
			"code":  code,
			"error": err.Error(),
		})
	}
	if statusCode == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", s.mcpOAuth.Challenge("invalid_token"))
	}
	writeJSON(w, statusCode, map[string]string{
		"error":   code,
		"message": config.SafeErrorMessage(err),
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
