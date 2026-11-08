package bitrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/google/uuid"
)

var (
	methodRegex    = regexp.MustCompile(`(?i)^[a-z][a-z0-9_.]+$`)
	retryableCodes = []string{"QUERY_LIMIT_EXCEEDED", "OPERATION_TIME_LIMIT", "TOO_MANY_REQUESTS"}
	tokenCodes     = []string{"expired_token", "INVALID_TOKEN", "invalid_token"}
)

const BitrixBatchLimit = 50

type NamedBatchCommand struct {
	Key    string
	Method string
	Params map[string]any
}

type RequestOptions struct {
	APIVersion     string
	IdempotencyKey string
	Headers        http.Header
}

type RequestOption func(*RequestOptions)

func WithAPIVersion(version string) RequestOption {
	return func(o *RequestOptions) {
		o.APIVersion = version
	}
}

func WithIdempotencyKey(key string) RequestOption {
	return func(o *RequestOptions) {
		o.IdempotencyKey = key
	}
}

func WithHeader(key, value string) RequestOption {
	return func(o *RequestOptions) {
		if o.Headers == nil {
			o.Headers = make(http.Header)
		}
		o.Headers.Set(key, value)
	}
}

type BitrixTime struct {
	Start            float64 `json:"start,omitempty"`
	Finish           float64 `json:"finish,omitempty"`
	Duration         float64 `json:"duration,omitempty"`
	Processing       float64 `json:"processing,omitempty"`
	DateStart        string  `json:"date_start,omitempty"`
	DateFinish       string  `json:"date_finish,omitempty"`
	Operating        float64 `json:"operating,omitempty"`
	OperatingResetAt int64   `json:"operating_reset_at,omitempty"`
}

func (t *BitrixTime) UnmarshalJSON(data []byte) error {
	var raw struct {
		Start            float64         `json:"start"`
		Finish           float64         `json:"finish"`
		Duration         float64         `json:"duration"`
		Processing       float64         `json:"processing"`
		DateStart        string          `json:"date_start"`
		DateFinish       string          `json:"date_finish"`
		Operating        float64         `json:"operating"`
		OperatingResetAt json.RawMessage `json:"operating_reset_at"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.Start = raw.Start
	t.Finish = raw.Finish
	t.Duration = raw.Duration
	t.Processing = raw.Processing
	t.DateStart = raw.DateStart
	t.DateFinish = raw.DateFinish
	t.Operating = raw.Operating
	if len(raw.OperatingResetAt) > 0 {
		var n float64
		if err := json.Unmarshal(raw.OperatingResetAt, &n); err == nil {
			t.OperatingResetAt = int64(n)
		} else {
			var s string
			if err := json.Unmarshal(raw.OperatingResetAt, &s); err == nil {
				if parsed, err := strconv.ParseInt(s, 10, 64); err == nil {
					t.OperatingResetAt = parsed
				}
			}
		}
	}
	return nil
}

type BitrixErrorDetail struct {
	Code       string                         `json:"code,omitempty"`
	Message    string                         `json:"message,omitempty"`
	Validation []config.BitrixValidationError `json:"validation,omitempty"`
}

type BitrixResponse struct {
	Result           json.RawMessage    `json:"result"`
	Error            string             `json:"-"`
	ErrorDescription string             `json:"error_description,omitempty"`
	ErrorDetail      *BitrixErrorDetail `json:"-"`
	Total            *int               `json:"total,omitempty"`
	Next             *int               `json:"next,omitempty"`
	Time             *BitrixTime        `json:"time,omitempty"`
}

func (r *BitrixResponse) UnmarshalJSON(data []byte) error {
	type rawResponse struct {
		Result           json.RawMessage `json:"result"`
		Error            json.RawMessage `json:"error,omitempty"`
		ErrorDescription string          `json:"error_description,omitempty"`
		Total            *int            `json:"total,omitempty"`
		Next             *int            `json:"next,omitempty"`
		Time             *BitrixTime     `json:"time,omitempty"`
	}
	var raw rawResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.Result = raw.Result
	r.ErrorDescription = raw.ErrorDescription
	r.Total = raw.Total
	r.Next = raw.Next
	r.Time = raw.Time

	if len(raw.Error) > 0 && string(raw.Error) != "null" {
		var errStr string
		if err := json.Unmarshal(raw.Error, &errStr); err == nil {
			r.Error = errStr
			r.ErrorDetail = &BitrixErrorDetail{
				Code:    errStr,
				Message: raw.ErrorDescription,
			}
		} else {
			var detail BitrixErrorDetail
			if err := json.Unmarshal(raw.Error, &detail); err == nil {
				r.ErrorDetail = &detail
				r.Error = detail.Code
				if r.ErrorDescription == "" {
					r.ErrorDescription = detail.Message
				}
			} else {
				r.Error = string(raw.Error)
			}
		}
	}
	return nil
}

type batchResultPayload struct {
	Result      FlexMap[json.RawMessage] `json:"result"`
	ResultError FlexMap[json.RawMessage] `json:"result_error"`
	ResultTotal FlexMap[int]             `json:"result_total"`
	ResultNext  FlexMap[int]             `json:"result_next"`
}

type v3BatchItem struct {
	Method string         `json:"method"`
	Query  map[string]any `json:"query,omitempty"`
}

func appendBatchParams(search *url.Values, prefix string, value any) {
	if value == nil {
		return
	}
	switch v := value.(type) {
	case []any:
		for i, item := range v {
			appendBatchParams(search, fmt.Sprintf("%s[%d]", prefix, i), item)
		}
	case []string:
		for i, item := range v {
			appendBatchParams(search, fmt.Sprintf("%s[%d]", prefix, i), item)
		}
	case []int:
		for i, item := range v {
			appendBatchParams(search, fmt.Sprintf("%s[%d]", prefix, i), item)
		}
	case map[string]any:
		for k, item := range v {
			appendBatchParams(search, fmt.Sprintf("%s[%s]", prefix, k), item)
		}
	case map[string]string:
		for k, item := range v {
			appendBatchParams(search, fmt.Sprintf("%s[%s]", prefix, k), item)
		}
	case bool:
		search.Add(prefix, strconv.FormatBool(v))
	case int:
		search.Add(prefix, strconv.Itoa(v))
	case int64:
		search.Add(prefix, strconv.FormatInt(v, 10))
	case float64:
		search.Add(prefix, strconv.FormatFloat(v, 'f', -1, 64))
	case string:
		search.Add(prefix, v)
	default:
		search.Add(prefix, fmt.Sprintf("%v", v))
	}
}

func BuildBatchCommand(method string, params map[string]any) (string, error) {
	if !methodRegex.MatchString(method) {
		return "", config.NewAppError("Invalid Bitrix REST method name", "INVALID_METHOD", 400)
	}
	if len(params) == 0 {
		return method, nil
	}
	search := url.Values{}
	for k, v := range params {
		appendBatchParams(&search, k, v)
	}
	query := search.Encode()
	if query == "" {
		return method, nil
	}
	return method + "?" + query, nil
}

func buildEndpoint(clientEndpoint string, isWebhook bool, apiVersion string, method string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(clientEndpoint))
	if err != nil {
		return "", err
	}
	origin := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	cleanPath := strings.Trim(u.Path, "/")

	if isWebhook {
		parts := strings.Split(cleanPath, "/")
		restIdx := -1
		for i, part := range parts {
			if part == "rest" {
				restIdx = i
				break
			}
		}
		if restIdx == -1 || restIdx+2 >= len(parts) {
			return "", fmt.Errorf("invalid webhook endpoint: %s", clientEndpoint)
		}
		prefix := ""
		if restIdx > 0 {
			prefix = "/" + strings.Join(parts[:restIdx], "/")
		}
		userID := parts[restIdx+1]
		code := parts[restIdx+2]

		if apiVersion == config.APIVersionV3 {
			return fmt.Sprintf("%s%s/rest/api/%s/%s/%s", origin, prefix, userID, code, method), nil
		}
		return fmt.Sprintf("%s%s/rest/%s/%s/%s.json", origin, prefix, userID, code, method), nil
	}

	parts := strings.Split(cleanPath, "/")
	restIdx := -1
	for i, part := range parts {
		if part == "rest" {
			restIdx = i
			break
		}
	}
	prefix := ""
	if restIdx > 0 {
		prefix = "/" + strings.Join(parts[:restIdx], "/")
	}

	if apiVersion == config.APIVersionV3 {
		return fmt.Sprintf("%s%s/rest/api/%s", origin, prefix, method), nil
	}
	return fmt.Sprintf("%s%s/rest/%s.json", origin, prefix, method), nil
}

type RestClient struct {
	cfg        *config.Config
	tokens     *TokenManager
	logger     *config.Logger
	httpClient *http.Client
}

func NewRestClient(cfg *config.Config, tokens *TokenManager, logger *config.Logger, httpClient *http.Client) *RestClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: time.Duration(cfg.BitrixRequestTimeoutMs) * time.Millisecond}
	}
	return &RestClient{
		cfg:        cfg,
		tokens:     tokens,
		logger:     logger,
		httpClient: httpClient,
	}
}

func isVersionFallbackError(appErr *config.AppError) bool {
	if appErr == nil {
		return false
	}
	if appErr.StatusCode == 404 {
		return true
	}
	switch appErr.Code {
	case "ERROR_METHOD_NOT_FOUND",
		"BITRIX_REST_V3_EXCEPTION_UNKNOWNMETHOD",
		"BITRIX_REST_V3_EXCEPTION_METHODNOTFOUNDEXCEPTION",
		"HTTP_404":
		return true
	}
	return false
}

func isLegacyOnlyMethod(method string) bool {
	if strings.HasPrefix(method, "crm.item.") ||
		strings.HasPrefix(method, "crm.type.") ||
		strings.HasPrefix(method, "crm.status.") ||
		strings.HasPrefix(method, "crm.category.") ||
		strings.HasPrefix(method, "user.") ||
		strings.HasPrefix(method, "app.") ||
		method == "profile" {
		return true
	}
	return false
}

func (c *RestClient) Call(ctx context.Context, connectionID string, method string, params map[string]any, opts ...RequestOption) (*BitrixResponse, error) {
	if !methodRegex.MatchString(method) {
		return nil, config.NewAppError("Invalid Bitrix REST method name", "INVALID_METHOD", 400)
	}
	if params == nil {
		params = make(map[string]any)
	}
	var options RequestOptions
	for _, opt := range opts {
		opt(&options)
	}

	targetVersion := options.APIVersion
	if targetVersion == "" {
		targetVersion = c.cfg.BitrixAPIVersion
	}
	if targetVersion == "" {
		targetVersion = config.APIVersionAuto
	}

	if targetVersion == config.APIVersionAuto && isLegacyOnlyMethod(method) {
		targetVersion = config.APIVersionV2
	}

	if targetVersion != config.APIVersionAuto {
		return c.callWithVersion(ctx, connectionID, method, params, targetVersion, options)
	}

	// Auto mode: check schema_cache
	if st := c.tokens.Store(); st != nil {
		var cachedVersion string
		found, err := st.GetSchemaCache(ctx, connectionID, "api_version", c.cfg.SchemaCacheTTLSeconds, &cachedVersion)
		if err == nil && found && cachedVersion == config.APIVersionV2 {
			return c.callWithVersion(ctx, connectionID, method, params, config.APIVersionV2, options)
		}
	}

	// Try v3
	resp, err := c.callWithVersion(ctx, connectionID, method, params, config.APIVersionV3, options)
	if err != nil {
		var appErr *config.AppError
		if errors.As(err, &appErr) && isVersionFallbackError(appErr) {
			if st := c.tokens.Store(); st != nil {
				_ = st.SetSchemaCache(ctx, connectionID, "api_version", config.APIVersionV2)
			}
			if c.logger != nil {
				c.logger.Warn("Bitrix24 REST 3.0 endpoint unavailable, falling back to Legacy REST v2", map[string]any{
					"connectionId": connectionID,
					"method":       method,
					"statusCode":   appErr.StatusCode,
					"errorCode":    appErr.Code,
				})
			}
			return c.callWithVersion(ctx, connectionID, method, params, config.APIVersionV2, options)
		}
		return nil, err
	}
	return resp, nil
}

func (c *RestClient) callWithVersion(
	ctx context.Context,
	connectionID string,
	method string,
	params map[string]any,
	version string,
	options RequestOptions,
) (*BitrixResponse, error) {
	requestID := uuid.NewString()
	refreshedForRequest := false
	forceRefreshNext := false
	var staleAccessToken string

	for attempt := 0; attempt <= c.cfg.BitrixMaxRetries; attempt++ {
		credentials, err := c.tokens.Get(ctx, connectionID, forceRefreshNext, staleAccessToken)
		if err != nil {
			return nil, err
		}
		forceRefreshNext = false
		staleAccessToken = ""

		isWebhook := credentials.Connection.IsWebhook()
		endpoint, err := buildEndpoint(credentials.Connection.ClientEndpoint, isWebhook, version, method)
		if err != nil {
			return nil, err
		}

		reqBody := make(map[string]any, len(params)+1)
		for k, v := range params {
			reqBody[k] = v
		}
		if !isWebhook {
			reqBody["auth"] = credentials.AccessToken
		}

		payloadBytes, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}

		if c.logger != nil {
			c.logger.Debug("Calling Bitrix24 REST API", map[string]any{
				"requestId":    requestID,
				"connectionId": connectionID,
				"method":       method,
				"attempt":      attempt,
				"version":      version,
				"isWebhook":    isWebhook,
			})
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json")
		if options.IdempotencyKey != "" {
			httpReq.Header.Set("Idempotency-Key", options.IdempotencyKey)
		}
		if options.Headers != nil {
			for k, v := range options.Headers {
				httpReq.Header[k] = v
			}
		}

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			if attempt < c.cfg.BitrixMaxRetries {
				if backoffErr := sleepBackoff(ctx, attempt, "", 0); backoffErr != nil {
					return nil, backoffErr
				}
				continue
			}
			return nil, config.NewAppError("Bitrix24 REST request failed at the network layer", "NETWORK_ERROR", 502)
		}

		respBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			if attempt < c.cfg.BitrixMaxRetries {
				if backoffErr := sleepBackoff(ctx, attempt, "", 0); backoffErr != nil {
					return nil, backoffErr
				}
				continue
			}
			return nil, config.NewAppError("Failed to read Bitrix24 response", "NETWORK_ERROR", 502)
		}

		var payload BitrixResponse
		_ = json.Unmarshal(respBytes, &payload)

		code := payload.Error
		if code == "" && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
			code = fmt.Sprintf("HTTP_%d", resp.StatusCode)
		}

		if code == "" && len(payload.Result) > 0 {
			return &payload, nil
		}
		if code == "" {
			code = "INVALID_RESPONSE"
		}

		if !isWebhook && code != "" && slices.Contains(tokenCodes, code) && !refreshedForRequest {
			refreshedForRequest = true
			forceRefreshNext = true
			staleAccessToken = credentials.AccessToken
			continue
		}

		var operatingResetAt int64
		if payload.Time != nil {
			operatingResetAt = payload.Time.OperatingResetAt
		}
		retryHeader := resp.Header.Get("Retry-After")
		if (resp.StatusCode == 429 || resp.StatusCode >= 500 || (code != "" && slices.Contains(retryableCodes, code))) && attempt < c.cfg.BitrixMaxRetries {
			if c.logger != nil {
				c.logger.Warn("Retrying Bitrix24 request", map[string]any{
					"requestId":        requestID,
					"connectionId":     connectionID,
					"method":           method,
					"attempt":          attempt,
					"bitrixCode":       code,
					"operatingResetAt": operatingResetAt,
				})
			}
			if backoffErr := sleepBackoff(ctx, attempt, retryHeader, operatingResetAt); backoffErr != nil {
				return nil, backoffErr
			}
			continue
		}

		if c.logger != nil {
			c.logger.Warn("Bitrix24 REST request rejected", map[string]any{
				"requestId":    requestID,
				"connectionId": connectionID,
				"method":       method,
				"bitrixCode":   code,
				"statusCode":   resp.StatusCode,
			})
		}
		status := resp.StatusCode
		if status < 400 {
			status = 502
		}

		errMsg := fmt.Sprintf("Bitrix24 rejected %s (%s)", method, code)
		if payload.ErrorDetail != nil && len(payload.ErrorDetail.Validation) > 0 {
			var vMsgs []string
			for _, v := range payload.ErrorDetail.Validation {
				vMsgs = append(vMsgs, fmt.Sprintf("%s: %s", v.Field, v.Message))
			}
			errMsg = fmt.Sprintf("%s: %s", errMsg, strings.Join(vMsgs, "; "))
			return nil, &config.AppError{
				Message:    errMsg,
				Code:       code,
				StatusCode: status,
				Validation: payload.ErrorDetail.Validation,
			}
		}
		return nil, config.NewAppError(errMsg, code, status)
	}

	return nil, config.NewAppError("Bitrix24 request exhausted its retry budget", "RETRY_EXHAUSTED", 502)
}

func (c *RestClient) CallBatch(
	ctx context.Context,
	connectionID string,
	commands []NamedBatchCommand,
	halt bool,
	opts ...RequestOption,
) (map[string]json.RawMessage, error) {
	if len(commands) == 0 {
		return make(map[string]json.RawMessage), nil
	}

	var options RequestOptions
	for _, opt := range opts {
		opt(&options)
	}

	targetVersion := options.APIVersion
	if targetVersion == "" {
		targetVersion = c.cfg.BitrixAPIVersion
	}
	if targetVersion == "" {
		targetVersion = config.APIVersionAuto
	}

	if targetVersion == config.APIVersionAuto {
		for _, cmd := range commands {
			if isLegacyOnlyMethod(cmd.Method) {
				targetVersion = config.APIVersionV2
				break
			}
		}
	}

	if targetVersion == config.APIVersionAuto {
		if st := c.tokens.Store(); st != nil {
			var cachedVersion string
			found, err := st.GetSchemaCache(ctx, connectionID, "api_version", c.cfg.SchemaCacheTTLSeconds, &cachedVersion)
			if err == nil && found && cachedVersion == config.APIVersionV2 {
				targetVersion = config.APIVersionV2
			}
		}
	}

	if targetVersion == config.APIVersionV3 {
		res, err := c.callBatchV3(ctx, connectionID, commands, halt, options)
		if err == nil {
			return res, nil
		}
		var appErr *config.AppError
		if options.APIVersion == "" && errors.As(err, &appErr) && isVersionFallbackError(appErr) {
			if st := c.tokens.Store(); st != nil {
				_ = st.SetSchemaCache(ctx, connectionID, "api_version", config.APIVersionV2)
			}
			return c.callBatchV2(ctx, connectionID, commands, halt, options)
		}
		return nil, err
	}

	if targetVersion == config.APIVersionAuto {
		res, err := c.callBatchV3(ctx, connectionID, commands, halt, options)
		if err == nil {
			return res, nil
		}
		var appErr *config.AppError
		if errors.As(err, &appErr) && isVersionFallbackError(appErr) {
			if st := c.tokens.Store(); st != nil {
				_ = st.SetSchemaCache(ctx, connectionID, "api_version", config.APIVersionV2)
			}
			return c.callBatchV2(ctx, connectionID, commands, halt, options)
		}
		return nil, err
	}

	return c.callBatchV2(ctx, connectionID, commands, halt, options)
}

func (c *RestClient) callBatchV3(
	ctx context.Context,
	connectionID string,
	commands []NamedBatchCommand,
	halt bool,
	options RequestOptions,
) (map[string]json.RawMessage, error) {
	combined := make(map[string]json.RawMessage)

	for offset := 0; offset < len(commands); offset += BitrixBatchLimit {
		end := offset + BitrixBatchLimit
		if end > len(commands) {
			end = len(commands)
		}
		chunk := commands[offset:end]

		items := make([]v3BatchItem, 0, len(chunk))
		for _, cmd := range chunk {
			items = append(items, v3BatchItem{
				Method: cmd.Method,
				Query:  cmd.Params,
			})
		}

		payloadBytes, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}

		credentials, err := c.tokens.Get(ctx, connectionID, false, "")
		if err != nil {
			return nil, err
		}
		isWebhook := credentials.Connection.IsWebhook()
		endpoint, err := buildEndpoint(credentials.Connection.ClientEndpoint, isWebhook, config.APIVersionV3, "batch")
		if err != nil {
			return nil, err
		}
		if !isWebhook {
			u, _ := url.Parse(endpoint)
			q := u.Query()
			q.Set("auth", credentials.AccessToken)
			u.RawQuery = q.Encode()
			endpoint = u.String()
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json")
		if options.IdempotencyKey != "" {
			httpReq.Header.Set("Idempotency-Key", options.IdempotencyKey)
		}
		if options.Headers != nil {
			for k, v := range options.Headers {
				httpReq.Header[k] = v
			}
		}

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			return nil, config.NewAppError("Bitrix24 REST batch request failed at the network layer", "NETWORK_ERROR", 502)
		}
		respBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, config.NewAppError("Failed to read Bitrix24 batch response", "NETWORK_ERROR", 502)
		}

		var bitrixResp BitrixResponse
		_ = json.Unmarshal(respBytes, &bitrixResp)

		if resp.StatusCode < 200 || resp.StatusCode >= 300 || (bitrixResp.Error != "" && bitrixResp.Error != "INVALID_RESPONSE") {
			status := resp.StatusCode
			if status < 400 {
				status = 502
			}
			code := bitrixResp.Error
			if code == "" {
				code = fmt.Sprintf("HTTP_%d", resp.StatusCode)
			}
			return nil, config.NewAppError(fmt.Sprintf("Bitrix24 batch failed (%s)", code), code, status)
		}

		var resList []json.RawMessage
		if err := json.Unmarshal(bitrixResp.Result, &resList); err == nil && len(resList) == len(chunk) {
			for i, cmd := range chunk {
				combined[cmd.Key] = resList[i]
			}
		} else {
			var batchPayload batchResultPayload
			if err := json.Unmarshal(bitrixResp.Result, &batchPayload); err == nil && len(batchPayload.Result) > 0 {
				for _, cmd := range chunk {
					if val, ok := batchPayload.Result[cmd.Key]; ok {
						combined[cmd.Key] = val
					}
				}
			}
		}
	}
	return combined, nil
}

func (c *RestClient) callBatchV2(
	ctx context.Context,
	connectionID string,
	commands []NamedBatchCommand,
	halt bool,
	options RequestOptions,
) (map[string]json.RawMessage, error) {
	combined := make(map[string]json.RawMessage)
	for offset := 0; offset < len(commands); offset += BitrixBatchLimit {
		end := offset + BitrixBatchLimit
		if end > len(commands) {
			end = len(commands)
		}
		chunk := commands[offset:end]

		cmdMap := make(map[string]any, len(chunk))
		for _, cmd := range chunk {
			serialized, err := BuildBatchCommand(cmd.Method, cmd.Params)
			if err != nil {
				return nil, err
			}
			cmdMap[cmd.Key] = serialized
		}

		haltVal := 0
		if halt {
			haltVal = 1
		}

		resp, err := c.callWithVersion(ctx, connectionID, "batch", map[string]any{
			"halt": haltVal,
			"cmd":  cmdMap,
		}, config.APIVersionV2, options)
		if err != nil {
			return nil, err
		}

		var batchPayload batchResultPayload
		if err := json.Unmarshal(resp.Result, &batchPayload); err != nil {
			return nil, err
		}

		if len(batchPayload.ResultError) > 0 {
			for failedKey, errVal := range batchPayload.ResultError {
				var errObj struct {
					Error            string `json:"error"`
					ErrorDescription string `json:"error_description"`
				}
				_ = json.Unmarshal(errVal, &errObj)
				errCode := errObj.Error
				if errCode == "" {
					errCode = "BATCH_COMMAND_ERROR"
				}
				return nil, config.NewAppError(
					fmt.Sprintf("Bitrix24 batch command '%s' failed (%s)", failedKey, errCode),
					errCode,
					502,
				)
			}
		}

		for _, cmd := range chunk {
			if val, ok := batchPayload.Result[cmd.Key]; ok {
				combined[cmd.Key] = val
			}
		}
	}

	return combined, nil
}

// CollectPages fetches the first page (start=0) of a paginated Bitrix24 REST method
// and, if additional pages exist within c.cfg.BitrixMaxPages and the optional limit,
// fetches subsequent pages via CallBatch (when total is known) or sequential Call fallback.
func (c *RestClient) CollectPages(
	ctx context.Context,
	connectionID string,
	method string,
	params map[string]any,
	limit int,
	extract func(raw json.RawMessage) []map[string]any,
) ([]map[string]any, *int, error) {
	firstResp, err := c.Call(ctx, connectionID, method, clonePageParams(params, 0))
	if err != nil {
		return nil, nil, err
	}

	values := extract(firstResp.Result)
	if values == nil {
		values = []map[string]any{}
	}
	total := firstResp.Total

	if firstResp.Next != nil && c.cfg.BitrixMaxPages > 1 && (limit <= 0 || len(values) < limit) {
		if startVal, ok := params["start"]; ok && ToInt(startVal) < 0 {
			return values, total, nil
		}
		if total != nil && *total > len(values) {
			targetCount := *total
			if limit > 0 && limit < targetCount {
				targetCount = limit
			}
			maxCount := c.cfg.BitrixMaxPages * BitrixBatchLimit
			if targetCount > maxCount {
				targetCount = maxCount
			}
			var commands []NamedBatchCommand
			var keys []string
			page := 1
			for start := *firstResp.Next; start < targetCount && page < c.cfg.BitrixMaxPages; start += BitrixBatchLimit {
				key := fmt.Sprintf("p_%d", start)
				keys = append(keys, key)
				commands = append(commands, NamedBatchCommand{
					Key:    key,
					Method: method,
					Params: clonePageParams(params, start),
				})
				page++
			}
			if len(commands) > 0 {
				batchResult, bErr := c.CallBatch(ctx, connectionID, commands, false)
				if bErr != nil {
					return nil, nil, bErr
				}
				for _, k := range keys {
					if raw, ok := batchResult[k]; ok {
						values = append(values, extract(raw)...)
					}
				}
			}
		} else {
			start := *firstResp.Next
			for page := 1; page < c.cfg.BitrixMaxPages && (limit <= 0 || len(values) < limit); page++ {
				resp, rErr := c.Call(ctx, connectionID, method, clonePageParams(params, start))
				if rErr != nil {
					return nil, nil, rErr
				}
				values = append(values, extract(resp.Result)...)
				if resp.Next == nil {
					break
				}
				start = *resp.Next
			}
		}
	}

	return values, total, nil
}

func clonePageParams(params map[string]any, start int) map[string]any {
	pageParams := make(map[string]any, len(params)+1)
	for k, v := range params {
		pageParams[k] = v
	}
	if existingStart, ok := params["start"]; ok && ToInt(existingStart) < 0 {
		pageParams["start"] = existingStart
	} else {
		pageParams["start"] = start
	}
	return pageParams
}

func sleepBackoff(ctx context.Context, attempt int, retryAfter string, operatingResetAt int64) error {
	var (
		delay        time.Duration
		hasDelay     bool
		maxRetryWait = 10 * time.Second
	)
	if operatingResetAt > 0 {
		now := time.Now().Unix()
		diff := operatingResetAt - now
		if diff > 0 {
			delay = time.Duration(diff) * time.Second
			if delay > maxRetryWait {
				delay = maxRetryWait
			}
			hasDelay = true
		}
	}
	if !hasDelay && retryAfter != "" {
		if sec, err := strconv.Atoi(retryAfter); err == nil && sec >= 0 {
			delay = time.Duration(sec) * time.Second
			if delay > maxRetryWait {
				delay = maxRetryWait
			}
			hasDelay = true
		}
	}
	if !hasDelay {
		base := 250 * math.Pow(2, float64(attempt))
		jitter := rand.Float64() * 200
		totalMs := math.Min(base+jitter, 5000)
		delay = time.Duration(totalMs) * time.Millisecond
	}
	if delay <= 0 {
		return ctx.Err()
	}
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
