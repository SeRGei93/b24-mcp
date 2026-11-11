package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/mcp"
	"github.com/SeRGei93/b24-mcp/internal/store"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

const (
	maxBodyBytes      = 1048576
	consentCSRFCookie = "mcp_oauth_csrf"
)

type Option func(*Server)

func WithMCPHandlerFactory(factory func(connectionID string) http.Handler) Option {
	return func(s *Server) {
		if factory != nil {
			s.mcpHandlerFactory = factory
		}
	}
}

type Server struct {
	cfg               *config.Config
	store             *store.Store
	oauth             *bitrix.OAuthService
	tokens            *bitrix.TokenManager
	mcpOAuth          *mcp.OAuthService
	schema            *bitrix.SchemaDiscovery
	crm               *bitrix.CrmService
	ops               *bitrix.OperationService
	mcpHandlerFactory func(connectionID string) http.Handler
	limiter           *WindowRateLimiter
	logger            *config.Logger
	allowedOrigin     string
	mux               *http.ServeMux
}

func NewHandler(
	cfg *config.Config,
	st *store.Store,
	oauth *bitrix.OAuthService,
	tokens *bitrix.TokenManager,
	logger *config.Logger,
	opts ...Option,
) http.Handler {
	var httpClient *http.Client
	if oauth != nil {
		httpClient = oauth.HTTPClient()
	}
	rc := bitrix.NewRestClient(cfg, tokens, logger, httpClient)
	schema := bitrix.NewSchemaDiscovery(cfg, st, rc)
	crm := bitrix.NewCrmService(cfg, rc)
	ops := bitrix.NewOperationService(cfg, st, rc)
	mcpOAuth := mcp.NewOAuthService(cfg, st)

	return NewHandlerWithServices(cfg, st, oauth, tokens, mcpOAuth, schema, crm, ops, logger, opts...)
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
	opts ...Option,
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
		cfg:               cfg,
		store:             st,
		oauth:             oauth,
		tokens:            tokens,
		mcpOAuth:          mcpOAuth,
		schema:            schema,
		crm:               crm,
		ops:               ops,
		mcpHandlerFactory: newDefaultMCPHandlerFactory(st, schema, crm, ops),
		limiter:           NewWindowRateLimiter(8192),
		logger:            logger,
		allowedOrigin:     allowedOrigin,
		mux:               http.NewServeMux(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	s.registerRoutes()
	return s.mux
}

func newDefaultMCPHandlerFactory(
	st *store.Store,
	schema *bitrix.SchemaDiscovery,
	crm *bitrix.CrmService,
	ops *bitrix.OperationService,
) func(connectionID string) http.Handler {
	buildHandler := func(connectionID string) http.Handler {
		mcpSrv := mcp.NewServer(st, schema, crm, ops, connectionID)
		return mcpserver.NewStreamableHTTPServer(
			mcpSrv,
			mcpserver.WithStateLess(true),
			mcpserver.WithDisableStreaming(true),
		)
	}
	defaultHandler := buildHandler("")
	return func(connectionID string) http.Handler {
		if connectionID == "" {
			return defaultHandler
		}
		return buildHandler(connectionID)
	}
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
	if !s.isAllowedOrigin(r.Header.Get("Origin")) {
		return nil, config.NewAppError("Request origin is not allowed", "ORIGIN_NOT_ALLOWED", http.StatusForbidden)
	}
	return access, nil
}

func (s *Server) isAllowedOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	if s.allowedOrigin != "" {
		return origin == s.allowedOrigin
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return false
	}
	return config.IsLoopbackHost(parsed.Hostname())
}

func (s *Server) handleMCPPost(w http.ResponseWriter, r *http.Request) {
	access, err := s.authenticate(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.mcpHandlerFactory(access.ConnectionID).ServeHTTP(w, r)
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
