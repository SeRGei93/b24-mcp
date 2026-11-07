package bitrix

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

const PageSize = 50

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
	{EntityTypeID: 31, Title: "Invoice", IsDynamic: false},
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

	dynamicRaw, err := s.collect(ctx, connectionID, "crm.type.list", map[string]any{}, "types")
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

		normItem := make(map[string]any, len(item)+3)
		for k, v := range item {
			normItem[k] = v
		}
		normItem["entityTypeId"] = id
		normItem["title"] = title
		normItem["isDynamic"] = true
		dynamic = append(dynamic, normItem)
		existingIDs[id] = true
	}

	result := make([]map[string]any, 0, len(BuiltInTypes)+len(dynamic))
	for _, bi := range BuiltInTypes {
		if !existingIDs[bi.EntityTypeID] {
			result = append(result, map[string]any{
				"entityTypeId": bi.EntityTypeID,
				"title":        bi.Title,
				"isDynamic":    bi.IsDynamic,
			})
		}
	}
	result = append(result, dynamic...)

	_ = s.store.SetSchemaCache(ctx, connectionID, "entity-types", result)
	return result, nil
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

	resp, err := s.client.Call(ctx, connectionID, "crm.item.fields", map[string]any{
		"entityTypeId": entityTypeID,
	})
	if err != nil {
		return nil, err
	}
	var resObj struct {
		Fields map[string]any `json:"fields"`
	}
	_ = json.Unmarshal(resp.Result, &resObj)
	fields := resObj.Fields
	if fields == nil {
		fields = make(map[string]any)
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

	cats, err := s.collect(ctx, connectionID, "crm.category.list", map[string]any{
		"entityTypeId": entityTypeID,
	}, "categories")
	if err != nil {
		return nil, err
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

	statusEntityID := stageEntityID(entityTypeID, categoryID)
	stages, err := s.collect(ctx, connectionID, "crm.status.list", map[string]any{
		"filter": map[string]any{"ENTITY_ID": statusEntityID},
		"order":  map[string]any{"SORT": "ASC"},
	}, "")
	if err != nil {
		return nil, err
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
		loaded, _ := s.Stages(ctx, connectionID, entityTypeID, cID, refresh)
		stagesByCategory[cID] = loaded
	} else if len(uncachedCatIDs) > 1 {
		var commands []NamedBatchCommand
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
		batchRes, batchErr := s.client.CallBatch(ctx, connectionID, commands, false)
		if batchErr == nil {
			for _, cID := range uncachedCatIDs {
				key := fmt.Sprintf("cat_%d", cID)
				var list []map[string]any
				if raw, ok := batchRes[key]; ok {
					_ = json.Unmarshal(raw, &list)
				}
				if list == nil {
					list = []map[string]any{}
				}
				_ = s.store.SetSchemaCache(ctx, connectionID, fmt.Sprintf("stages:%d:%d", entityTypeID, cID), list)
				stagesByCategory[cID] = list
			}
		} else {
			for _, cID := range uncachedCatIDs {
				stagesByCategory[cID] = []map[string]any{}
			}
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
		if normalizeName(title) == normalized || idStr == normalized {
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
		var list []map[string]any
		if err := json.Unmarshal(raw, &list); err == nil {
			return list
		}
		return nil
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
	return fmt.Sprintf("DYNAMIC_%d_STAGE_%d", entityTypeID, categoryID)
}

func normalizeName(value string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	result, _, _ := transform.String(t, value)
	return strings.ToLower(strings.TrimSpace(result))
}
