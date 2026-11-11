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
	"sync"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/google/uuid"
)

var (
	methodRegex    = regexp.MustCompile(`(?i)^[a-z][a-z0-9_.]+$`)
	retryableCodes = []string{"QUERY_LIMIT_EXCEEDED", "OPERATION_TIME_LIMIT", "TOO_MANY_REQUESTS"}
	tokenCodes     = []string{"expired_token", "INVALID_TOKEN", "invalid_token"}
)

const (
	BitrixBatchLimit          = 50
	operatingHighWaterSeconds = 400.0
	operatingPacingDelay      = 200 * time.Millisecond
)

type NamedBatchCommand struct {
	Key    string
	Method string
	Params map[string]any
}

type RequestOptions struct {
	APIVersion        config.APIVersion
	IdempotencyKey    string
	IgnoreBatchErrors bool
}

type RequestOption func(*RequestOptions)

func WithAPIVersion(version config.APIVersion) RequestOption {
	return func(o *RequestOptions) {
		o.APIVersion = version
	}
}

func WithIdempotencyKey(key string) RequestOption {
	return func(o *RequestOptions) {
		o.IdempotencyKey = key
	}
}

func WithIgnoreBatchErrors() RequestOption {
	return func(o *RequestOptions) {
		o.IgnoreBatchErrors = true
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

func buildEndpoint(clientEndpoint string, isWebhook bool, apiVersion config.APIVersion, method string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(clientEndpoint))
	if err != nil {
		return "", err
	}
	origin := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	cleanPath := strings.Trim(u.Path, "/")

	var parts []string
	if cleanPath != "" {
		parts = strings.Split(cleanPath, "/")
	}
	restIdx := slices.Index(parts, "rest")
	prefix := ""
	if restIdx > 0 {
		prefix = "/" + strings.Join(parts[:restIdx], "/")
	}

	if isWebhook {
		if restIdx == -1 || restIdx+2 >= len(parts) {
			return "", fmt.Errorf("invalid webhook endpoint: %s", clientEndpoint)
		}
		userID := parts[restIdx+1]
		code := parts[restIdx+2]
		if apiVersion == config.APIVersionV3 {
			return fmt.Sprintf("%s%s/rest/%s/%s/api/%s", origin, prefix, userID, code, method), nil
		}
		return fmt.Sprintf("%s%s/rest/%s/%s/%s.json", origin, prefix, userID, code, method), nil
	}

	if apiVersion == config.APIVersionV3 {
		return fmt.Sprintf("%s%s/rest/api/%s", origin, prefix, method), nil
	}
	return fmt.Sprintf("%s%s/rest/%s.json", origin, prefix, method), nil
}

type RestClient struct {
	cfg         *config.Config
	tokens      *TokenManager
	logger      *config.Logger
	httpClient  *http.Client
	pacingMu    sync.Mutex
	pacingUntil map[string]time.Time
}

func NewRestClient(cfg *config.Config, tokens *TokenManager, logger *config.Logger, httpClient *http.Client) *RestClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: time.Duration(cfg.BitrixRequestTimeoutMs) * time.Millisecond}
	}
	return &RestClient{
		cfg:         cfg,
		tokens:      tokens,
		logger:      logger,
		httpClient:  httpClient,
		pacingUntil: make(map[string]time.Time),
	}
}

func (c *RestClient) IsV2Mode(ctx context.Context, connectionID string) bool {
	if c == nil || c.cfg == nil {
		return false
	}
	if c.cfg.BitrixAPIVersion == config.APIVersionV2 {
		return true
	}
	if c.cfg.BitrixAPIVersion == config.APIVersionV3 {
		return false
	}
	if cachedVersion, found := c.tokens.GetCachedAPIVersion(ctx, connectionID, c.cfg.SchemaCacheTTLSeconds); found && cachedVersion == config.APIVersionV2 {
		return true
	}
	return false
}

func (c *RestClient) ShouldFallbackToV2(err error) bool {
	if c != nil && c.cfg != nil && c.cfg.BitrixAPIVersion == config.APIVersionV3 {
		return false
	}
	var appErr *config.AppError
	return errors.As(err, &appErr) && isVersionFallbackError(appErr)
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

func shouldCachePortalV2(method string, appErr *config.AppError) bool {
	if appErr == nil {
		return false
	}
	if appErr.Code == "HTTP_404" {
		return true
	}
	if strings.HasSuffix(method, ".aggregate") ||
		strings.HasSuffix(method, ".delete") ||
		strings.HasSuffix(method, ".productrow.list") ||
		strings.HasPrefix(method, "crm.productrow.") {
		return false
	}
	return isVersionFallbackError(appErr)
}

func isLegacyOnlyMethod(method string) bool {
	return strings.HasPrefix(method, "crm.item.") ||
		strings.HasPrefix(method, "crm.type.") ||
		strings.HasPrefix(method, "crm.status.") ||
		strings.HasPrefix(method, "crm.category.") ||
		strings.HasPrefix(method, "user.") ||
		strings.HasPrefix(method, "app.") ||
		method == "profile"
}

func (c *RestClient) resolveTargetVersion(ctx context.Context, connectionID string, options RequestOptions, methods ...string) config.APIVersion {
	target := options.APIVersion
	if target == "" {
		target = c.cfg.BitrixAPIVersion
	}
	if target == "" {
		target = config.APIVersionAuto
	}
	if options.APIVersion != config.APIVersionV3 {
		for _, m := range methods {
			if isLegacyOnlyMethod(m) {
				return config.APIVersionV2
			}
		}
	}
	if target == config.APIVersionAuto && c.IsV2Mode(ctx, connectionID) {
		return config.APIVersionV2
	}
	return target
}

func (c *RestClient) waitForPacing(ctx context.Context, connectionID string) error {
	c.pacingMu.Lock()
	until, ok := c.pacingUntil[connectionID]
	c.pacingMu.Unlock()
	if !ok {
		return nil
	}
	wait := time.Until(until)
	if wait <= 0 {
		return nil
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *RestClient) recordOperatingTelemetry(connectionID, method string, t *BitrixTime) {
	if t == nil || t.Operating < operatingHighWaterSeconds {
		return
	}
	c.pacingMu.Lock()
	c.pacingUntil[connectionID] = time.Now().Add(operatingPacingDelay)
	c.pacingMu.Unlock()
	if c.logger != nil {
		c.logger.Warn("Bitrix24 operating time approaching 10-minute limit, applying pacing delay", map[string]any{
			"connectionId":     connectionID,
			"method":           method,
			"operating":        t.Operating,
			"operatingResetAt": t.OperatingResetAt,
		})
	}
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

	targetVersion := c.resolveTargetVersion(ctx, connectionID, options, method)
	if targetVersion != config.APIVersionAuto {
		return c.callWithVersion(ctx, connectionID, method, params, targetVersion, options)
	}

	resp, err := c.callWithVersion(ctx, connectionID, method, params, config.APIVersionV3, options)
	if err != nil {
		var appErr *config.AppError
		if errors.As(err, &appErr) && isVersionFallbackError(appErr) {
			if shouldCachePortalV2(method, appErr) {
				c.tokens.CacheAPIVersion(ctx, connectionID, config.APIVersionV2)
			}
			if c.logger != nil {
				c.logger.Warn("Bitrix24 REST 3.0 endpoint unavailable, falling back to Legacy REST v2", map[string]any{
					"connectionId": connectionID,
					"method":       method,
					"statusCode":   appErr.StatusCode,
					"errorCode":    appErr.Code,
				})
			}
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
	version config.APIVersion,
	options RequestOptions,
) (*BitrixResponse, error) {
	return c.doPostWithRetry(ctx, connectionID, method, version, options, nil, func(accessToken string, isWebhook bool) ([]byte, error) {
		reqBody := make(map[string]any, len(params)+1)
		for k, v := range params {
			reqBody[k] = v
		}
		if !isWebhook {
			reqBody["auth"] = accessToken
		}
		return json.Marshal(reqBody)
	})
}

func (c *RestClient) doPostWithRetry(
	ctx context.Context,
	connectionID string,
	method string,
	version config.APIVersion,
	options RequestOptions,
	buildQuery func(accessToken string, isWebhook bool) url.Values,
	buildBody func(accessToken string, isWebhook bool) ([]byte, error),
) (*BitrixResponse, error) {
	if err := c.waitForPacing(ctx, connectionID); err != nil {
		return nil, err
	}

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
		if buildQuery != nil {
			if q := buildQuery(credentials.AccessToken, isWebhook); len(q) > 0 {
				endpoint = endpoint + "?" + q.Encode()
			}
		}

		payloadBytes, err := buildBody(credentials.AccessToken, isWebhook)
		if err != nil {
			return nil, err
		}

		if c.logger != nil {
			c.logger.Debug("Calling Bitrix24 REST API", map[string]any{
				"requestId":    requestID,
				"connectionId": connectionID,
				"method":       method,
				"attempt":      attempt,
				"version":      string(version),
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
		c.recordOperatingTelemetry(connectionID, method, payload.Time)

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

	methods := make([]string, len(commands))
	for i, cmd := range commands {
		methods[i] = cmd.Method
	}
	targetVersion := c.resolveTargetVersion(ctx, connectionID, options, methods...)
	if targetVersion == config.APIVersionV2 {
		return c.callBatchV2(ctx, connectionID, commands, halt, options)
	}

	res, err := c.callBatchV3(ctx, connectionID, commands, halt, options)
	if err != nil {
		var appErr *config.AppError
		if targetVersion == config.APIVersionAuto && errors.As(err, &appErr) && isVersionFallbackError(appErr) {
			c.tokens.CacheAPIVersion(ctx, connectionID, config.APIVersionV2)
		}
		return nil, err
	}
	return res, nil
}

func forEachBatchChunk(commands []NamedBatchCommand, fn func(chunk []NamedBatchCommand) error) error {
	for offset := 0; offset < len(commands); offset += BitrixBatchLimit {
		end := offset + BitrixBatchLimit
		if end > len(commands) {
			end = len(commands)
		}
		if err := fn(commands[offset:end]); err != nil {
			return err
		}
	}
	return nil
}

func (c *RestClient) callBatchV3(
	ctx context.Context,
	connectionID string,
	commands []NamedBatchCommand,
	halt bool,
	options RequestOptions,
) (map[string]json.RawMessage, error) {
	combined := make(map[string]json.RawMessage)

	err := forEachBatchChunk(commands, func(chunk []NamedBatchCommand) error {
		queryBuilder := func(accessToken string, isWebhook bool) url.Values {
			q := url.Values{}
			if !isWebhook && accessToken != "" {
				q.Set("auth", accessToken)
			}
			if halt {
				q.Set("halt", "1")
			}
			return q
		}
		bodyBuilder := func(_ string, _ bool) ([]byte, error) {
			items := make([]v3BatchItem, 0, len(chunk))
			for _, cmd := range chunk {
				var q map[string]any
				if len(cmd.Params) > 0 {
					q = make(map[string]any, len(cmd.Params))
					for k, v := range cmd.Params {
						q[k] = v
					}
				}
				items = append(items, v3BatchItem{
					Method: cmd.Method,
					Query:  q,
				})
			}
			return json.Marshal(items)
		}

		bitrixResp, err := c.doPostWithRetry(ctx, connectionID, "batch", config.APIVersionV3, options, queryBuilder, bodyBuilder)
		if err != nil {
			return err
		}

		var resList []json.RawMessage
		if err := json.Unmarshal(bitrixResp.Result, &resList); err == nil && len(resList) == len(chunk) {
			for i, cmd := range chunk {
				var itemResp BitrixResponse
				if err := json.Unmarshal(resList[i], &itemResp); err == nil {
					if itemResp.Error != "" {
						if options.IgnoreBatchErrors {
							continue
						}
						return config.NewAppError(
							fmt.Sprintf("Bitrix24 batch command '%s' failed (%s)", cmd.Key, itemResp.Error),
							itemResp.Error,
							502,
						)
					}
					if len(itemResp.Result) > 0 {
						combined[cmd.Key] = itemResp.Result
						continue
					}
				}
				combined[cmd.Key] = resList[i]
			}
			return nil
		}

		var batchPayload batchResultPayload
		if err := json.Unmarshal(bitrixResp.Result, &batchPayload); err == nil {
			if !options.IgnoreBatchErrors {
				if err := checkBatchResultErrors(batchPayload.ResultError); err != nil {
					return err
				}
			}
			for _, cmd := range chunk {
				if val, ok := batchPayload.Result[cmd.Key]; ok {
					combined[cmd.Key] = val
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
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

	err := forEachBatchChunk(commands, func(chunk []NamedBatchCommand) error {
		cmdMap := make(map[string]any, len(chunk))
		for _, cmd := range chunk {
			serialized, err := BuildBatchCommand(cmd.Method, cmd.Params)
			if err != nil {
				return err
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
			return err
		}

		var batchPayload batchResultPayload
		if err := json.Unmarshal(resp.Result, &batchPayload); err != nil {
			return err
		}

		if !options.IgnoreBatchErrors {
			if err := checkBatchResultErrors(batchPayload.ResultError); err != nil {
				return err
			}
		}

		for _, cmd := range chunk {
			if val, ok := batchPayload.Result[cmd.Key]; ok {
				combined[cmd.Key] = val
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return combined, nil
}

func checkBatchResultErrors(resultError FlexMap[json.RawMessage]) error {
	for failedKey, errVal := range resultError {
		var errResp BitrixResponse
		_ = json.Unmarshal(errVal, &errResp)
		errCode := errResp.Error
		if errCode == "" {
			errCode = "BATCH_COMMAND_ERROR"
		}
		return config.NewAppError(
			fmt.Sprintf("Bitrix24 batch command '%s' failed (%s)", failedKey, errCode),
			errCode,
			502,
		)
	}
	return nil
}

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
		delay            time.Duration
		hasDelay         bool
		maxRetryWait     = 10 * time.Second
		maxOperatingWait = 60 * time.Second
	)
	if deadline, ok := ctx.Deadline(); ok {
		if rem := time.Until(deadline); rem > 0 && rem < maxOperatingWait {
			maxOperatingWait = rem
		}
	}
	if operatingResetAt > 0 {
		now := time.Now().Unix()
		diff := operatingResetAt - now
		if diff > 0 {
			delay = time.Duration(diff) * time.Second
			if delay > maxOperatingWait {
				delay = maxOperatingWait
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
