package bitrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

type EntityType struct {
	EntityTypeID int    `json:"entityTypeId"`
	Title        string `json:"title"`
	IsDynamic    bool   `json:"isDynamic"`
}

var BuiltInTypes = []EntityType{
	{EntityTypeID: 1, Title: "Lead", IsDynamic: false},
	{EntityTypeID: 2, Title: "Deal", IsDynamic: false},
	{EntityTypeID: 3, Title: "Contact", IsDynamic: false},
	{EntityTypeID: 4, Title: "Company", IsDynamic: false},
	{EntityTypeID: 7, Title: "Quote", IsDynamic: false},
	{EntityTypeID: 31, Title: "Invoice", IsDynamic: false},
}

func EntitySlug(entityTypeID int) (string, error) {
	switch entityTypeID {
	case 1:
		return "lead", nil
	case 2:
		return "deal", nil
	case 3:
		return "contact", nil
	case 4:
		return "company", nil
	case 7:
		return "quote", nil
	case 31:
		return "smartinvoice", nil
	default:
		if entityTypeID >= 128 {
			return fmt.Sprintf("smartprocess%d", entityTypeID), nil
		}
		return "", config.NewAppError(fmt.Sprintf("Unsupported CRM entityTypeId: %d", entityTypeID), "INVALID_ENTITY_TYPE", 400)
	}
}

func SupportsCategories(entityTypeID int) bool {
	return entityTypeID == 2 || entityTypeID == 31 || entityTypeID >= 128
}

func SupportsStages(entityTypeID int) bool {
	return entityTypeID == 1 || entityTypeID == 2 || entityTypeID == 7 || entityTypeID == 31 || entityTypeID >= 128
}

func entityCode(entityTypeID int) string {
	switch entityTypeID {
	case 1:
		return "LEAD"
	case 2:
		return "DEAL"
	case 3:
		return "CONTACT"
	case 4:
		return "COMPANY"
	case 7:
		return "QUOTE"
	case 31:
		return "SMART_INVOICE"
	default:
		return fmt.Sprintf("DYNAMIC_%d", entityTypeID)
	}
}

type SchemaDiscovery struct {
	cfg    *config.Config
	store  *store.Store
	client *RestClient
}

func NewSchemaDiscovery(cfg *config.Config, st *store.Store, client *RestClient) *SchemaDiscovery {
	return &SchemaDiscovery{
		cfg:    cfg,
		store:  st,
		client: client,
	}
}

func (s *SchemaDiscovery) isV2Mode(ctx context.Context, connectionID string) bool {
	if s.cfg.BitrixAPIVersion == config.APIVersionV2 {
		return true
	}
	if s.store != nil {
		var cachedVersion string
		found, err := s.store.GetSchemaCache(ctx, connectionID, "api_version", s.cfg.SchemaCacheTTLSeconds, &cachedVersion)
		if err == nil && found && cachedVersion == config.APIVersionV2 {
			return true
		}
	}
	return false
}

func (s *SchemaDiscovery) ListEntityTypes(ctx context.Context, connectionID string, refresh bool) ([]map[string]any, error) {
	var cached []map[string]any
	if !refresh {
		found, err := s.store.GetSchemaCache(ctx, connectionID, "entity-types", s.cfg.SchemaCacheTTLSeconds, &cached)
		if err == nil && found {
			for _, item := range cached {
				if id, ok := item["entityTypeId"]; ok {
					item["entityTypeId"] = ToInt(id)
				}
			}
			return cached, nil
		}
	}

	dynamicRaw, err := s.collect(ctx, connectionID, "crm.type.list", map[string]any{
		"start": -1,
	}, "types")
	if err != nil {
		return nil, err
	}

	var dynamic []map[string]any
	existingIDs := make(map[int]bool)
	for _, item := range dynamicRaw {
		id := ToInt(item["entityTypeId"])
		if id <= 0 {
			continue
		}
		title := fmt.Sprintf("Entity %d", id)
		if t, ok := item["title"].(string); ok && t != "" {
			title = t
		} else if n, ok := item["name"].(string); ok && n != "" {
			title = n
		}

		normItem := make(map[string]any, len(item)+6)
		for k, v := range item {
			normItem[k] = v
		}
		normItem["entityTypeId"] = id
		normItem["title"] = title
		normItem["isDynamic"] = true
		if slug, err := EntitySlug(id); err == nil {
			normItem["slug"] = slug
		}
		normItem["supportsCategories"] = SupportsCategories(id)
		normItem["supportsStages"] = SupportsStages(id)
		if _, ok := normItem["code"]; !ok {
			normItem["code"] = fmt.Sprintf("DYNAMIC_%d", id)
		}
		dynamic = append(dynamic, normItem)
		existingIDs[id] = true
	}

	result := make([]map[string]any, 0, len(BuiltInTypes)+len(dynamic))
	for _, bi := range BuiltInTypes {
		if !existingIDs[bi.EntityTypeID] {
			slug, _ := EntitySlug(bi.EntityTypeID)
			result = append(result, map[string]any{
				"entityTypeId":       bi.EntityTypeID,
				"title":              bi.Title,
				"isDynamic":          bi.IsDynamic,
				"code":               entityCode(bi.EntityTypeID),
				"slug":               slug,
				"supportsCategories": SupportsCategories(bi.EntityTypeID),
				"supportsStages":     SupportsStages(bi.EntityTypeID),
			})
		}
	}
	result = append(result, dynamic...)

	_ = s.store.SetSchemaCache(ctx, connectionID, "entity-types", result)
	return result, nil
}

func extractFields(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return make(map[string]any)
	}
	var resObj struct {
		Fields map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(raw, &resObj); err == nil && resObj.Fields != nil {
		return resObj.Fields
	}
	var direct map[string]any
	if err := json.Unmarshal(raw, &direct); err == nil && direct != nil {
		return direct
	}
	return make(map[string]any)
}

func (s *SchemaDiscovery) Fields(ctx context.Context, connectionID string, entityTypeID int, refresh bool) (map[string]any, error) {
	key := fmt.Sprintf("fields:%d", entityTypeID)
	var cached map[string]any
	if !refresh {
		found, err := s.store.GetSchemaCache(ctx, connectionID, key, s.cfg.SchemaCacheTTLSeconds, &cached)
		if err == nil && found {
			return cached, nil
		}
	}

	var fields map[string]any
	slug, slugErr := EntitySlug(entityTypeID)
	if slugErr == nil && !s.isV2Mode(ctx, connectionID) {
		method := fmt.Sprintf("crm.%s.fields", slug)
		resp, err := s.client.Call(ctx, connectionID, method, map[string]any{})
		if err == nil {
			fields = extractFields(resp.Result)
		} else {
			var appErr *config.AppError
			if errors.As(err, &appErr) && isVersionFallbackError(appErr) {
				// fall back to legacy crm.item.fields
			} else {
				return nil, err
			}
		}
	}

	if fields == nil {
		resp, err := s.client.Call(ctx, connectionID, "crm.item.fields", map[string]any{
			"entityTypeId": entityTypeID,
		})
		if err != nil {
			return nil, err
		}
		fields = extractFields(resp.Result)
	}

	_ = s.store.SetSchemaCache(ctx, connectionID, key, fields)
	return fields, nil
}

func (s *SchemaDiscovery) Categories(ctx context.Context, connectionID string, entityTypeID int, refresh bool) ([]map[string]any, error) {
	key := fmt.Sprintf("categories:%d", entityTypeID)
	var cached []map[string]any
	if !refresh {
		found, err := s.store.GetSchemaCache(ctx, connectionID, key, s.cfg.SchemaCacheTTLSeconds, &cached)
		if err == nil && found {
			return cached, nil
		}
	}

	if !SupportsCategories(entityTypeID) {
		_ = s.store.SetSchemaCache(ctx, connectionID, key, []map[string]any{})
		return []map[string]any{}, nil
	}

	var cats []map[string]any
	slug, slugErr := EntitySlug(entityTypeID)
	if slugErr == nil && !s.isV2Mode(ctx, connectionID) && (entityTypeID == 2 || entityTypeID >= 128) {
		method := fmt.Sprintf("crm.%s.category.list", slug)
		loaded, err := s.collect(ctx, connectionID, method, map[string]any{}, "items")
		if err == nil {
			cats = loaded
		} else {
			var appErr *config.AppError
			if errors.As(err, &appErr) && isVersionFallbackError(appErr) {
				// fall through to legacy
			} else {
				return nil, err
			}
		}
	}

	if cats == nil {
		loaded, err := s.collect(ctx, connectionID, "crm.category.list", map[string]any{
			"entityTypeId": entityTypeID,
		}, "categories")
		if err != nil {
			return nil, err
		}
		cats = loaded
	}

	if cats == nil {
		cats = []map[string]any{}
	}

	_ = s.store.SetSchemaCache(ctx, connectionID, key, cats)
	return cats, nil
}

func (s *SchemaDiscovery) Stages(ctx context.Context, connectionID string, entityTypeID int, categoryID int, refresh bool) ([]map[string]any, error) {
	key := fmt.Sprintf("stages:%d:%d", entityTypeID, categoryID)
	var cached []map[string]any
	if !refresh {
		found, err := s.store.GetSchemaCache(ctx, connectionID, key, s.cfg.SchemaCacheTTLSeconds, &cached)
		if err == nil && found {
			return cached, nil
		}
	}

	var stages []map[string]any
	slug, slugErr := EntitySlug(entityTypeID)
	if slugErr == nil && !s.isV2Mode(ctx, connectionID) && (entityTypeID == 2 || entityTypeID >= 128) {
		method := fmt.Sprintf("crm.%s.category.stage.list", slug)
		loaded, err := s.collect(ctx, connectionID, method, map[string]any{
			"filter": []any{"categoryId", "=", categoryID},
		}, "items")
		if err == nil {
			stages = loaded
		} else {
			var appErr *config.AppError
			if errors.As(err, &appErr) && isVersionFallbackError(appErr) {
				// fall through to legacy
			} else {
				return nil, err
			}
		}
	}

	if stages == nil {
		statusEntityID := stageEntityID(entityTypeID, categoryID)
		loaded, err := s.collect(ctx, connectionID, "crm.status.list", map[string]any{
			"filter": map[string]any{"ENTITY_ID": statusEntityID},
			"order":  map[string]any{"SORT": "ASC"},
		}, "")
		if err != nil {
			return nil, err
		}
		stages = loaded
	}

	if stages == nil {
		stages = []map[string]any{}
	}

	_ = s.store.SetSchemaCache(ctx, connectionID, key, stages)
	return stages, nil
}

func (s *SchemaDiscovery) Describe(
	ctx context.Context,
	connectionID string,
	entityTypeID int,
	categoryID *int,
	refresh bool,
) (map[string]any, error) {
	entities, err := s.ListEntityTypes(ctx, connectionID, refresh)
	if err != nil {
		return nil, err
	}
	var foundEntity map[string]any
	for _, e := range entities {
		if id, ok := e["entityTypeId"].(int); ok && id == entityTypeID {
			foundEntity = e
			break
		}
	}
	if foundEntity == nil {
		return nil, config.NewAppError(fmt.Sprintf("Unknown CRM entity type '%d'", entityTypeID), "UNKNOWN_ENTITY_TYPE", 404)
	}

	fields, err := s.Fields(ctx, connectionID, entityTypeID, refresh)
	if err != nil {
		return nil, err
	}

	cats, err := s.Categories(ctx, connectionID, entityTypeID, refresh)
	if err != nil {
		cats = []map[string]any{}
	}

	var selectedCategories []map[string]any
	if categoryID != nil {
		for _, cat := range cats {
			id := ToInt(cat["id"])
			if id == *categoryID {
				selectedCategories = append(selectedCategories, cat)
			}
		}
	} else {
		selectedCategories = cats
	}

	var catIDs []int
	if len(selectedCategories) > 0 {
		for _, cat := range selectedCategories {
			catIDs = append(catIDs, ToInt(cat["id"]))
		}
	} else {
		cid := 0
		if categoryID != nil {
			cid = *categoryID
		}
		catIDs = []int{cid}
	}

	stagesByCategory := make(map[int][]map[string]any)
	var uncachedCatIDs []int

	for _, cID := range catIDs {
		if !refresh {
			var cached []map[string]any
			found, err := s.store.GetSchemaCache(ctx, connectionID, fmt.Sprintf("stages:%d:%d", entityTypeID, cID), s.cfg.SchemaCacheTTLSeconds, &cached)
			if err == nil && found {
				stagesByCategory[cID] = cached
				continue
			}
		}
		uncachedCatIDs = append(uncachedCatIDs, cID)
	}

	if len(uncachedCatIDs) == 1 {
		cID := uncachedCatIDs[0]
		loaded, err := s.Stages(ctx, connectionID, entityTypeID, cID, refresh)
		if err != nil {
			return nil, err
		}
		stagesByCategory[cID] = loaded
	} else if len(uncachedCatIDs) > 1 {
		slug, slugErr := EntitySlug(entityTypeID)
		useV3 := slugErr == nil && !s.isV2Mode(ctx, connectionID) && (entityTypeID == 2 || entityTypeID >= 128)

		var commands []NamedBatchCommand
		if useV3 {
			for _, cID := range uncachedCatIDs {
				commands = append(commands, NamedBatchCommand{
					Key:    fmt.Sprintf("cat_%d", cID),
					Method: fmt.Sprintf("crm.%s.category.stage.list", slug),
					Params: map[string]any{
						"filter": []any{"categoryId", "=", cID},
					},
				})
			}
		} else {
			for _, cID := range uncachedCatIDs {
				commands = append(commands, NamedBatchCommand{
					Key:    fmt.Sprintf("cat_%d", cID),
					Method: "crm.status.list",
					Params: map[string]any{
						"filter": map[string]any{"ENTITY_ID": stageEntityID(entityTypeID, cID)},
						"order":  map[string]any{"SORT": "ASC"},
					},
				})
			}
		}

		batchRes, batchErr := s.client.CallBatch(ctx, connectionID, commands, false)
		if batchErr != nil && useV3 {
			var appErr *config.AppError
			if errors.As(batchErr, &appErr) && isVersionFallbackError(appErr) {
				useV3 = false
				var legacyCommands []NamedBatchCommand
				for _, cID := range uncachedCatIDs {
					legacyCommands = append(legacyCommands, NamedBatchCommand{
						Key:    fmt.Sprintf("cat_%d", cID),
						Method: "crm.status.list",
						Params: map[string]any{
							"filter": map[string]any{"ENTITY_ID": stageEntityID(entityTypeID, cID)},
							"order":  map[string]any{"SORT": "ASC"},
						},
					})
				}
				batchRes, batchErr = s.client.CallBatch(ctx, connectionID, legacyCommands, false)
			}
		}
		if batchErr != nil {
			return nil, batchErr
		}
		for _, cID := range uncachedCatIDs {
			key := fmt.Sprintf("cat_%d", cID)
			var list []map[string]any
			if raw, ok := batchRes[key]; ok {
				list = extractList(raw)
			}
			if list == nil {
				list = []map[string]any{}
			}
			_ = s.store.SetSchemaCache(ctx, connectionID, fmt.Sprintf("stages:%d:%d", entityTypeID, cID), list)
			stagesByCategory[cID] = list
		}
	}

	stages := make([]map[string]any, 0, len(catIDs))
	for _, cID := range catIDs {
		list := stagesByCategory[cID]
		if list == nil {
			list = []map[string]any{}
		}
		stages = append(stages, map[string]any{
			"categoryId": cID,
			"stages":     list,
		})
	}

	return map[string]any{
		"entity":     foundEntity,
		"fields":     fields,
		"categories": cats,
		"stages":     stages,
	}, nil
}

func (s *SchemaDiscovery) ResolveEntityType(ctx context.Context, connectionID string, name string) (map[string]any, error) {
	normalized := normalizeName(name)
	entities, err := s.ListEntityTypes(ctx, connectionID, false)
	if err != nil {
		return nil, err
	}
	var matches []map[string]any
	for _, e := range entities {
		title := fmt.Sprintf("%v", e["title"])
		idStr := fmt.Sprintf("%v", e["entityTypeId"])
		code := fmt.Sprintf("%v", e["code"])
		slug := fmt.Sprintf("%v", e["slug"])
		if normalizeName(title) == normalized || idStr == normalized || normalizeName(code) == normalized || normalizeName(slug) == normalized {
			matches = append(matches, e)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return nil, config.NewAppError(fmt.Sprintf("Entity name '%s' is ambiguous", name), "AMBIGUOUS_ENTITY", 400)
	}
	return nil, config.NewAppError(fmt.Sprintf("No CRM entity matches '%s'", name), "UNKNOWN_ENTITY_TYPE", 404)
}

func (s *SchemaDiscovery) Refresh(ctx context.Context, connectionID string) (map[string]any, error) {
	cleared, err := s.store.ClearSchemaCache(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"clearedEntries": cleared,
	}, nil
}

func extractList(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err == nil {
		for _, k := range []string{"items", "categories", "stages", "types"} {
			if inner, ok := wrapper[k]; ok {
				var list []map[string]any
				if err := json.Unmarshal(inner, &list); err == nil {
					return list
				}
			}
		}
	}
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	return nil
}

func (s *SchemaDiscovery) collect(
	ctx context.Context,
	connectionID string,
	method string,
	params map[string]any,
	nestedKey string,
) ([]map[string]any, error) {
	extract := func(raw json.RawMessage) []map[string]any {
		if len(raw) == 0 {
			return nil
		}
		if nestedKey != "" {
			var wrapper map[string]json.RawMessage
			if err := json.Unmarshal(raw, &wrapper); err == nil {
				if inner, ok := wrapper[nestedKey]; ok {
					var list []map[string]any
					if err := json.Unmarshal(inner, &list); err == nil {
						return list
					}
				}
			}
		}
		return extractList(raw)
	}
	values, _, err := s.client.CollectPages(ctx, connectionID, method, params, 0, extract)
	return values, err
}

func stageEntityID(entityTypeID, categoryID int) string {
	if entityTypeID == 1 {
		return "STATUS"
	}
	if entityTypeID == 2 {
		if categoryID == 0 {
			return "DEAL_STAGE"
		}
		return fmt.Sprintf("DEAL_STAGE_%d", categoryID)
	}
	if entityTypeID == 7 {
		return "QUOTE_STATUS"
	}
	if entityTypeID == 31 {
		return fmt.Sprintf("SMART_INVOICE_STAGE_%d", categoryID)
	}
	return fmt.Sprintf("DYNAMIC_%d_STAGE_%d", entityTypeID, categoryID)
}

func normalizeName(value string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	result, _, _ := transform.String(t, value)
	return strings.ToLower(strings.TrimSpace(result))
}
