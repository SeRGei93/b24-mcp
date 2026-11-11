package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/mcp"
)

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
