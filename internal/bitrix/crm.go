package bitrix

import (
	"context"
	"encoding/json"
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

func toOwnerTypeCode(entityTypeID int) string {
	meta, _ := getEntityMeta(entityTypeID)
	return meta.ownerTypeCode
}

func isWildcardSelect(userSelect []string) bool {
	if len(userSelect) == 0 {
		return true
	}
	for _, s := range userSelect {
		if strings.TrimSpace(s) == "*" {
			return true
		}
	}
	return false
}

func extractItemID(item map[string]any) int {
	if id := ToInt(item["id"]); id > 0 {
		return id
	}
	return ToInt(item["ID"])
}

func extractContactID(item map[string]any) int {
	if cID := ToInt(item["contactId"]); cID > 0 {
		return cID
	}
	if cID := ToInt(item["CONTACT_ID"]); cID > 0 {
		return cID
	}
	if cMap, ok := item["contact"].(map[string]any); ok {
		return ToInt(cMap["id"])
	}
	return 0
}

func normalizeFieldName(field string) string {
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
		return normalizeFieldName(s)
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

func defaultSelectForEntity(entityTypeID int) []string {
	meta, _ := getEntityMeta(entityTypeID)
	return meta.defaultSelect
}

func curatedSelect(entityTypeID int, userSelect []string) []string {
	def := defaultSelectForEntity(entityTypeID)
	if isWildcardSelect(userSelect) {
		return def
	}

	var result []string
	hasID := false
	for _, s := range userSelect {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			continue
		}
		norm := normalizeFieldName(trimmed)
		if norm == "id" || norm == "ID" {
			hasID = true
			result = append(result, "id")
		} else {
			result = append(result, norm)
		}
	}
	if len(result) == 0 {
		return def
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

func translateFilter(raw any) (any, bool) {
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

func isLogicalFilterGroup(m map[string]any) bool {
	if condRaw, ok := m["conditions"]; ok {
		switch condRaw.(type) {
		case []any, map[string]any:
			return true
		}
	}
	if t, ok := m["type"].(string); ok && strings.EqualFold(t, "filter") {
		if _, hasCond := m["conditions"]; hasCond {
			return true
		}
	}
	return false
}

func translateFilterMap(m map[string]any) (any, bool) {
	if len(m) == 0 {
		return nil, false
	}
	if isLogicalFilterGroup(m) {
		logic := "and"
		for k, v := range m {
			if strings.EqualFold(k, "logic") {
				if s := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", v))); s != "" {
					logic = s
				}
			}
		}
		conds, hasSub := translateFilter(m["conditions"])
		return map[string]any{
			"type":       "filter",
			"logic":      logic,
			"conditions": conds,
		}, hasSub
	}

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
	if len(s) == 0 {
		return nil, false
	}
	if len(s) == 3 {
		if fStr, ok0 := s[0].(string); ok0 {
			if opStr, ok1 := s[1].(string); ok1 {
				op := strings.ToLower(opStr)
				val := s[2]
				hasSub := op == "like" || op == "contains" || strings.Contains(fmt.Sprintf("%v", val), "%")
				return []any{[]any{toCamelCase(fStr), op, val}}, hasSub
			}
		}
	}

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

func toLegacyFilter(raw any) map[string]any {
	if raw == nil {
		return make(map[string]any)
	}
	switch f := raw.(type) {
	case map[string]any:
		if isLogicalFilterGroup(f) {
			return toLegacyFilter(f["conditions"])
		}
		return f
	case []any:
		res := make(map[string]any)
		if len(f) == 3 {
			if _, ok0 := f[0].(string); ok0 {
				if _, ok1 := f[1].(string); ok1 {
					addConditionToLegacyMap(res, f)
					return res
				}
			}
		}
		for _, item := range f {
			if cond, ok := item.([]any); ok && len(cond) == 3 {
				addConditionToLegacyMap(res, cond)
			}
		}
		return res
	default:
		return make(map[string]any)
	}
}

func addConditionToLegacyMap(dst map[string]any, cond []any) {
	field := toCamelCase(fmt.Sprintf("%v", cond[0]))
	op := strings.ToLower(fmt.Sprintf("%v", cond[1]))
	val := cond[2]
	switch op {
	case "like", "contains":
		if strVal, ok := val.(string); ok {
			val = strings.Trim(strVal, "%")
		}
		dst["%"+field] = val
	case "in":
		dst["@"+field] = val
	case "not in":
		dst["!@"+field] = val
	case "=", "!=", ">", ">=", "<", "<=":
		if strVal, ok := val.(string); ok && strings.Contains(strVal, "%") {
			dst["%"+field] = strings.Trim(strVal, "%")
		} else {
			dst[op+field] = val
		}
	default:
		dst["="+field] = val
	}
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
	Page               int
	IncludeDetails     bool
	IncludeProductRows bool
}

func (in SearchItemsInput) shouldEnrich() bool {
	return in.IncludeDetails || in.IncludeProductRows
}

type GetItemInput struct {
	ConnectionID   string
	EntityTypeID   int
	ID             int
	IDs            []int
	Select         []string
	IncludeDetails bool
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

func (s *CrmService) SearchItems(ctx context.Context, input SearchItemsInput) (*SearchItemsResult, error) {
	if input.Limit <= 0 {
		input.Limit = 100
	}
	if input.Limit > MaxSearchLimit {
		return nil, config.NewAppError(fmt.Sprintf("limit cannot exceed %d", MaxSearchLimit), "INVALID_LIMIT", 400)
	}
	page := input.Page
	if page <= 0 {
		page = 1
	}

	baseOrder := input.Order
	if len(baseOrder) == 0 {
		baseOrder = map[string]string{"id": "ASC"}
	}

	slug, slugErr := entitySlug(input.EntityTypeID)
	transFilter, hasSubstring := translateFilter(input.Filter)

	if !s.client.IsV2Mode(ctx, input.ConnectionID) && slugErr == nil && !hasSubstring {
		curatedSel := curatedSelect(input.EntityTypeID, input.Select)
		reqBody := map[string]any{
			"select":     curatedSel,
			"order":      baseOrder,
			"pagination": map[string]any{"limit": input.Limit, "page": page},
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
				if input.shouldEnrich() && len(sliced) > 0 {
					sliced = s.enrichItems(ctx, input.ConnectionID, input.EntityTypeID, sliced, isWildcardSelect(input.Select))
				}
				truncated := len(pageObj.Items) > len(sliced) || (total != nil && *total > len(sliced))
				return &SearchItemsResult{
					Items:     sliced,
					Total:     total,
					Truncated: truncated,
				}, nil
			}
		} else if !s.client.ShouldFallbackToV2(err) {
			return nil, err
		}
	}

	// Legacy REST fallback
	baseSelect := input.Select
	if len(baseSelect) == 0 {
		baseSelect = []string{"*", "UF_*"}
	}
	legacyFilter := toLegacyFilter(input.Filter)

	baseParams := map[string]any{
		"entityTypeId": input.EntityTypeID,
		"filter":       legacyFilter,
		"select":       baseSelect,
		"order":        baseOrder,
	}
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

	if input.shouldEnrich() && len(sliced) > 0 {
		sliced = s.enrichItems(ctx, input.ConnectionID, input.EntityTypeID, sliced, false)
	}

	truncated := len(items) > len(sliced) || (total != nil && *total > len(sliced))

	return &SearchItemsResult{
		Items:     sliced,
		Total:     total,
		Truncated: truncated,
	}, nil
}

func cloneItemMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src)+4)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func (s *CrmService) enrichItems(ctx context.Context, connectionID string, entityTypeID int, items []map[string]any, includeFullFields bool) []map[string]any {
	var uniqueOwnerIDs []int
	for _, item := range items {
		if id := extractItemID(item); id > 0 && !slices.Contains(uniqueOwnerIDs, id) {
			uniqueOwnerIDs = append(uniqueOwnerIDs, id)
		}
	}
	if len(uniqueOwnerIDs) == 0 {
		return items
	}

	withProdRows := supportsProductRows(entityTypeID)
	withOrderDetails := supportsOrderDetails(entityTypeID)
	if !withProdRows && !withOrderDetails && !includeFullFields {
		return items
	}

	enriched := make([]map[string]any, len(items))
	for i, item := range items {
		enriched[i] = cloneItemMap(item)
	}

	if len(uniqueOwnerIDs) > BitrixBatchLimit {
		if withProdRows {
			rowsRes, err := s.listProductRowsForOwners(ctx, connectionID, uniqueOwnerIDs, entityTypeID, "", nil, nil)
			if err == nil && rowsRes.ByOwnerID != nil {
				for _, item := range enriched {
					oID := extractItemID(item)
					if oID <= 0 {
						continue
					}
					prodRows := rowsRes.ByOwnerID[strconv.Itoa(oID)]
					if prodRows == nil {
						prodRows = []map[string]any{}
					}
					item["productRows"] = prodRows
				}
			}
		}
		return enriched
	}

	ownerType := toOwnerTypeCode(entityTypeID)
	ord := map[string]string{"sort": "asc"}
	var commands []NamedBatchCommand

	var uniqueContactIDs []int
	for _, item := range enriched {
		if cID := extractContactID(item); cID > 0 && !slices.Contains(uniqueContactIDs, cID) {
			uniqueContactIDs = append(uniqueContactIDs, cID)
		}
	}

	for _, oID := range uniqueOwnerIDs {
		if withProdRows {
			commands = append(commands, NamedBatchCommand{
				Key:    strconv.Itoa(oID),
				Method: "crm.item.productrow.list",
				Params: map[string]any{
					"filter": buildProductRowFilter(ownerType, oID, nil),
					"order":  ord,
					"start":  0,
				},
			})
		}
		if withOrderDetails {
			commands = append(commands,
				NamedBatchCommand{
					Key:    fmt.Sprintf("pay_%d", oID),
					Method: "crm.item.payment.list",
					Params: map[string]any{
						"entityTypeId": entityTypeID,
						"entityId":     oID,
					},
				},
				NamedBatchCommand{
					Key:    fmt.Sprintf("deliv_%d", oID),
					Method: "crm.item.delivery.list",
					Params: map[string]any{
						"entityTypeId": entityTypeID,
						"entityId":     oID,
					},
				},
			)
		}
		if includeFullFields {
			commands = append(commands, NamedBatchCommand{
				Key:    fmt.Sprintf("full_%d", oID),
				Method: "crm.item.get",
				Params: map[string]any{
					"entityTypeId": entityTypeID,
					"id":           oID,
				},
			})
		}
	}

	if withProdRows && len(uniqueOwnerIDs) <= BitrixBatchLimit/2 {
		for _, cID := range uniqueContactIDs {
			commands = append(commands, NamedBatchCommand{
				Key:    fmt.Sprintf("contact_%d", cID),
				Method: "crm.item.get",
				Params: map[string]any{
					"entityTypeId": EntityTypeContact,
					"id":           cID,
				},
			})
		}
	}

	if len(commands) == 0 {
		return enriched
	}

	batchRes, err := s.client.CallBatch(ctx, connectionID, commands, false, WithIgnoreBatchErrors())
	if err != nil {
		return enriched
	}

	contactsByID := make(map[int]map[string]any, len(uniqueContactIDs))
	for _, cID := range uniqueContactIDs {
		if raw, ok := batchRes[fmt.Sprintf("contact_%d", cID)]; ok {
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err == nil {
				if cItem, ok := parsed["item"].(map[string]any); ok {
					contactsByID[cID] = cItem
				} else if len(parsed) > 0 {
					contactsByID[cID] = parsed
				}
			}
		}
	}

	for _, item := range enriched {
		oID := extractItemID(item)
		if oID <= 0 {
			continue
		}
		idStr := strconv.Itoa(oID)

		if includeFullFields {
			if raw, ok := batchRes[fmt.Sprintf("full_%d", oID)]; ok {
				var parsed map[string]any
				if err := json.Unmarshal(raw, &parsed); err == nil {
					fullItem := parsed
					if inner, ok := parsed["item"].(map[string]any); ok {
						fullItem = inner
					}
					for k, v := range fullItem {
						if existing, exists := item[k]; !exists || existing == nil {
							item[k] = v
						}
					}
				}
			}
		}

		if withProdRows {
			var rowsList []map[string]any
			if raw, ok := batchRes[idStr]; ok {
				rowsList, _ = extractProductRows(raw)
			}
			if rowsList == nil {
				rowsList = []map[string]any{}
			}
			item["productRows"] = rowsList
		}

		if withOrderDetails {
			if raw, ok := batchRes[fmt.Sprintf("pay_%d", oID)]; ok {
				if list, ok := extractObjectSlice(raw); ok {
					item["payments"] = list
				}
			}
			if raw, ok := batchRes[fmt.Sprintf("deliv_%d", oID)]; ok {
				if list, ok := extractObjectSlice(raw); ok {
					item["deliveries"] = list
				}
			}
		}

		if cID := extractContactID(item); cID > 0 {
			if fullContact, ok := contactsByID[cID]; ok {
				cMap, ok := item["contact"].(map[string]any)
				if !ok || cMap == nil {
					cMap = map[string]any{"id": cID}
				} else {
					cMap = cloneItemMap(cMap)
				}
				for _, field := range []string{"id", "name", "lastName", "secondName", "fm", "post", "typeId"} {
					if val, exists := fullContact[field]; exists && val != nil {
						if _, has := cMap[field]; !has || cMap[field] == nil {
							cMap[field] = val
						}
					}
				}
				item["contact"] = cMap
			}
		}
	}

	return enriched
}

func (s *CrmService) FetchItems(ctx context.Context, input GetItemInput) (any, error) {
	fullFields := isWildcardSelect(input.Select)

	if len(input.IDs) > 0 {
		items, err := s.GetItems(ctx, input.ConnectionID, input.EntityTypeID, input.IDs, input.Select)
		if err != nil {
			return nil, err
		}
		if input.IncludeDetails && len(items) > 0 {
			items = s.enrichItems(ctx, input.ConnectionID, input.EntityTypeID, items, fullFields)
		}
		return items, nil
	}

	if input.ID > 0 {
		itemRaw, err := s.GetItem(ctx, input.ConnectionID, input.EntityTypeID, input.ID, input.Select)
		if err != nil {
			return nil, err
		}
		if input.IncludeDetails {
			if itemMap, ok := itemRaw.(map[string]any); ok {
				enriched := s.enrichItems(ctx, input.ConnectionID, input.EntityTypeID, []map[string]any{itemMap}, fullFields)
				return enriched[0], nil
			}
		}
		return itemRaw, nil
	}

	return nil, config.NewAppError("Supply id or ids", "MISSING_ITEM_ID", 400)
}

func (s *CrmService) GetItem(ctx context.Context, connectionID string, entityTypeID int, id int, userSelect ...[]string) (any, error) {
	var selInput []string
	if len(userSelect) > 0 {
		selInput = userSelect[0]
	}

	slug, slugErr := entitySlug(entityTypeID)
	if slugErr == nil && !s.client.IsV2Mode(ctx, connectionID) {
		method := fmt.Sprintf("crm.%s.get", slug)
		sel := curatedSelect(entityTypeID, selInput)
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
		if !s.client.ShouldFallbackToV2(err) {
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

func (s *CrmService) GetItems(ctx context.Context, connectionID string, entityTypeID int, ids []int, userSelect ...[]string) ([]map[string]any, error) {
	if len(ids) > MaxSearchLimit {
		return nil, config.NewAppError(fmt.Sprintf("ids cannot exceed %d items", MaxSearchLimit), "TOO_MANY_IDS", 400)
	}

	var selInput []string
	if len(userSelect) > 0 {
		selInput = userSelect[0]
	}

	var uniqueIDs []int
	for _, id := range ids {
		if !slices.Contains(uniqueIDs, id) {
			uniqueIDs = append(uniqueIDs, id)
		}
	}
	if len(uniqueIDs) == 0 {
		return []map[string]any{}, nil
	}

	slug, slugErr := entitySlug(entityTypeID)
	if slugErr == nil && !s.client.IsV2Mode(ctx, connectionID) {
		method := fmt.Sprintf("crm.%s.list", slug)
		sel := curatedSelect(entityTypeID, selInput)
		resp, err := s.client.Call(ctx, connectionID, method, map[string]any{
			"filter":     []any{[]any{"id", "in", uniqueIDs}},
			"select":     sel,
			"pagination": map[string]any{"limit": len(uniqueIDs), "page": 1},
		})
		if err == nil {
			var pageObj struct {
				Items []map[string]any `json:"items"`
			}
			if jsonErr := json.Unmarshal(resp.Result, &pageObj); jsonErr == nil {
				byID := make(map[int]map[string]any, len(pageObj.Items))
				for _, item := range pageObj.Items {
					if itemID := extractItemID(item); itemID > 0 {
						byID[itemID] = item
					}
				}
				ordered := make([]map[string]any, 0, len(pageObj.Items))
				for _, reqID := range uniqueIDs {
					if item, ok := byID[reqID]; ok {
						ordered = append(ordered, item)
					}
				}
				return ordered, nil
			}
		} else if !s.client.ShouldFallbackToV2(err) {
			return nil, err
		}
	}

	if len(uniqueIDs) > MaxBatchItems {
		return nil, config.NewAppError(fmt.Sprintf("ids cannot exceed %d items in legacy v2 mode", MaxBatchItems), "TOO_MANY_IDS", 400)
	}

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

	ordered := make([]map[string]any, 0, len(uniqueIDs))
	for _, id := range uniqueIDs {
		key := strconv.Itoa(id)
		if raw, ok := batchRes[key]; ok {
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err == nil {
				if item, ok := parsed["item"].(map[string]any); ok {
					ordered = append(ordered, item)
				} else {
					ordered = append(ordered, parsed)
				}
			}
		}
	}
	return ordered, nil
}

func (s *CrmService) CountItems(ctx context.Context, input CountItemsInput) (int, error) {
	slug, slugErr := entitySlug(input.EntityTypeID)
	transFilter, hasSubstring := translateFilter(input.Filter)

	if !s.client.IsV2Mode(ctx, input.ConnectionID) && slugErr == nil && !hasSubstring {
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
		if !s.client.ShouldFallbackToV2(err) {
			return 0, err
		}
	}

	legacyFilter := toLegacyFilter(input.Filter)
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

func buildProductRowFilter(ownerType string, ownerID int, filter map[string]any) map[string]any {
	pFilter := make(map[string]any, len(filter)+2)
	for k, v := range filter {
		pFilter[k] = v
	}
	pFilter["=ownerType"] = ownerType
	pFilter["=ownerId"] = ownerID
	return pFilter
}

func buildLegacyProductRowBatchCommands(uniqueOwnerIDs []int, ownerType string, filter map[string]any, ord map[string]string) []NamedBatchCommand {
	commands := make([]NamedBatchCommand, 0, len(uniqueOwnerIDs))
	for _, oID := range uniqueOwnerIDs {
		commands = append(commands, NamedBatchCommand{
			Key:    strconv.Itoa(oID),
			Method: "crm.item.productrow.list",
			Params: map[string]any{
				"filter": buildProductRowFilter(ownerType, oID, filter),
				"order":  ord,
				"start":  0,
			},
		})
	}
	return commands
}

func extractProductRows(raw json.RawMessage) ([]map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var parsed struct {
		ProductRows []map[string]any `json:"productRows"`
		Items       []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil {
		if parsed.ProductRows != nil {
			return parsed.ProductRows, true
		}
		if parsed.Items != nil {
			return parsed.Items, true
		}
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err == nil && arr != nil {
		return arr, true
	}
	return nil, false
}

func extractObjectSlice(raw json.RawMessage) ([]map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err == nil && arr != nil {
		return arr, true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, key := range []string{"items", "payments", "deliveries", "productRows"} {
			if subRaw, ok := obj[key]; ok {
				var subArr []map[string]any
				if err := json.Unmarshal(subRaw, &subArr); err == nil && subArr != nil {
					return subArr, true
				}
			}
		}
	}
	return nil, false
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
		ownerType = toOwnerTypeCode(entityTypeID)
	}
	ord := map[string]string{"sort": "asc"}
	if len(order) > 0 {
		ord = make(map[string]string, len(order))
		for k, v := range order {
			ord[k] = strings.ToLower(v)
		}
	}

	// Single owner: try REST 3.0 crm.productrow.list first, then fall back to crm.item.productrow.list
	if len(uniqueOwnerIDs) == 1 {
		oID := uniqueOwnerIDs[0]
		if !s.client.IsV2Mode(ctx, connectionID) && len(filter) == 0 {
			resp, err := s.client.Call(ctx, connectionID, "crm.productrow.list", map[string]any{
				"filter": []any{
					[]any{"ownerType", "=", ownerType},
					[]any{"ownerId", "=", oID},
				},
				"select":     productRowSel,
				"order":      ord,
				"pagination": map[string]any{"limit": MaxSearchLimit, "page": 1},
			})
			if err == nil {
				if rows, ok := extractProductRows(resp.Result); ok {
					key := strconv.Itoa(oID)
					byOwnerID := map[string][]map[string]any{key: rows}
					count := 1
					return &ListProductRowsResult{
						ByOwnerID:   byOwnerID,
						ProductRows: rows,
						OwnersCount: &count,
					}, nil
				}
			} else if !s.client.ShouldFallbackToV2(err) {
				return nil, err
			}
		}

		baseParams := map[string]any{
			"filter": buildProductRowFilter(ownerType, oID, filter),
			"order":  ord,
			"start":  0,
		}
		resp, err := s.client.Call(ctx, connectionID, "crm.item.productrow.list", baseParams)
		if err != nil {
			return nil, err
		}
		if rows, ok := extractProductRows(resp.Result); ok {
			if resp.Next != nil && *resp.Next > 0 {
				moreRows, _, pageErr := s.client.CollectPages(ctx, connectionID, "crm.item.productrow.list", baseParams, MaxSearchLimit, func(raw json.RawMessage) []map[string]any {
					r, _ := extractProductRows(raw)
					return r
				})
				if pageErr == nil && len(moreRows) > 0 {
					rows = moreRows
				}
			}
			key := strconv.Itoa(oID)
			byOwnerID := map[string][]map[string]any{key: rows}
			count := 1
			return &ListProductRowsResult{
				ByOwnerID:   byOwnerID,
				ProductRows: rows,
				OwnersCount: &count,
			}, nil
		}
	}

	// Multiple owners (or single owner fallback): batch REST 3.0 crm.productrow.list with legacy fallback
	useV3 := !s.client.IsV2Mode(ctx, connectionID) && len(filter) == 0
	var commands []NamedBatchCommand
	if useV3 {
		commands = make([]NamedBatchCommand, 0, len(uniqueOwnerIDs))
		for _, oID := range uniqueOwnerIDs {
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
					"pagination": map[string]any{"limit": MaxSearchLimit, "page": 1},
				},
			})
		}
	} else {
		commands = buildLegacyProductRowBatchCommands(uniqueOwnerIDs, ownerType, filter, ord)
	}

	batchRes, err := s.client.CallBatch(ctx, connectionID, commands, false)
	if err != nil && useV3 && s.client.ShouldFallbackToV2(err) {
		batchRes, err = s.client.CallBatch(ctx, connectionID, buildLegacyProductRowBatchCommands(uniqueOwnerIDs, ownerType, filter, ord), false)
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
			rowsList, _ = extractProductRows(raw)
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

	entityTypeID := EntityTypeDeal
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
