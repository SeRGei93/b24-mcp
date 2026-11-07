package bitrix

import (
	"bytes"
	"context"
	"encoding/json"
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

type BitrixResponse struct {
	Result           json.RawMessage `json:"result"`
	Error            string          `json:"error,omitempty"`
	ErrorDescription string          `json:"error_description,omitempty"`
	Total            *int            `json:"total,omitempty"`
	Next             *int            `json:"next,omitempty"`
}

type batchResultPayload struct {
	Result      FlexMap[json.RawMessage] `json:"result"`
	ResultError FlexMap[json.RawMessage] `json:"result_error"`
	ResultTotal FlexMap[int]             `json:"result_total"`
	ResultNext  FlexMap[int]             `json:"result_next"`
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

func (c *RestClient) Call(ctx context.Context, connectionID string, method string, params map[string]any) (*BitrixResponse, error) {
	if !methodRegex.MatchString(method) {
		return nil, config.NewAppError("Invalid Bitrix REST method name", "INVALID_METHOD", 400)
	}
	if params == nil {
		params = make(map[string]any)
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

		clientEndpoint := credentials.Connection.ClientEndpoint
		endpoint := strings.TrimRight(clientEndpoint, "/") + "/" + method + ".json"

		reqBody := make(map[string]any, len(params)+1)
		for k, v := range params {
			reqBody[k] = v
		}
		reqBody["auth"] = credentials.AccessToken

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
			})
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			if attempt < c.cfg.BitrixMaxRetries {
				if backoffErr := sleepBackoff(ctx, attempt, ""); backoffErr != nil {
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
				if backoffErr := sleepBackoff(ctx, attempt, ""); backoffErr != nil {
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

		if code != "" && slices.Contains(tokenCodes, code) && !refreshedForRequest {
			refreshedForRequest = true
			forceRefreshNext = true
			staleAccessToken = credentials.AccessToken
			continue
		}

		retryHeader := resp.Header.Get("Retry-After")
		if (resp.StatusCode == 429 || resp.StatusCode >= 500 || (code != "" && slices.Contains(retryableCodes, code))) && attempt < c.cfg.BitrixMaxRetries {
			if c.logger != nil {
				c.logger.Warn("Retrying Bitrix24 request", map[string]any{
					"requestId":    requestID,
					"connectionId": connectionID,
					"method":       method,
					"attempt":      attempt,
					"bitrixCode":   code,
				})
			}
			if backoffErr := sleepBackoff(ctx, attempt, retryHeader); backoffErr != nil {
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
			})
		}
		status := resp.StatusCode
		if status < 400 {
			status = 502
		}
		return nil, config.NewAppError(fmt.Sprintf("Bitrix24 rejected %s (%s)", method, code), code, status)
	}

	return nil, config.NewAppError("Bitrix24 request exhausted its retry budget", "RETRY_EXHAUSTED", 502)
}

func (c *RestClient) CallBatch(
	ctx context.Context,
	connectionID string,
	commands []NamedBatchCommand,
	halt bool,
) (map[string]json.RawMessage, error) {
	if len(commands) == 0 {
		return make(map[string]json.RawMessage), nil
	}

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

		resp, err := c.Call(ctx, connectionID, "batch", map[string]any{
			"halt": haltVal,
			"cmd":  cmdMap,
		})
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
	firstParams := make(map[string]any, len(params)+1)
	for k, v := range params {
		firstParams[k] = v
	}
	firstParams["start"] = 0

	firstResp, err := c.Call(ctx, connectionID, method, firstParams)
	if err != nil {
		return nil, nil, err
	}

	values := extract(firstResp.Result)
	if values == nil {
		values = []map[string]any{}
	}
	total := firstResp.Total

	if firstResp.Next != nil && c.cfg.BitrixMaxPages > 1 && (limit <= 0 || len(values) < limit) {
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
				pageParams := make(map[string]any, len(params)+1)
				for k, v := range params {
					pageParams[k] = v
				}
				pageParams["start"] = start
				commands = append(commands, NamedBatchCommand{
					Key:    key,
					Method: method,
					Params: pageParams,
				})
				page++
			}
			if len(commands) > 0 {
				batchResult, bErr := c.CallBatch(ctx, connectionID, commands, false)
				if bErr == nil {
					for _, k := range keys {
						if raw, ok := batchResult[k]; ok {
							values = append(values, extract(raw)...)
						}
					}
				}
			}
		} else {
			start := *firstResp.Next
			for page := 1; page < c.cfg.BitrixMaxPages && (limit <= 0 || len(values) < limit); page++ {
				pageParams := make(map[string]any, len(params)+1)
				for k, v := range params {
					pageParams[k] = v
				}
				pageParams["start"] = start
				resp, rErr := c.Call(ctx, connectionID, method, pageParams)
				if rErr != nil {
					break
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

func sleepBackoff(ctx context.Context, attempt int, retryAfter string) error {
	var (
		delay          time.Duration
		hasRetryAfter  bool
		maxRetryWait   = 10 * time.Second
	)
	if retryAfter != "" {
		if sec, err := strconv.Atoi(retryAfter); err == nil && sec >= 0 {
			delay = time.Duration(sec) * time.Second
			if delay > maxRetryWait {
				delay = maxRetryWait
			}
			hasRetryAfter = true
		}
	}
	if !hasRetryAfter {
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
