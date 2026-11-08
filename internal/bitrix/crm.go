package bitrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/SeRGei93/b24-mcp/internal/config"
)

const (
	MaxBatchItems  = 250
	MaxSearchLimit = 1000
)

var (
	ufRegex       = regexp.MustCompile(`(?i)^uf_?crm_?(.*)$`)
	productRowSel = []string{
		"id", "ownerId", "ownerType", "productId", "productName",
		"price", "quantity", "discountTypeId", "discountRate", "discountSum",
		"taxRate", "taxIncluded", "measureCode", "measureName", "sort",
	}
)

func ToOwnerTypeCode(entityTypeID int) string {
	switch entityTypeID {
	case 1:
		return "L"
	case 2:
		return "D"
	case 3:
		return "C"
	case 4:
		return "CO"
	case 7:
		return "Q"
	case 31:
		return "SI"
	default:
		return fmt.Sprintf("T%x", entityTypeID)
	}
}

func NormalizeFieldName(field string) string {
	m := ufRegex.FindStringSubmatch(field)
	if len(m) > 1 {
		return "UF_CRM_" + strings.ToUpper(m[1])
	}
	return field
}

func toCamelCase(s string) string {
	if s == "" {
		return ""
	}
	if ufRegex.MatchString(s) {
		return NormalizeFieldName(s)
	}
	if strings.Contains(s, ".") {
		parts := strings.Split(s, ".")
		for i, p := range parts {
			parts[i] = toCamelCase(p)
		}
		return strings.Join(parts, ".")
	}

	parts := strings.Split(s, "_")
	var b strings.Builder
	for i, part := range parts {
		if part == "" {
			continue
		}
		lower := strings.ToLower(part)
		if i == 0 {
			b.WriteString(lower)
		} else {
			b.WriteString(strings.ToUpper(lower[:1]) + lower[1:])
		}
	}
	res := b.String()
	if res == "" {
		return strings.ToLower(s)
	}
	return res
}

func DefaultSelectForEntity(entityTypeID int) []string {
	switch entityTypeID {
	case 1: // Lead
		return []string{
			"id", "title", "name", "lastName", "companyTitle", "statusId", "stage.name",
			"opportunity", "currencyId", "assignedById", "assignedBy.firstName", "assignedBy.lastName",
			"createdTime", "updatedTime",
		}
	case 2: // Deal
		return []string{
			"id", "title", "stageId", "stage.name", "categoryId", "category.name",
			"opportunity", "currencyId", "companyId", "company.title", "contactId", "contact.name",
			"assignedById", "assignedBy.firstName", "assignedBy.lastName",
			"begindate", "closedate", "createdTime", "updatedTime",
		}
	case 3: // Contact
		return []string{
			"id", "name", "lastName", "secondName", "companyId", "company.title",
			"typeId", "assignedById", "assignedBy.firstName", "assignedBy.lastName",
			"createdTime", "updatedTime",
		}
	case 4: // Company
		return []string{
			"id", "title", "companyType", "assignedById", "assignedBy.firstName", "assignedBy.lastName",
			"createdTime", "updatedTime",
		}
	case 7: // Quote
		return []string{
			"id", "title", "statusId", "stage.name",
			"opportunity", "currencyId", "companyId", "company.title", "contactId", "contact.name",
			"assignedById", "assignedBy.firstName", "assignedBy.lastName",
			"begindate", "closedate", "createdTime", "updatedTime",
		}
	case 31: // Smart Invoice
		return []string{
			"id", "title", "stageId", "stage.name", "categoryId", "category.name",
			"opportunity", "currencyId", "companyId", "company.title", "contactId", "contact.name",
			"assignedById", "assignedBy.firstName", "assignedBy.lastName",
			"begindate", "closedate", "createdTime", "updatedTime",
		}
	default:
		return []string{
			"id", "title", "stageId", "stage.name", "categoryId", "category.name",
			"opportunity", "currencyId", "companyId", "company.title", "contactId", "contact.name",
			"assignedById", "assignedBy.firstName", "assignedBy.lastName",
			"createdTime", "updatedTime",
		}
	}
}

func CuratedSelect(entityTypeID int, userSelect []string) []string {
	def := DefaultSelectForEntity(entityTypeID)
	if len(userSelect) == 0 {
		return def
	}

	var hasWildcard bool
	for _, s := range userSelect {
		if strings.TrimSpace(s) == "*" {
			hasWildcard = true
			break
		}
	}
	if hasWildcard {
		return def
	}

	var result []string
	hasID := false
	for _, s := range userSelect {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			continue
		}
		norm := NormalizeFieldName(trimmed)
		if norm == "id" || norm == "ID" {
			hasID = true
			result = append(result, "id")
		} else {
			result = append(result, norm)
		}
	}
	if !hasID {
		result = append([]string{"id"}, result...)
	}
	return result
}

func parseOpPrefix(key string) (op string, field string) {
	prefixes := []struct {
		prefix string
		op     string
	}{
		{"!@", "not in"},
		{"@", "in"},
		{">=", ">="},
		{"<=", "<="},
		{"!=", "!="},
		{"!", "!="},
		{">", ">"},
		{"<", "<"},
		{"=", "="},
		{"%", "like"},
		{"?", "like"},
		{"*", "like"},
	}
	for _, p := range prefixes {
		if strings.HasPrefix(key, p.prefix) {
			return p.op, key[len(p.prefix):]
		}
	}
	return "=", key
}

func TranslateFilter(raw any) (any, bool) {
	if raw == nil {
		return nil, false
	}

	switch f := raw.(type) {
	case map[string]any:
		return translateFilterMap(f)
	case []any:
		return translateFilterSlice(f)
	default:
		return raw, false
	}
}

func translateFilterMap(m map[string]any) (any, bool) {
	var (
		conditions   []any
		hasSubstring bool
		logic        string
	)

	for k, v := range m {
		if strings.EqualFold(k, "LOGIC") {
			logic = strings.ToLower(fmt.Sprintf("%v", v))
			continue
		}
		op, field := parseOpPrefix(k)
		if op == "like" || strings.Contains(fmt.Sprintf("%v", v), "%") {
			hasSubstring = true
		}
		normField := toCamelCase(field)
		conditions = append(conditions, []any{normField, op, v})
	}

	if logic == "or" {
		return map[string]any{
			"type":       "filter",
			"logic":      "or",
			"conditions": conditions,
		}, hasSubstring
	}
	return conditions, hasSubstring
}

func translateFilterSlice(s []any) (any, bool) {
	var (
		updated      []any
		hasSubstring bool
	)

	for _, item := range s {
		switch cond := item.(type) {
		case []any:
			if len(cond) == 3 {
				field := fmt.Sprintf("%v", cond[0])
				op := strings.ToLower(fmt.Sprintf("%v", cond[1]))
				val := cond[2]
				if op == "like" || op == "contains" || strings.Contains(fmt.Sprintf("%v", val), "%") {
					hasSubstring = true
				}
				normField := toCamelCase(field)
				updated = append(updated, []any{normField, op, val})
			} else {
				updated = append(updated, cond)
			}
		case map[string]any:
			sub, subHas := translateFilterMap(cond)
			if subHas {
				hasSubstring = true
			}
			updated = append(updated, sub)
		default:
			updated = append(updated, cond)
		}
	}
	return updated, hasSubstring
}

func extractCount(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var intVal int
	if err := json.Unmarshal(raw, &intVal); err == nil {
		return intVal
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		if cVal, ok := obj["count"]; ok {
			switch cv := cVal.(type) {
			case float64:
				return int(cv)
			case int:
				return cv
			case map[string]any:
				for _, v := range cv {
					return ToInt(v)
				}
			}
		}
		if totalVal, ok := obj["total"]; ok {
			return ToInt(totalVal)
		}
	}
	return 0
}

type SearchItemsInput struct {
	ConnectionID       string
	EntityTypeID       int
	Filter             any
	Select             []string
	Order              map[string]string
	Limit              int
	IncludeProductRows bool
}

type SearchItemsResult struct {
	Items     []map[string]any `json:"items"`
	Total     *int             `json:"total,omitempty"`
	Truncated bool             `json:"truncated"`
}

type CountItemsInput struct {
	ConnectionID string
	EntityTypeID int
	Filter       any
}

type ListProductRowsInput struct {
	ConnectionID string
	OwnerID      *int
	OwnerIDs     []int
	EntityTypeID *int
	OwnerType    string
	Filter       map[string]any
	Order        map[string]string
	Limit        *int
}

type ListProductRowsResult struct {
	ProductRows []map[string]any            `json:"productRows"`
	Total       *int                        `json:"total,omitempty"`
	Truncated   *bool                       `json:"truncated,omitempty"`
	ByOwnerID   map[string][]map[string]any `json:"byOwnerId,omitempty"`
	OwnersCount *int                        `json:"ownersCount,omitempty"`
}

type CrmService struct {
	cfg    *config.Config
	client *RestClient
}

func NewCrmService(cfg *config.Config, client *RestClient) *CrmService {
	return &CrmService{
		cfg:    cfg,
		client: client,
	}
}

func (s *CrmService) isV2Mode(ctx context.Context, connectionID string) bool {
	if s.cfg.BitrixAPIVersion == config.APIVersionV2 {
		return true
	}
	if s.client != nil && s.client.tokens != nil {
		if st := s.client.tokens.Store(); st != nil {
			var cachedVersion string
			found, err := st.GetSchemaCache(ctx, connectionID, "api_version", s.cfg.SchemaCacheTTLSeconds, &cachedVersion)
			if err == nil && found && cachedVersion == config.APIVersionV2 {
				return true
			}
		}
	}
	return false
}

func (s *CrmService) SearchItems(ctx context.Context, input SearchItemsInput) (*SearchItemsResult, error) {
	if input.Limit <= 0 {
		input.Limit = 100
	}
	if input.Limit > MaxSearchLimit {
		return nil, config.NewAppError(fmt.Sprintf("limit cannot exceed %d", MaxSearchLimit), "INVALID_LIMIT", 400)
	}

	baseOrder := input.Order
	if len(baseOrder) == 0 {
		baseOrder = map[string]string{"id": "ASC"}
	}

	slug, slugErr := EntitySlug(input.EntityTypeID)
	transFilter, hasSubstring := TranslateFilter(input.Filter)
	isV2 := s.isV2Mode(ctx, input.ConnectionID)

	// Try REST 3.0 when not in v2 mode, slug is known, and filter does not require substring match
	if !isV2 && slugErr == nil && !hasSubstring {
		curatedSel := CuratedSelect(input.EntityTypeID, input.Select)
		reqBody := map[string]any{
			"select":     curatedSel,
			"order":      baseOrder,
			"pagination": map[string]any{"limit": input.Limit, "page": 1},
		}
		if transFilter != nil {
			reqBody["filter"] = transFilter
		}

		method := fmt.Sprintf("crm.%s.list", slug)
		resp, err := s.client.Call(ctx, input.ConnectionID, method, reqBody)
		if err == nil {
			var pageObj struct {
				Items []map[string]any `json:"items"`
				Total *int             `json:"total"`
			}
			if unmarshalErr := json.Unmarshal(resp.Result, &pageObj); unmarshalErr == nil && pageObj.Items != nil {
				sliced := pageObj.Items
				if len(sliced) > input.Limit {
					sliced = sliced[:input.Limit]
				}
				total := pageObj.Total
				if total == nil && resp.Total != nil {
					total = resp.Total
				}
				if input.IncludeProductRows && len(sliced) > 0 {
					s.enrichProductRows(ctx, input.ConnectionID, input.EntityTypeID, sliced)
				}
				truncated := len(pageObj.Items) > len(sliced) || (total != nil && *total > len(sliced))
				return &SearchItemsResult{
					Items:     sliced,
					Total:     total,
					Truncated: truncated,
				}, nil
			}
		} else {
			var appErr *config.AppError
			if !errors.As(err, &appErr) || !isVersionFallbackError(appErr) {
				return nil, err
			}
		}
	}

	// Legacy REST fallback
	baseSelect := input.Select
	if len(baseSelect) == 0 {
		baseSelect = []string{"*"}
	}
	legacyFilter := input.Filter
	if legacyFilter == nil {
		legacyFilter = make(map[string]any)
	}

	baseParams := map[string]any{
		"entityTypeId": input.EntityTypeID,
		"filter":       legacyFilter,
		"select":       baseSelect,
		"order":        baseOrder,
	}
	// Pass start: -1 on bounded queries that do not need total to skip COUNT(*)
	if input.Limit <= 50 {
		baseParams["start"] = -1
	}

	items, total, err := s.client.CollectPages(ctx, input.ConnectionID, "crm.item.list", baseParams, input.Limit, func(raw json.RawMessage) []map[string]any {
		var pageObj struct {
			Items []map[string]any `json:"items"`
		}
		_ = json.Unmarshal(raw, &pageObj)
		return pageObj.Items
	})
	if err != nil {
		return nil, err
	}

	sliced := items
	if len(sliced) > input.Limit {
		sliced = sliced[:input.Limit]
	}

	if input.IncludeProductRows && len(sliced) > 0 {
		s.enrichProductRows(ctx, input.ConnectionID, input.EntityTypeID, sliced)
	}

	truncated := len(items) > len(sliced) || (total != nil && *total > len(sliced))

	return &SearchItemsResult{
		Items:     sliced,
		Total:     total,
		Truncated: truncated,
	}, nil
}

func (s *CrmService) enrichProductRows(ctx context.Context, connectionID string, entityTypeID int, items []map[string]any) {
	var ownerIDs []int
	for _, item := range items {
		id := ToInt(item["id"])
		if id == 0 {
			id = ToInt(item["ID"])
		}
		if id > 0 {
			ownerIDs = append(ownerIDs, id)
		}
	}
	if len(ownerIDs) == 0 {
		return
	}
	rowsRes, err := s.listProductRowsForOwners(ctx, connectionID, ownerIDs, entityTypeID, "", nil, nil)
	if err != nil || rowsRes.ByOwnerID == nil {
		return
	}
	for i, item := range items {
		idStr := fmt.Sprintf("%v", item["id"])
		if idStr == "" || idStr == "<nil>" {
			idStr = fmt.Sprintf("%v", item["ID"])
		}
		prodRows := rowsRes.ByOwnerID[idStr]
		if prodRows == nil {
			prodRows = []map[string]any{}
		}
		item["productRows"] = prodRows
		items[i] = item
	}
}

func (s *CrmService) GetItem(ctx context.Context, connectionID string, entityTypeID int, id int) (any, error) {
	slug, slugErr := EntitySlug(entityTypeID)
	if slugErr == nil && !s.isV2Mode(ctx, connectionID) {
		method := fmt.Sprintf("crm.%s.get", slug)
		sel := CuratedSelect(entityTypeID, nil)
		resp, err := s.client.Call(ctx, connectionID, method, map[string]any{
			"id":     id,
			"select": sel,
		})
		if err == nil {
			var obj map[string]any
			if unmarshalErr := json.Unmarshal(resp.Result, &obj); unmarshalErr == nil {
				if item, ok := obj["item"]; ok {
					return item, nil
				}
				return obj, nil
			}
			return resp.Result, nil
		}
		var appErr *config.AppError
		if !errors.As(err, &appErr) || !isVersionFallbackError(appErr) {
			return nil, err
		}
	}

	resp, err := s.client.Call(ctx, connectionID, "crm.item.get", map[string]any{
		"entityTypeId": entityTypeID,
		"id":           id,
	})
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(resp.Result, &obj); err == nil {
		if item, ok := obj["item"]; ok {
			return item, nil
		}
		return obj, nil
	}
	return resp.Result, nil
}

func (s *CrmService) GetItems(ctx context.Context, connectionID string, entityTypeID int, ids []int) (map[string]any, error) {
	if len(ids) > MaxBatchItems {
		return nil, config.NewAppError(fmt.Sprintf("ids cannot exceed %d items", MaxBatchItems), "TOO_MANY_IDS", 400)
	}

	var uniqueIDs []int
	for _, id := range ids {
		if !slices.Contains(uniqueIDs, id) {
			uniqueIDs = append(uniqueIDs, id)
		}
	}

	slug, slugErr := EntitySlug(entityTypeID)
	if slugErr == nil && !s.isV2Mode(ctx, connectionID) {
		method := fmt.Sprintf("crm.%s.list", slug)
		sel := CuratedSelect(entityTypeID, nil)
		resp, err := s.client.Call(ctx, connectionID, method, map[string]any{
			"filter":     []any{[]any{"id", "in", uniqueIDs}},
			"select":     sel,
			"pagination": map[string]any{"limit": len(uniqueIDs), "page": 1},
		})
		if err == nil {
			var pageObj struct {
				Items []map[string]any `json:"items"`
			}
			if jsonErr := json.Unmarshal(resp.Result, &pageObj); jsonErr == nil && len(pageObj.Items) > 0 {
				byID := make(map[string]any, len(pageObj.Items))
				for _, item := range pageObj.Items {
					idStr := fmt.Sprintf("%v", item["id"])
					byID[idStr] = item
				}
				return byID, nil
			}
		}
		var appErr *config.AppError
		if !errors.As(err, &appErr) || !isVersionFallbackError(appErr) {
			return nil, err
		}
	}

	// Legacy batch fallback
	commands := make([]NamedBatchCommand, 0, len(uniqueIDs))
	for _, id := range uniqueIDs {
		commands = append(commands, NamedBatchCommand{
			Key:    strconv.Itoa(id),
			Method: "crm.item.get",
			Params: map[string]any{
				"entityTypeId": entityTypeID,
				"id":           id,
			},
		})
	}

	batchRes, err := s.client.CallBatch(ctx, connectionID, commands, false)
	if err != nil {
		return nil, err
	}

	itemsByID := make(map[string]any, len(uniqueIDs))
	for _, id := range uniqueIDs {
		key := strconv.Itoa(id)
		if raw, ok := batchRes[key]; ok {
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err == nil {
				if item, ok := parsed["item"]; ok {
					itemsByID[key] = item
				} else {
					itemsByID[key] = parsed
				}
			} else {
				itemsByID[key] = raw
			}
		}
	}
	return itemsByID, nil
}

func (s *CrmService) CountItems(ctx context.Context, input CountItemsInput) (int, error) {
	slug, slugErr := EntitySlug(input.EntityTypeID)
	transFilter, hasSubstring := TranslateFilter(input.Filter)
	isV2 := s.isV2Mode(ctx, input.ConnectionID)

	if !isV2 && slugErr == nil && !hasSubstring {
		method := fmt.Sprintf("crm.%s.aggregate", slug)
		params := map[string]any{
			"select": map[string]any{"count": []string{"id"}},
		}
		if transFilter != nil {
			params["filter"] = transFilter
		}
		resp, err := s.client.Call(ctx, input.ConnectionID, method, params)
		if err == nil {
			return extractCount(resp.Result), nil
		}
		var appErr *config.AppError
		if !errors.As(err, &appErr) || !isVersionFallbackError(appErr) {
			return 0, err
		}
	}

	// Legacy fallback to crm.item.list with start: 0 to get total count
	legacyFilter := input.Filter
	if legacyFilter == nil {
		legacyFilter = make(map[string]any)
	}
	resp, err := s.client.Call(ctx, input.ConnectionID, "crm.item.list", map[string]any{
		"entityTypeId": input.EntityTypeID,
		"filter":       legacyFilter,
		"select":       []string{"id"},
		"start":        0,
	})
	if err != nil {
		return 0, err
	}
	if resp.Total != nil {
		return *resp.Total, nil
	}
	return extractCount(resp.Result), nil
}

func (s *CrmService) listProductRowsForOwners(
	ctx context.Context,
	connectionID string,
	ownerIDs []int,
	entityTypeID int,
	ownerType string,
	filter map[string]any,
	order map[string]string,
) (*ListProductRowsResult, error) {
	var uniqueOwnerIDs []int
	for _, id := range ownerIDs {
		if !slices.Contains(uniqueOwnerIDs, id) {
			uniqueOwnerIDs = append(uniqueOwnerIDs, id)
		}
	}

	if ownerType == "" {
		ownerType = ToOwnerTypeCode(entityTypeID)
	}
	ord := map[string]string{"sort": "asc"}
	if len(order) > 0 {
		ord = make(map[string]string, len(order))
		for k, v := range order {
			ord[k] = strings.ToLower(v)
		}
	}

	isV2 := s.isV2Mode(ctx, connectionID)

	// Single owner with REST 3.0
	if !isV2 && len(uniqueOwnerIDs) == 1 {
		oID := uniqueOwnerIDs[0]
		resp, err := s.client.Call(ctx, connectionID, "crm.productrow.list", map[string]any{
			"filter": []any{
				[]any{"ownerType", "=", ownerType},
				[]any{"ownerId", "=", oID},
			},
			"select":     productRowSel,
			"order":      ord,
			"pagination": map[string]any{"limit": 1000, "page": 1},
		})
		if err == nil {
			var parsed struct {
				Items []map[string]any `json:"items"`
			}
			if jsonErr := json.Unmarshal(resp.Result, &parsed); jsonErr == nil && parsed.Items != nil {
				key := strconv.Itoa(oID)
				byOwnerID := map[string][]map[string]any{key: parsed.Items}
				count := 1
				return &ListProductRowsResult{
					ByOwnerID:   byOwnerID,
					ProductRows: parsed.Items,
					OwnersCount: &count,
				}, nil
			}
		}
	}

	// Multiple owners: batch
	commands := make([]NamedBatchCommand, 0, len(uniqueOwnerIDs))
	useV3 := !isV2
	for _, oID := range uniqueOwnerIDs {
		if useV3 {
			commands = append(commands, NamedBatchCommand{
				Key:    strconv.Itoa(oID),
				Method: "crm.productrow.list",
				Params: map[string]any{
					"filter": []any{
						[]any{"ownerType", "=", ownerType},
						[]any{"ownerId", "=", oID},
					},
					"select":     productRowSel,
					"order":      ord,
					"pagination": map[string]any{"limit": 1000, "page": 1},
				},
			})
		} else {
			pFilter := make(map[string]any)
			for k, v := range filter {
				pFilter[k] = v
			}
			pFilter["=ownerType"] = ownerType
			pFilter["=ownerId"] = oID
			commands = append(commands, NamedBatchCommand{
				Key:    strconv.Itoa(oID),
				Method: "crm.item.productrow.list",
				Params: map[string]any{
					"filter": pFilter,
					"order":  ord,
					"start":  0,
				},
			})
		}
	}

	batchRes, err := s.client.CallBatch(ctx, connectionID, commands, false)
	if err != nil && useV3 {
		var appErr *config.AppError
		if errors.As(err, &appErr) && isVersionFallbackError(appErr) {
			// Retry with legacy
			legacyCommands := make([]NamedBatchCommand, 0, len(uniqueOwnerIDs))
			for _, oID := range uniqueOwnerIDs {
				pFilter := make(map[string]any)
				for k, v := range filter {
					pFilter[k] = v
				}
				pFilter["=ownerType"] = ownerType
				pFilter["=ownerId"] = oID
				legacyCommands = append(legacyCommands, NamedBatchCommand{
					Key:    strconv.Itoa(oID),
					Method: "crm.item.productrow.list",
					Params: map[string]any{
						"filter": pFilter,
						"order":  ord,
						"start":  0,
					},
				})
			}
			batchRes, err = s.client.CallBatch(ctx, connectionID, legacyCommands, false)
		}
	}
	if err != nil {
		return nil, err
	}

	byOwnerID := make(map[string][]map[string]any, len(uniqueOwnerIDs))
	var allProductRows []map[string]any

	for _, oID := range uniqueOwnerIDs {
		key := strconv.Itoa(oID)
		var rowsList []map[string]any
		if raw, ok := batchRes[key]; ok {
			var parsed struct {
				ProductRows []map[string]any `json:"productRows"`
				Items       []map[string]any `json:"items"`
			}
			_ = json.Unmarshal(raw, &parsed)
			if parsed.ProductRows != nil {
				rowsList = parsed.ProductRows
			} else if parsed.Items != nil {
				rowsList = parsed.Items
			}
		}
		if rowsList == nil {
			rowsList = []map[string]any{}
		}
		byOwnerID[key] = rowsList
		allProductRows = append(allProductRows, rowsList...)
	}

	count := len(uniqueOwnerIDs)
	return &ListProductRowsResult{
		ByOwnerID:   byOwnerID,
		ProductRows: allProductRows,
		OwnersCount: &count,
	}, nil
}

func (s *CrmService) ListProductRows(ctx context.Context, input ListProductRowsInput) (*ListProductRowsResult, error) {
	ids := input.OwnerIDs
	if len(ids) == 0 && input.OwnerID != nil {
		ids = []int{*input.OwnerID}
	}
	if len(ids) == 0 {
		return nil, config.NewAppError("Supply dealIds, ownerIds, dealId, or ownerId", "MISSING_OWNER_ID", 400)
	}
	if len(ids) > MaxBatchItems {
		return nil, config.NewAppError(fmt.Sprintf("ownerIds/dealIds cannot exceed %d items", MaxBatchItems), "TOO_MANY_IDS", 400)
	}
	if input.Limit != nil && *input.Limit > MaxSearchLimit {
		return nil, config.NewAppError(fmt.Sprintf("limit cannot exceed %d", MaxSearchLimit), "INVALID_LIMIT", 400)
	}

	entityTypeID := 2
	if input.EntityTypeID != nil {
		entityTypeID = *input.EntityTypeID
	}

	if len(ids) > 1 || len(input.OwnerIDs) > 0 {
		return s.listProductRowsForOwners(ctx, input.ConnectionID, ids, entityTypeID, input.OwnerType, input.Filter, input.Order)
	}

	singleOwnerID := ids[0]
	limit := 100
	if input.Limit != nil && *input.Limit > 0 {
		limit = *input.Limit
	}

	batchSingle, err := s.listProductRowsForOwners(ctx, input.ConnectionID, []int{singleOwnerID}, entityTypeID, input.OwnerType, input.Filter, input.Order)
	if err != nil {
		return nil, err
	}

	allRows := batchSingle.ProductRows
	sliced := allRows
	if len(sliced) > limit {
		sliced = sliced[:limit]
	}
	total := len(allRows)
	truncated := total > len(sliced)

	return &ListProductRowsResult{
		ProductRows: sliced,
		Total:       &total,
		Truncated:   &truncated,
	}, nil
}

func (s *CrmService) ListUsers(ctx context.Context, connectionID string, filter map[string]any) ([]map[string]any, error) {
	if filter == nil {
		filter = make(map[string]any)
	}
	users, _, err := s.client.CollectPages(ctx, connectionID, "user.get", map[string]any{
		"filter": filter,
	}, 0, func(raw json.RawMessage) []map[string]any {
		var pageUsers []map[string]any
		_ = json.Unmarshal(raw, &pageUsers)
		return pageUsers
	})
	return users, err
}
