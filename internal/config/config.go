package config

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type ConnectionMode string

type APIVersion string

const (
	ModeReadOnly              ConnectionMode = "read_only"
	ModeWriteWithConfirmation ConnectionMode = "write_with_confirmation"

	DefaultBitrixOAuthURL = "https://oauth.bitrix.info/oauth/token"

	APIVersionAuto APIVersion = "auto"
	APIVersionV3   APIVersion = "v3"
	APIVersionV2   APIVersion = "v2"
)

func ParseConnectionMode(raw string) (ConnectionMode, error) {
	switch ConnectionMode(raw) {
	case ModeReadOnly, ModeWriteWithConfirmation:
		return ConnectionMode(raw), nil
	default:
		return "", errors.New("invalid connection mode")
	}
}

func ParseAPIVersion(raw string) (APIVersion, error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	switch trimmed {
	case "", string(APIVersionAuto):
		return APIVersionAuto, nil
	case string(APIVersionV3):
		return APIVersionV3, nil
	case string(APIVersionV2), "legacy":
		return APIVersionV2, nil
	default:
		return "", errors.New("must be 'auto', 'v3', 'v2', or 'legacy'")
	}
}

type TrustProxyConfig struct {
	Enabled   bool
	Hops      int
	Addresses []string
}

type Config struct {
	Host                           string
	Port                           int
	AppURL                         string
	DatabasePath                   string
	BitrixClientID                 string
	BitrixClientSecret             string
	BitrixPortalDomain             string
	BitrixWebhookURL               string
	BitrixOAuthURL                 string
	BitrixAPIVersion               APIVersion
	DefaultMode                    ConnectionMode
	AdminToolsEnabled              bool
	McpAuthToken                   string
	TokenEncryptionKey             string
	Transport                      string
	TrustProxy                     TrustProxyConfig
	LogLevel                       string
	OAuthStateTTLSeconds           int
	McpOAuthRequestTTLSeconds      int
	McpOAuthCodeTTLSeconds         int
	McpOAuthAccessTokenTTLSeconds  int
	McpOAuthRefreshTokenTTLSeconds int
	SchemaCacheTTLSeconds          int
	PendingOperationTTLSeconds     int
	BitrixRequestTimeoutMs         int
	BitrixMaxRetries               int
	BitrixMaxPages                 int
}

var hexKeyPattern = regexp.MustCompile(`(?i)^[a-f0-9]{64}$`)

func ParseEncryptionKey(value string) ([]byte, error) {
	if hexKeyPattern.MatchString(value) {
		decoded, err := hex.DecodeString(value)
		if err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil || len(decoded) != 32 {
		return nil, errors.New("TOKEN_ENCRYPTION_KEY must be a 32-byte base64 or 64-character hexadecimal key")
	}
	return decoded, nil
}

func NormalizeSqlitePath(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == ":memory:" || strings.HasPrefix(trimmed, "file::memory:") {
		return ":memory:"
	}
	for _, prefix := range []string{"sqlite://", "sqlite:", "file://", "file:"} {
		if strings.HasPrefix(trimmed, prefix) {
			return strings.TrimPrefix(trimmed, prefix)
		}
	}
	return trimmed
}

var webhookPattern = regexp.MustCompile(`^/rest/([0-9]+)/([a-zA-Z0-9_\-]+)/?$`)

func ParseWebhookURL(raw string, allowedPortalDomains ...string) (domain string, userID string, webhookCode string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", "", errors.New("webhook URL is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", "", "", errors.New("invalid webhook URL")
	}
	hostname := parsed.Hostname()
	isLoopback := IsLoopbackHost(hostname)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback) {
		return "", "", "", errors.New("webhook URL must use HTTPS except on localhost or a loopback address")
	}
	matches := webhookPattern.FindStringSubmatch(parsed.Path)
	if len(matches) != 3 {
		return "", "", "", errors.New("webhook URL must match format https://<domain>/rest/<user_id>/<webhook_code>[/]")
	}
	if !isLoopback {
		if parsed.Port() != "" || parsed.User != nil {
			return "", "", "", errors.New("webhook URL must not include custom port or credentials")
		}
		normDomain, domErr := NormalizePortalDomain(hostname, allowedPortalDomains)
		if domErr != nil {
			return "", "", "", domErr
		}
		return normDomain, matches[1], matches[2], nil
	}
	return parsed.Host, matches[1], matches[2], nil
}

func LoadFromOS() (*Config, error) {
	env := make(map[string]string)
	loadDotEnv(".env", env)
	for _, entry := range os.Environ() {
		if k, v, ok := strings.Cut(entry, "="); ok {
			env[k] = v
		}
	}
	return Load(env)
}

func loadDotEnv(path string, target map[string]string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := target[k]; !exists {
			target[k] = v
		}
	}
}

func Load(env map[string]string) (*Config, error) {
	var issues []string

	host := getDefault(env, "HOST", "0.0.0.0")
	port := parseIntInRange(env, "PORT", 3000, 1, 65535, &issues)

	bitrixPortalDomain := strings.TrimSpace(env["BITRIX_PORTAL_DOMAIN"])
	if bitrixPortalDomain != "" {
		if normPortal, err := ParsePortalHost(bitrixPortalDomain); err != nil {
			issues = append(issues, fmt.Sprintf("BITRIX_PORTAL_DOMAIN: %s", err.Error()))
		} else {
			bitrixPortalDomain = normPortal
		}
	}

	rawWebhookURL := strings.TrimSpace(env["BITRIX_WEBHOOK_URL"])
	var bitrixWebhookURL string
	if rawWebhookURL != "" {
		domain, userID, code, err := ParseWebhookURL(rawWebhookURL, bitrixPortalDomain)
		if err != nil {
			issues = append(issues, fmt.Sprintf("BITRIX_WEBHOOK_URL: %s", err.Error()))
		} else {
			scheme := "https"
			if strings.HasPrefix(strings.ToLower(rawWebhookURL), "http://") {
				scheme = "http"
			}
			bitrixWebhookURL = fmt.Sprintf("%s://%s/rest/%s/%s", scheme, domain, userID, code)
		}
	}

	rawAppURL := strings.TrimSpace(env["APP_URL"])
	var appURL string
	if rawAppURL != "" || bitrixWebhookURL == "" {
		validAppURL, err := validateAppURL(rawAppURL)
		if err != nil {
			issues = append(issues, fmt.Sprintf("APP_URL: %s", err.Error()))
		} else {
			appURL = validAppURL
		}
	}

	dbRaw := strings.TrimSpace(env["DATABASE_PATH"])
	if dbRaw == "" {
		dbRaw = strings.TrimSpace(env["DATABASE_URL"])
	}
	if dbRaw == "" {
		dbRaw = "./data/bitrix24-mcp.sqlite"
	}
	databasePath := NormalizeSqlitePath(dbRaw)

	bitrixClientID := strings.TrimSpace(env["BITRIX_CLIENT_ID"])
	if bitrixClientID == "" && bitrixWebhookURL == "" {
		issues = append(issues, "BITRIX_CLIENT_ID: required")
	}
	bitrixClientSecret := strings.TrimSpace(env["BITRIX_CLIENT_SECRET"])
	if bitrixClientSecret == "" && bitrixWebhookURL == "" {
		issues = append(issues, "BITRIX_CLIENT_SECRET: required")
	}

	bitrixOAuthURL := strings.TrimSpace(env["BITRIX_OAUTH_URL"])
	if bitrixOAuthURL == "" {
		bitrixOAuthURL = DefaultBitrixOAuthURL
	} else {
		parsedOAuth, err := url.Parse(bitrixOAuthURL)
		if err != nil || parsedOAuth.Scheme == "" || parsedOAuth.Host == "" {
			issues = append(issues, "BITRIX_OAUTH_URL: must be a valid URL")
		}
	}

	bitrixAPIVersion, err := ParseAPIVersion(env["BITRIX_API_VERSION"])
	if err != nil {
		issues = append(issues, fmt.Sprintf("BITRIX_API_VERSION: %s", err.Error()))
	}

	defaultModeStr := getDefault(env, "BITRIX_MCP_DEFAULT_MODE", string(ModeReadOnly))
	defaultMode, err := ParseConnectionMode(defaultModeStr)
	if err != nil {
		issues = append(issues, "BITRIX_MCP_DEFAULT_MODE: invalid mode")
	}

	adminToolsEnabled, err := parseBoolEnv(getDefault(env, "BITRIX_MCP_ADMIN_TOOLS", "false"))
	if err != nil {
		issues = append(issues, fmt.Sprintf("BITRIX_MCP_ADMIN_TOOLS: %s", err.Error()))
	}

	mcpAuthToken := env["MCP_AUTH_TOKEN"]
	if len(mcpAuthToken) < 32 {
		issues = append(issues, "MCP_AUTH_TOKEN: MCP_AUTH_TOKEN must contain at least 32 characters")
	}

	tokenEncryptionKey := strings.TrimSpace(env["TOKEN_ENCRYPTION_KEY"])
	if _, err := ParseEncryptionKey(tokenEncryptionKey); err != nil {
		issues = append(issues, fmt.Sprintf("TOKEN_ENCRYPTION_KEY: %s", err.Error()))
	}

	transport := getDefault(env, "MCP_TRANSPORT", "http")
	if transport != "http" && transport != "stdio" {
		issues = append(issues, "MCP_TRANSPORT: must be http or stdio")
	}

	trustProxy := parseTrustProxy(env["TRUST_PROXY"])

	logLevel := getDefault(env, "LOG_LEVEL", "info")
	if _, ok := logLevelPriority[logLevel]; !ok {
		issues = append(issues, "LOG_LEVEL: invalid log level")
	}

	oauthStateTTL := parsePositiveInt(env, "OAUTH_STATE_TTL_SECONDS", 600, &issues)
	mcpOAuthRequestTTL := parsePositiveInt(env, "MCP_OAUTH_REQUEST_TTL_SECONDS", 600, &issues)
	mcpOAuthCodeTTL := parsePositiveInt(env, "MCP_OAUTH_CODE_TTL_SECONDS", 300, &issues)
	mcpOAuthAccessTTL := parsePositiveInt(env, "MCP_OAUTH_ACCESS_TOKEN_TTL_SECONDS", 3600, &issues)
	mcpOAuthRefreshTTL := parsePositiveInt(env, "MCP_OAUTH_REFRESH_TOKEN_TTL_SECONDS", 2592000, &issues)
	schemaCacheTTL := parsePositiveInt(env, "SCHEMA_CACHE_TTL_SECONDS", 900, &issues)
	pendingOpTTL := parsePositiveInt(env, "PENDING_OPERATION_TTL_SECONDS", 600, &issues)
	bitrixTimeoutMs := parsePositiveInt(env, "BITRIX_REQUEST_TIMEOUT_MS", 15000, &issues)
	bitrixMaxRetries := parseIntInRange(env, "BITRIX_MAX_RETRIES", 3, 0, 8, &issues)
	bitrixMaxPages := parseIntInRange(env, "BITRIX_MAX_PAGES", 20, 1, 100, &issues)

	if len(issues) > 0 {
		return nil, fmt.Errorf("Invalid environment configuration: %s", strings.Join(issues, "; "))
	}

	return &Config{
		Host:                           host,
		Port:                           port,
		AppURL:                         strings.TrimRight(appURL, "/"),
		DatabasePath:                   databasePath,
		BitrixClientID:                 bitrixClientID,
		BitrixClientSecret:             bitrixClientSecret,
		BitrixPortalDomain:             bitrixPortalDomain,
		BitrixWebhookURL:               bitrixWebhookURL,
		BitrixOAuthURL:                 bitrixOAuthURL,
		BitrixAPIVersion:               bitrixAPIVersion,
		DefaultMode:                    defaultMode,
		AdminToolsEnabled:              adminToolsEnabled,
		McpAuthToken:                   mcpAuthToken,
		TokenEncryptionKey:             tokenEncryptionKey,
		Transport:                      transport,
		TrustProxy:                     trustProxy,
		LogLevel:                       logLevel,
		OAuthStateTTLSeconds:           oauthStateTTL,
		McpOAuthRequestTTLSeconds:      mcpOAuthRequestTTL,
		McpOAuthCodeTTLSeconds:         mcpOAuthCodeTTL,
		McpOAuthAccessTokenTTLSeconds:  mcpOAuthAccessTTL,
		McpOAuthRefreshTokenTTLSeconds: mcpOAuthRefreshTTL,
		SchemaCacheTTLSeconds:          schemaCacheTTL,
		PendingOperationTTLSeconds:     pendingOpTTL,
		BitrixRequestTimeoutMs:         bitrixTimeoutMs,
		BitrixMaxRetries:               bitrixMaxRetries,
		BitrixMaxPages:                 bitrixMaxPages,
	}, nil
}

func validateAppURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid URL")
	}
	isLoopback := IsLoopbackHost(parsed.Hostname())
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback) {
		return "", errors.New("URL must use HTTPS except on localhost or a loopback address")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("APP_URL must be an origin without a path, query, or fragment")
	}
	return raw, nil
}

func getDefault(env map[string]string, key, fallback string) string {
	if v, ok := env[key]; ok && v != "" {
		return v
	}
	return fallback
}

func parseBoolEnv(val string) (bool, error) {
	switch val {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean value %q", val)
	}
}

func parseTrustProxy(raw string) TrustProxyConfig {
	val := strings.TrimSpace(raw)
	if val == "" || val == "false" || val == "0" || val == "no" {
		return TrustProxyConfig{Enabled: false}
	}
	if val == "true" || val == "yes" {
		return TrustProxyConfig{Enabled: true, Hops: 1}
	}
	if hops, err := strconv.Atoi(val); err == nil && hops > 0 {
		return TrustProxyConfig{Enabled: true, Hops: hops}
	}
	var addrs []string
	for _, part := range strings.Split(val, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			addrs = append(addrs, trimmed)
		}
	}
	return TrustProxyConfig{Enabled: len(addrs) > 0, Addresses: addrs}
}

func parsePositiveInt(env map[string]string, key string, fallback int, issues *[]string) int {
	return parseIntInRange(env, key, fallback, 1, math.MaxInt, issues)
}

func parseIntInRange(env map[string]string, key string, fallback, minVal, maxVal int, issues *[]string) int {
	raw, ok := env[key]
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v < minVal || v > maxVal {
		*issues = append(*issues, fmt.Sprintf("%s: must be an integer between %d and %d", key, minVal, maxVal))
		return fallback
	}
	return v
}
