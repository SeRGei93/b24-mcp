package server

import (
	"encoding/json"
	"net/http"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
)

type Server struct {
	cfg    *config.Config
	store  *store.Store
	logger *config.Logger
	mux    *http.ServeMux
}

func NewHandler(cfg *config.Config, st *store.Store, logger *config.Logger) http.Handler {
	s := &Server{
		cfg:    cfg,
		store:  st,
		logger: logger,
		mux:    http.NewServeMux(),
	}
	s.registerRoutes()
	return s.mux
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /{$}", s.handleRoot)
	s.mux.HandleFunc("GET /health", s.handleHealth)
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

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
