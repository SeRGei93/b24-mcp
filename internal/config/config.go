package config

import (
	"bufio"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ConnectionMode string

const (
	ModeReadOnly              ConnectionMode = "read_only"
	ModeWriteWithConfirmation ConnectionMode = "write_with_confirmation"
)

func ParseConnectionMode(raw string) (ConnectionMode, error) {
	switch ConnectionMode(raw) {
	case ModeReadOnly, ModeWriteWithConfirmation:
		return ConnectionMode(raw), nil
	default:
		return "", errors.New("invalid connection mode")
	}
}

type TrustProxyConfig struct {
	Enabled   bool
	Hops      int
	Addresses []string
}

type Config struct {
	NodeEnv                        string
	Host                           string
	Port                           int
	AppURL                         string
	DatabasePath                   string
	BitrixClientID                 string
	BitrixClientSecret             string
	BitrixPortalDomain             string
	DefaultMode                    ConnectionMode
	AdminToolsEnabled              bool
	McpAuthToken                   string
	TokenEncryptionKey             string
	EncryptionKey                  []byte
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

	nodeEnv := getDefault(env, "NODE_ENV", "development")
	switch nodeEnv {
	case "development", "test", "production":
	default:
		issues = append(issues, "NODE_ENV: must be development, test, or production")
	}

	host := getDefault(env, "HOST", "0.0.0.0")
	port := parseIntInRange(env, "PORT", 3000, 1, 65535, &issues)

	rawAppURL := strings.TrimSpace(env["APP_URL"])
	appURL, err := validateAppURL(rawAppURL)
	if err != nil {
		issues = append(issues, fmt.Sprintf("APP_URL: %s", err.Error()))
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
	if bitrixClientID == "" {
		issues = append(issues, "BITRIX_CLIENT_ID: required")
	}
	bitrixClientSecret := strings.TrimSpace(env["BITRIX_CLIENT_SECRET"])
	if bitrixClientSecret == "" {
		issues = append(issues, "BITRIX_CLIENT_SECRET: required")
	}

	bitrixPortalDomain := strings.TrimSpace(env["BITRIX_PORTAL_DOMAIN"])

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
	encryptionKey, err := ParseEncryptionKey(tokenEncryptionKey)
	if err != nil {
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
		NodeEnv:                        nodeEnv,
		Host:                           host,
		Port:                           port,
		AppURL:                         strings.TrimRight(appURL, "/"),
		DatabasePath:                   databasePath,
		BitrixClientID:                 bitrixClientID,
		BitrixClientSecret:             bitrixClientSecret,
		BitrixPortalDomain:             bitrixPortalDomain,
		DefaultMode:                    defaultMode,
		AdminToolsEnabled:              adminToolsEnabled,
		McpAuthToken:                   mcpAuthToken,
		TokenEncryptionKey:             tokenEncryptionKey,
		EncryptionKey:                  encryptionKey,
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
	hostname := parsed.Hostname()
	isLoopback := hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1"
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

var redactedKeys = map[string]struct{}{
	"access_token":       {},
	"refresh_token":      {},
	"accessToken":        {},
	"refreshToken":       {},
	"authorization":      {},
	"client_secret":      {},
	"bitrixClientSecret": {},
	"mcpAuthToken":       {},
	"tokenEncryptionKey": {},
	"code":               {},
	"code_verifier":      {},
	"token":              {},
	"auth":               {},
}

func RedactValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return redactMap(v, func(val any) any { return RedactValue(val) })
	case map[string]string:
		return redactMap(v, func(val string) any { return val })
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = RedactValue(item)
		}
		return out
	default:
		return value
	}
}

func redactMap[V any](in map[string]V, transform func(V) any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if _, ok := redactedKeys[k]; ok {
			out[k] = "[REDACTED]"
		} else {
			out[k] = transform(v)
		}
	}
	return out
}

var logLevelPriority = map[string]int{
	"trace":  10,
	"debug":  20,
	"info":   30,
	"warn":   40,
	"error":  50,
	"fatal":  60,
	"silent": 100,
}

type Logger struct {
	mu       sync.Mutex
	out      io.Writer
	minLevel int
}

func NewLogger(out io.Writer, level string) *Logger {
	prio, ok := logLevelPriority[level]
	if !ok {
		prio = logLevelPriority["info"]
	}
	return &Logger{out: out, minLevel: prio}
}

func StderrLogger(level string) *Logger {
	return NewLogger(os.Stderr, level)
}

func (l *Logger) Debug(message string, fields map[string]any) {
	l.write("debug", message, fields)
}

func (l *Logger) Info(message string, fields map[string]any) {
	l.write("info", message, fields)
}

func (l *Logger) Warn(message string, fields map[string]any) {
	l.write("warn", message, fields)
}

func (l *Logger) Error(message string, fields map[string]any) {
	l.write("error", message, fields)
}

func (l *Logger) write(level, message string, fields map[string]any) {
	if l == nil || l.out == nil {
		return
	}
	if logLevelPriority[level] < l.minLevel {
		return
	}
	payload := map[string]any{
		"level":   level,
		"time":    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"message": message,
	}
	if fields != nil {
		redacted, _ := RedactValue(fields).(map[string]any)
		for k, v := range redacted {
			payload[k] = v
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.out.Write(append(encoded, '\n'))
}

func IsValidBearerToken(authorization, expectedToken string) bool {
	if !strings.HasPrefix(authorization, "Bearer ") || expectedToken == "" {
		return false
	}
	supplied := authorization[len("Bearer "):]
	return IsValidSecret(supplied, expectedToken)
}

func IsValidSecret(supplied, expected string) bool {
	if len(supplied) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) == 1
}

type AppError struct {
	Message    string
	Code       string
	StatusCode int
	OAuthCode  string
	Cause      error
}

func (e *AppError) Error() string {
	return e.Message
}

func (e *AppError) Unwrap() error {
	return e.Cause
}

func NewAppError(message, code string, statusCode int) *AppError {
	if statusCode == 0 {
		statusCode = 400
	}
	return &AppError{Message: message, Code: code, StatusCode: statusCode}
}

func NewNotFoundError(message string) *AppError {
	return &AppError{Message: message, Code: "NOT_FOUND", StatusCode: 404}
}

func NewPolicyError(message string) *AppError {
	return &AppError{Message: message, Code: "POLICY_DENIED", StatusCode: 403}
}

func NewAuthenticationError(message string) *AppError {
	if message == "" {
		message = "Unauthorized"
	}
	return &AppError{Message: message, Code: "UNAUTHORIZED", StatusCode: 401}
}

func SafeErrorMessage(err error) string {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr.Message
	}
	return "An unexpected internal error occurred"
}

