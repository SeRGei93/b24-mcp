package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/mcp"
	"github.com/SeRGei93/b24-mcp/internal/store"
	"github.com/google/uuid"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

const maxBodyBytes = 1048576

var (
	authKeyPattern = regexp.MustCompile(`^auth\[([^\]]+)\]$`)
	unsafeKeys     = map[string]struct{}{
		"__proto__":   {},
		"constructor": {},
		"prototype":   {},
	}
)

type AccessInfo struct {
	ClientID     string
	ConnectionID string
	Scopes       []string
	Resource     string
}

type Server struct {
	cfg           *config.Config
	store         *store.Store
	oauth         *bitrix.OAuthService
	tokens        *bitrix.TokenManager
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
	parsedOrigin, _ := url.Parse(cfg.AppURL)
	allowedOrigin := ""
	if parsedOrigin != nil && parsedOrigin.Scheme != "" && parsedOrigin.Host != "" {
		allowedOrigin = parsedOrigin.Scheme + "://" + parsedOrigin.Host
	}

	s := &Server{
		cfg:           cfg,
		store:         st,
		oauth:         oauth,
		tokens:        tokens,
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
		"version":               "0.3.1",
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

func (s *Server) authenticate(r *http.Request) (*AccessInfo, error) {
	authHeader := r.Header.Get("Authorization")
	if !config.IsValidBearerToken(authHeader, s.cfg.McpAuthToken) {
		return nil, config.NewAuthenticationError("Unauthorized")
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.allowedOrigin {
		return nil, config.NewAppError("Request origin is not allowed", "ORIGIN_NOT_ALLOWED", http.StatusForbidden)
	}
	return &AccessInfo{
		ClientID: "static-bearer",
		Scopes:   []string{"mcp:tools"},
		Resource: s.cfg.AppURL + "/mcp",
	}, nil
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
		s.writeError(w, err)
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
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><script src="https://api.bitrix24.com/api/v1/"></script></head><body><p>Bitrix24 connected (%s).</p><script>if(window.BX24){BX24.init(function(){BX24.installFinish();});}</script></body></html>`, conn.Domain)
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
	mcpSrv := mcp.NewServer(s.store, access.ConnectionID)
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

func (s *Server) challengeHeader(errCode string) string {
	return fmt.Sprintf(
		`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp", scope="mcp:tools", error="%s"`,
		s.cfg.AppURL,
		errCode,
	)
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
		w.Header().Set("WWW-Authenticate", s.challengeHeader("invalid_token"))
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
