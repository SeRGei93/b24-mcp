package bitrix

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
)

type OperationService struct {
	cfg    *config.Config
	store  *store.Store
	client *RestClient
}

func NewOperationService(
	cfg *config.Config,
	st *store.Store,
	client *RestClient,
) *OperationService {
	return &OperationService{
		cfg:    cfg,
		store:  st,
		client: client,
	}
}

func normalizeFieldsMap(fields map[string]any) map[string]any {
	norm := make(map[string]any, len(fields))
	for k, v := range fields {
		norm[normalizeFieldName(k)] = v
	}
	return norm
}

func (s *OperationService) PrepareCreate(
	ctx context.Context,
	connectionID string,
	entityTypeID int,
	fields map[string]any,
) (map[string]any, error) {
	if err := s.assertPrepare(ctx, connectionID); err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, config.NewAppError("At least one field is required", "EMPTY_FIELDS", 400)
	}
	params := map[string]any{
		"entityTypeId": entityTypeID,
		"fields":       normalizeFieldsMap(fields),
	}
	summary := fmt.Sprintf("Create an item in CRM entity type %d.", entityTypeID)
	return s.createPending(ctx, connectionID, store.OpCreateItem, "crm.item.add", params, summary)
}

func (s *OperationService) PrepareUpdate(
	ctx context.Context,
	connectionID string,
	entityTypeID int,
	id int,
	fields map[string]any,
) (map[string]any, error) {
	if err := s.assertPrepare(ctx, connectionID); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, config.NewAppError("Valid item id is required", "INVALID_ITEM_ID", 400)
	}
	if len(fields) == 0 {
		return nil, config.NewAppError("At least one field is required", "EMPTY_FIELDS", 400)
	}
	params := map[string]any{
		"entityTypeId": entityTypeID,
		"id":           id,
		"fields":       normalizeFieldsMap(fields),
	}
	summary := fmt.Sprintf("Update item #%d in CRM entity type %d.", id, entityTypeID)
	return s.createPending(ctx, connectionID, store.OpUpdateItem, "crm.item.update", params, summary)
}

func (s *OperationService) PrepareMove(
	ctx context.Context,
	connectionID string,
	entityTypeID int,
	id int,
	stageID string,
) (map[string]any, error) {
	if err := s.assertPrepare(ctx, connectionID); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, config.NewAppError("Valid item id is required", "INVALID_ITEM_ID", 400)
	}
	if stageID == "" {
		return nil, config.NewAppError("stageId is required", "MISSING_STAGE_ID", 400)
	}
	params := map[string]any{
		"entityTypeId": entityTypeID,
		"id":           id,
		"fields":       map[string]any{"stageId": stageID},
	}
	summary := fmt.Sprintf("Move item #%d in CRM entity type %d to stage '%s'.", id, entityTypeID, stageID)
	return s.createPending(ctx, connectionID, store.OpMoveItemStage, "crm.item.update", params, summary)
}

func (s *OperationService) PrepareDelete(
	ctx context.Context,
	connectionID string,
	entityTypeID int,
	id int,
) (map[string]any, error) {
	if err := s.assertPrepare(ctx, connectionID); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, config.NewAppError("Valid item id is required", "INVALID_ITEM_ID", 400)
	}
	params := map[string]any{
		"entityTypeId": entityTypeID,
		"id":           id,
	}
	summary := fmt.Sprintf("Delete item #%d in CRM entity type %d.", id, entityTypeID)
	return s.createPending(ctx, connectionID, store.OpDeleteItem, "crm.item.delete", params, summary)
}

func (s *OperationService) failOperation(ctx context.Context, claimed *store.PendingOperation, method string, callErr error) error {
	msg := config.SafeErrorMessage(callErr)
	_ = s.store.FailPendingOperation(ctx, claimed.ID, msg)
	_ = s.store.RecordAuditLog(ctx, store.RecordAuditLogInput{
		ConnectionID: claimed.ConnectionID,
		OperationID:  claimed.ID,
		Action:       string(claimed.OperationType),
		Outcome:      string(store.OpStatusFailed),
		Details:      map[string]any{"method": method, "error": msg},
	})
	return callErr
}

type operationDispatch struct {
	action     string
	v3Params   map[string]any
	v2Params   map[string]any
	rawMethod  string
	rawPayload map[string]any
}

func buildOperationDispatch(claimed *store.PendingOperation) operationDispatch {
	entityTypeID := ToInt(claimed.Params["entityTypeId"])
	id := ToInt(claimed.Params["id"])
	rawFields, _ := claimed.Params["fields"].(map[string]any)
	normFields := normalizeFieldsMap(rawFields)

	switch claimed.OperationType {
	case store.OpCreateItem:
		return operationDispatch{
			action:   "add",
			v3Params: map[string]any{"fields": normFields},
			v2Params: map[string]any{"entityTypeId": entityTypeID, "fields": normFields},
		}
	case store.OpUpdateItem, store.OpMoveItemStage:
		return operationDispatch{
			action:   "update",
			v3Params: map[string]any{"id": id, "fields": normFields},
			v2Params: map[string]any{"entityTypeId": entityTypeID, "id": id, "fields": normFields},
		}
	case store.OpDeleteItem:
		return operationDispatch{
			action:   "delete",
			v3Params: map[string]any{"id": id},
			v2Params: map[string]any{"entityTypeId": entityTypeID, "id": id},
		}
	default:
		return operationDispatch{
			rawMethod:  claimed.Method,
			rawPayload: claimed.Params,
		}
	}
}

func (s *OperationService) Confirm(
	ctx context.Context,
	operationID string,
	authorizedConnectionID string,
) (map[string]any, error) {
	current, err := s.store.GetPendingOperation(ctx, operationID)
	if err != nil {
		return nil, err
	}
	if err := s.assertOperationAccess(current.ConnectionID, authorizedConnectionID); err != nil {
		return nil, err
	}
	conn, err := s.store.GetConnection(ctx, current.ConnectionID)
	if err != nil {
		return nil, err
	}
	if err := AssertCanExecuteWrite(conn, true); err != nil {
		return nil, err
	}

	claimed, err := s.store.ClaimPendingOperation(ctx, operationID)
	if err != nil {
		return nil, err
	}
	if claimed == nil {
		return nil, config.NewAppError("Operation is not pending or has expired", "OPERATION_NOT_CONFIRMABLE", 409)
	}

	entityTypeID := ToInt(claimed.Params["entityTypeId"])
	slug, slugErr := entitySlug(entityTypeID)
	dispatch := buildOperationDispatch(claimed)

	var (
		resp       *BitrixResponse
		callErr    error
		usedMethod string
	)

	if dispatch.action != "" && !s.client.IsV2Mode(ctx, claimed.ConnectionID) && slugErr == nil {
		usedMethod = fmt.Sprintf("crm.%s.%s", slug, dispatch.action)
		resp, callErr = s.client.Call(ctx, claimed.ConnectionID, usedMethod, dispatch.v3Params, WithIdempotencyKey(claimed.ID))
		if callErr != nil {
			if s.client.ShouldFallbackToV2(callErr) {
				resp = nil
			} else {
				return nil, s.failOperation(ctx, claimed, usedMethod, callErr)
			}
		}
	}

	if resp == nil {
		legacyMethod := dispatch.rawMethod
		legacyParams := dispatch.rawPayload
		if dispatch.action != "" {
			legacyMethod = "crm.item." + dispatch.action
			legacyParams = dispatch.v2Params
		}
		usedMethod = legacyMethod
		resp, callErr = s.client.Call(ctx, claimed.ConnectionID, legacyMethod, legacyParams, WithIdempotencyKey(claimed.ID))
		if callErr != nil {
			return nil, s.failOperation(ctx, claimed, legacyMethod, callErr)
		}
	}

	var resVal any
	if err := json.Unmarshal(resp.Result, &resVal); err != nil {
		resVal = resp.Result
	}

	_ = s.store.CompletePendingOperation(ctx, claimed.ID, resVal)
	_ = s.store.RecordAuditLog(ctx, store.RecordAuditLogInput{
		ConnectionID: claimed.ConnectionID,
		OperationID:  claimed.ID,
		Action:       string(claimed.OperationType),
		Outcome:      string(store.OpStatusCompleted),
		Details:      map[string]any{"method": usedMethod, "summary": claimed.HumanSummary},
	})

	return map[string]any{
		"operationId": claimed.ID,
		"status":      string(store.OpStatusCompleted),
		"summary":     claimed.HumanSummary,
		"result":      resVal,
	}, nil
}

func (s *OperationService) Cancel(
	ctx context.Context,
	operationID string,
	authorizedConnectionID string,
) (map[string]any, error) {
	current, err := s.store.GetPendingOperation(ctx, operationID)
	if err != nil {
		return nil, err
	}
	if err := s.assertOperationAccess(current.ConnectionID, authorizedConnectionID); err != nil {
		return nil, err
	}
	cancelled, err := s.store.CancelPendingOperation(ctx, operationID)
	if err != nil {
		return nil, err
	}
	if cancelled == nil {
		return nil, config.NewAppError("Operation is not pending and cannot be cancelled", "OPERATION_NOT_CANCELLABLE", 409)
	}

	_ = s.store.RecordAuditLog(ctx, store.RecordAuditLogInput{
		ConnectionID: cancelled.ConnectionID,
		OperationID:  cancelled.ID,
		Action:       string(cancelled.OperationType),
		Outcome:      string(store.OpStatusCancelled),
	})

	return map[string]any{
		"operationId": cancelled.ID,
		"status":      string(cancelled.Status),
		"summary":     cancelled.HumanSummary,
	}, nil
}

func (s *OperationService) assertPrepare(ctx context.Context, connectionID string) error {
	conn, err := s.store.GetConnection(ctx, connectionID)
	if err != nil {
		return err
	}
	return AssertCanPrepareWrite(conn)
}

func (s *OperationService) assertOperationAccess(connectionID, authorizedConnectionID string) error {
	if authorizedConnectionID != "" && connectionID != authorizedConnectionID {
		return config.NewPolicyError("This MCP token cannot access an operation from another Bitrix24 connection")
	}
	return nil
}

func (s *OperationService) createPending(
	ctx context.Context,
	connectionID string,
	operationType store.OperationType,
	method string,
	params map[string]any,
	summary string,
) (map[string]any, error) {
	exp := time.Now().Add(time.Duration(s.cfg.PendingOperationTTLSeconds) * time.Second)
	op, err := s.store.CreatePendingOperation(ctx, store.CreatePendingOperationInput{
		ConnectionID:  connectionID,
		OperationType: operationType,
		Method:        method,
		Params:        params,
		HumanSummary:  summary,
		ExpiresAt:     exp,
	})
	if err != nil {
		return nil, err
	}

	_ = s.store.RecordAuditLog(ctx, store.RecordAuditLogInput{
		ConnectionID: connectionID,
		OperationID:  op.ID,
		Action:       string(operationType),
		Outcome:      "prepared",
		Details:      map[string]any{"method": method, "summary": summary},
	})

	return map[string]any{
		"requires_confirmation": true,
		"operation_id":          op.ID,
		"summary":               summary,
		"expires_at":            op.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}, nil
}
