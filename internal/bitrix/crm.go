package bitrix

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/SeRGei93/b24-mcp/internal/config"
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

type SearchItemsInput struct {
	ConnectionID       string
	EntityTypeID       int
	Filter             map[string]any
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
	baseSelect := input.Select
	if len(baseSelect) == 0 {
		baseSelect = []string{"*"}
	}
	baseOrder := input.Order
	if len(baseOrder) == 0 {
		baseOrder = map[string]string{"id": "ASC"}
	}
	filter := input.Filter
	if filter == nil {
		filter = make(map[string]any)
	}

	baseParams := map[string]any{
		"entityTypeId": input.EntityTypeID,
		"filter":       filter,
		"select":       baseSelect,
		"order":        baseOrder,
		"start":        0,
	}

	firstResp, err := s.client.Call(ctx, input.ConnectionID, "crm.item.list", baseParams)
	if err != nil {
		return nil, err
	}

	var firstObj struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(firstResp.Result, &firstObj)
	items := firstObj.Items
	if items == nil {
		items = []map[string]any{}
	}

	total := firstResp.Total

	if firstResp.Next != nil && len(items) < input.Limit && s.cfg.BitrixMaxPages > 1 {
		if total != nil && *total > len(items) {
			targetCount := *total
			if input.Limit < targetCount {
				targetCount = input.Limit
			}
			maxCount := s.cfg.BitrixMaxPages * BitrixBatchLimit
			if targetCount > maxCount {
				targetCount = maxCount
			}

			var commands []NamedBatchCommand
			var keys []string
			page := 1
			for start := *firstResp.Next; start < targetCount && page < s.cfg.BitrixMaxPages; start += BitrixBatchLimit {
				key := fmt.Sprintf("p_%d", start)
				keys = append(keys, key)
				pageParams := map[string]any{
					"entityTypeId": input.EntityTypeID,
					"filter":       filter,
					"select":       baseSelect,
					"order":        baseOrder,
					"start":        start,
				}
				commands = append(commands, NamedBatchCommand{
					Key:    key,
					Method: "crm.item.list",
					Params: pageParams,
				})
				page++
			}
			if len(commands) > 0 {
				batchResult, bErr := s.client.CallBatch(ctx, input.ConnectionID, commands, false)
				if bErr == nil {
					for _, k := range keys {
						if raw, ok := batchResult[k]; ok {
							var pageObj struct {
								Items []map[string]any `json:"items"`
							}
							_ = json.Unmarshal(raw, &pageObj)
							items = append(items, pageObj.Items...)
						}
					}
				}
			}
		} else {
			start := *firstResp.Next
			for page := 1; page < s.cfg.BitrixMaxPages && len(items) < input.Limit; page++ {
				pageParams := map[string]any{
					"entityTypeId": input.EntityTypeID,
					"filter":       filter,
					"select":       baseSelect,
					"order":        baseOrder,
					"start":        start,
				}
				pageResp, pErr := s.client.Call(ctx, input.ConnectionID, "crm.item.list", pageParams)
				if pErr != nil {
					break
				}
				var pageObj struct {
					Items []map[string]any `json:"items"`
				}
				_ = json.Unmarshal(pageResp.Result, &pageObj)
				items = append(items, pageObj.Items...)
				if pageResp.Next == nil {
					break
				}
				start = *pageResp.Next
			}
		}
	}

	sliced := items
	if len(sliced) > input.Limit {
		sliced = sliced[:input.Limit]
	}

	if input.IncludeProductRows && len(sliced) > 0 {
		var ownerIDs []int
		for _, item := range sliced {
			id := toInt(item["id"])
			if id == 0 {
				id = toInt(item["ID"])
			}
			if id > 0 {
				ownerIDs = append(ownerIDs, id)
			}
		}
		if len(ownerIDs) > 0 {
			rowsRes, err := s.listProductRowsForOwners(ctx, input.ConnectionID, ownerIDs, input.EntityTypeID, "", nil, nil)
			if err == nil && rowsRes.ByOwnerID != nil {
				for i, item := range sliced {
					idStr := fmt.Sprintf("%v", item["id"])
					if idStr == "" || idStr == "<nil>" {
						idStr = fmt.Sprintf("%v", item["ID"])
					}
					prodRows := rowsRes.ByOwnerID[idStr]
					if prodRows == nil {
						prodRows = []map[string]any{}
					}
					item["productRows"] = prodRows
					sliced[i] = item
				}
			}
		}
	}

	truncated := len(items) > len(sliced) || (total != nil && *total > len(sliced))

	return &SearchItemsResult{
		Items:     sliced,
		Total:     total,
		Truncated: truncated,
	}, nil
}

func (s *CrmService) GetItem(ctx context.Context, connectionID string, entityTypeID int, id int) (any, error) {
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
	var uniqueIDs []int
	for _, id := range ids {
		if !slices.Contains(uniqueIDs, id) {
			uniqueIDs = append(uniqueIDs, id)
		}
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

	commands := make([]NamedBatchCommand, 0, len(uniqueOwnerIDs))
	for _, oID := range uniqueOwnerIDs {
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

	batchRes, err := s.client.CallBatch(ctx, connectionID, commands, false)
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
			}
			_ = json.Unmarshal(raw, &parsed)
			rowsList = parsed.ProductRows
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
	firstResp, err := s.client.Call(ctx, connectionID, "user.get", map[string]any{
		"filter": filter,
		"start":  0,
	})
	if err != nil {
		return nil, err
	}

	var users []map[string]any
	_ = json.Unmarshal(firstResp.Result, &users)
	if users == nil {
		users = []map[string]any{}
	}

	total := firstResp.Total

	if firstResp.Next != nil && s.cfg.BitrixMaxPages > 1 {
		if total != nil && *total > len(users) {
			targetCount := *total
			maxCount := s.cfg.BitrixMaxPages * BitrixBatchLimit
			if targetCount > maxCount {
				targetCount = maxCount
			}
			var commands []NamedBatchCommand
			var keys []string
			page := 1
			for start := *firstResp.Next; start < targetCount && page < s.cfg.BitrixMaxPages; start += BitrixBatchLimit {
				key := fmt.Sprintf("u_%d", start)
				keys = append(keys, key)
				commands = append(commands, NamedBatchCommand{
					Key:    key,
					Method: "user.get",
					Params: map[string]any{
						"filter": filter,
						"start":  start,
					},
				})
				page++
			}
			if len(commands) > 0 {
				batchResult, bErr := s.client.CallBatch(ctx, connectionID, commands, false)
				if bErr == nil {
					for _, k := range keys {
						if raw, ok := batchResult[k]; ok {
							var pageUsers []map[string]any
							_ = json.Unmarshal(raw, &pageUsers)
							users = append(users, pageUsers...)
						}
					}
				}
			}
		} else {
			start := *firstResp.Next
			for page := 1; page < s.cfg.BitrixMaxPages; page++ {
				resp, rErr := s.client.Call(ctx, connectionID, "user.get", map[string]any{
					"filter": filter,
					"start":  start,
				})
				if rErr != nil {
					break
				}
				var pageUsers []map[string]any
				_ = json.Unmarshal(resp.Result, &pageUsers)
				users = append(users, pageUsers...)
				if resp.Next == nil {
					break
				}
				start = *resp.Next
			}
		}
	}

	return users, nil
}
