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
	guard  *PolicyGuard
	client *RestClient
}

func NewOperationService(
	cfg *config.Config,
	st *store.Store,
	guard *PolicyGuard,
	client *RestClient,
) *OperationService {
	return &OperationService{
		cfg:    cfg,
		store:  st,
		guard:  guard,
		client: client,
	}
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
		"fields":       fields,
	}
	summary := fmt.Sprintf("Create an item in CRM entity type %d.", entityTypeID)
	return s.createPending(ctx, connectionID, string(store.OpCreateItem), "crm.item.add", params, summary)
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
	if len(fields) == 0 {
		return nil, config.NewAppError("At least one field is required", "EMPTY_FIELDS", 400)
	}
	params := map[string]any{
		"entityTypeId": entityTypeID,
		"id":           id,
		"fields":       fields,
	}
	summary := fmt.Sprintf("Update item #%d in CRM entity type %d.", id, entityTypeID)
	return s.createPending(ctx, connectionID, string(store.OpUpdateItem), "crm.item.update", params, summary)
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
	params := map[string]any{
		"entityTypeId": entityTypeID,
		"id":           id,
		"fields":       map[string]any{"stageId": stageID},
	}
	summary := fmt.Sprintf("Move item #%d in CRM entity type %d to stage '%s'.", id, entityTypeID, stageID)
	return s.createPending(ctx, connectionID, string(store.OpMoveItemStage), "crm.item.update", params, summary)
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
	if err := s.guard.Assert(conn, CapabilityWrite, true); err != nil {
		return nil, err
	}

	claimed, err := s.store.ClaimPendingOperation(ctx, operationID)
	if err != nil {
		return nil, err
	}
	if claimed == nil {
		return nil, config.NewAppError("Operation is not pending or has expired", "OPERATION_NOT_CONFIRMABLE", 409)
	}

	resp, err := s.client.Call(ctx, claimed.ConnectionID, claimed.Method, claimed.Params)
	if err != nil {
		msg := config.SafeErrorMessage(err)
		_ = s.store.FailPendingOperation(ctx, claimed.ID, msg)
		_ = s.store.RecordAuditLog(ctx, store.RecordAuditLogInput{
			ConnectionID: claimed.ConnectionID,
			OperationID:  claimed.ID,
			Action:       claimed.OperationType,
			Outcome:      string(store.OpStatusFailed),
			Details:      map[string]any{"method": claimed.Method, "error": msg},
		})
		return nil, err
	}

	var resVal any
	if err := json.Unmarshal(resp.Result, &resVal); err != nil {
		resVal = resp.Result
	}

	_ = s.store.CompletePendingOperation(ctx, claimed.ID, resVal)
	_ = s.store.RecordAuditLog(ctx, store.RecordAuditLogInput{
		ConnectionID: claimed.ConnectionID,
		OperationID:  claimed.ID,
		Action:       claimed.OperationType,
		Outcome:      string(store.OpStatusCompleted),
		Details:      map[string]any{"method": claimed.Method, "summary": claimed.HumanSummary},
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
		Action:       cancelled.OperationType,
		Outcome:      "cancelled",
	})

	return map[string]any{
		"operationId": cancelled.ID,
		"status":      cancelled.Status,
		"summary":     cancelled.HumanSummary,
	}, nil
}

func (s *OperationService) assertPrepare(ctx context.Context, connectionID string) error {
	conn, err := s.store.GetConnection(ctx, connectionID)
	if err != nil {
		return err
	}
	return s.guard.AssertCanPrepareWrite(conn)
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
	operationType string,
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
		Action:       operationType,
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
