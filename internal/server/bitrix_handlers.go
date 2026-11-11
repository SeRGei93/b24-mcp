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
	"github.com/google/uuid"
)

var (
	authKeyPattern = regexp.MustCompile(`^auth\[([^\]]+)\]$`)
	unsafeKeys     = map[string]struct{}{
		"__proto__":   {},
		"constructor": {},
		"prototype":   {},
	}
)

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
