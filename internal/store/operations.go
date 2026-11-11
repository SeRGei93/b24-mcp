package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/google/uuid"
)

type OperationType string

const (
	OpCreateItem    OperationType = "create_item"
	OpUpdateItem    OperationType = "update_item"
	OpMoveItemStage OperationType = "move_item_stage"
	OpDeleteItem    OperationType = "delete_item"
)

type OperationStatus string

const (
	OpStatusPending   OperationStatus = "pending"
	OpStatusExecuting OperationStatus = "executing"
	OpStatusCompleted OperationStatus = "completed"
	OpStatusCancelled OperationStatus = "cancelled"
	OpStatusExpired   OperationStatus = "expired"
	OpStatusFailed    OperationStatus = "failed"
)

type PendingOperation struct {
	ID            string          `json:"id"`
	ConnectionID  string          `json:"connectionId"`
	OperationType OperationType   `json:"operationType"`
	Method        string          `json:"method"`
	ParamsJSON    string          `json:"-"`
	Params        map[string]any  `json:"params"`
	HumanSummary  string          `json:"humanSummary"`
	Status        OperationStatus `json:"status"`
	ResultJSON    string          `json:"-"`
	Result        any             `json:"result"`
	ErrorMessage  string          `json:"errorMessage,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
	ExpiresAt     time.Time       `json:"expiresAt"`
	ConfirmedAt   *time.Time      `json:"confirmedAt,omitempty"`
	CompletedAt   *time.Time      `json:"completedAt,omitempty"`
}

type CreatePendingOperationInput struct {
	ConnectionID  string
	OperationType OperationType
	Method        string
	Params        map[string]any
	HumanSummary  string
	ExpiresAt     time.Time
}

type RecordAuditLogInput struct {
	ConnectionID string
	OperationID  string
	Action       string
	Outcome      string
	Details      map[string]any
}

func (s *Store) CreatePendingOperation(ctx context.Context, input CreatePendingOperationInput) (*PendingOperation, error) {
	paramsBytes, err := json.Marshal(input.Params)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	now := time.Now().UTC().Format(isoTimeFormat)
	exp := input.ExpiresAt.UTC().Format(isoTimeFormat)

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO pending_operations (
			id, connection_id, operation_type, method, params_json, human_summary,
			status, created_at, expires_at
		) VALUES (?1,?2,?3,?4,?5,?6,'pending',?7,?8)
	`,
		id,
		input.ConnectionID,
		string(input.OperationType),
		input.Method,
		string(paramsBytes),
		input.HumanSummary,
		now,
		exp,
	)
	if err != nil {
		return nil, err
	}
	return s.GetPendingOperation(ctx, id)
}

func (s *Store) GetPendingOperation(ctx context.Context, id string) (*PendingOperation, error) {
	_ = s.expireStalePendingOperations(ctx)
	row := s.db.QueryRowContext(ctx, `
		SELECT id, connection_id, operation_type, method, params_json, human_summary,
		       status, result_json, error_message, created_at, expires_at, confirmed_at, completed_at
		FROM pending_operations
		WHERE id = ?1
	`, id)
	op, err := scanPendingOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, config.NewNotFoundError("Pending operation was not found")
	}
	return op, err
}

func (s *Store) ListPendingOperations(ctx context.Context, connectionID *string) ([]PendingOperation, error) {
	_ = s.expireStalePendingOperations(ctx)
	var rows *sql.Rows
	var err error
	if connectionID != nil && *connectionID != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, connection_id, operation_type, method, params_json, human_summary,
			       status, result_json, error_message, created_at, expires_at, confirmed_at, completed_at
			FROM pending_operations
			WHERE connection_id = ?1
			ORDER BY created_at DESC
			LIMIT 100
		`, *connectionID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, connection_id, operation_type, method, params_json, human_summary,
			       status, result_json, error_message, created_at, expires_at, confirmed_at, completed_at
			FROM pending_operations
			ORDER BY created_at DESC
			LIMIT 100
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []PendingOperation
	for rows.Next() {
		op, err := scanPendingOperation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *op)
	}
	return result, rows.Err()
}

func (s *Store) ClaimPendingOperation(ctx context.Context, id string) (*PendingOperation, error) {
	now := time.Now().UTC().Format(isoTimeFormat)
	row := s.db.QueryRowContext(ctx, `
		UPDATE pending_operations
		SET status = 'executing', confirmed_at = ?2
		WHERE id = ?1 AND status = 'pending' AND expires_at > ?2
		RETURNING id, connection_id, operation_type, method, params_json, human_summary,
		          status, result_json, error_message, created_at, expires_at, confirmed_at, completed_at
	`, id, now)
	op, err := scanPendingOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return op, err
}

func (s *Store) CompletePendingOperation(ctx context.Context, id string, result any) error {
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(isoTimeFormat)
	_, err = s.db.ExecContext(ctx, `
		UPDATE pending_operations
		SET status = 'completed', result_json = ?2, completed_at = ?3
		WHERE id = ?1 AND status = 'executing'
	`, id, string(resultBytes), now)
	return err
}

func (s *Store) FailPendingOperation(ctx context.Context, id string, errorMessage string) error {
	now := time.Now().UTC().Format(isoTimeFormat)
	if len(errorMessage) > 1000 {
		errorMessage = errorMessage[:1000]
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE pending_operations
		SET status = 'failed', error_message = ?2, completed_at = ?3
		WHERE id = ?1 AND status = 'executing'
	`, id, errorMessage, now)
	return err
}

func (s *Store) CancelPendingOperation(ctx context.Context, id string) (*PendingOperation, error) {
	now := time.Now().UTC().Format(isoTimeFormat)
	row := s.db.QueryRowContext(ctx, `
		UPDATE pending_operations
		SET status = 'cancelled', completed_at = ?2
		WHERE id = ?1 AND status = 'pending'
		RETURNING id, connection_id, operation_type, method, params_json, human_summary,
		          status, result_json, error_message, created_at, expires_at, confirmed_at, completed_at
	`, id, now)
	op, err := scanPendingOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return op, err
}

func (s *Store) RecordAuditLog(ctx context.Context, input RecordAuditLogInput) error {
	detailsJSON, err := json.Marshal(input.Details)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(isoTimeFormat)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO audit_logs (id, connection_id, operation_id, action, outcome, details_json, created_at)
		VALUES (?1,?2,?3,?4,?5,?6,?7)
	`,
		uuid.NewString(),
		nullableString(input.ConnectionID),
		nullableString(input.OperationID),
		input.Action,
		input.Outcome,
		string(detailsJSON),
		now,
	)
	return err
}

func (s *Store) expireStalePendingOperations(ctx context.Context) error {
	now := time.Now().UTC().Format(isoTimeFormat)
	_, err := s.db.ExecContext(ctx, `
		UPDATE pending_operations
		SET status = 'expired', completed_at = ?1
		WHERE status = 'pending' AND expires_at <= ?1
	`, now)
	return err
}

func scanPendingOperation(scanner rowScanner) (*PendingOperation, error) {
	var (
		op                                               PendingOperation
		opTypeStr, statusStr                             string
		resultJSON, errorMsg, confirmedStr, completedStr sql.NullString
		createdStr, expiresStr                           string
	)
	err := scanner.Scan(
		&op.ID,
		&op.ConnectionID,
		&opTypeStr,
		&op.Method,
		&op.ParamsJSON,
		&op.HumanSummary,
		&statusStr,
		&resultJSON,
		&errorMsg,
		&createdStr,
		&expiresStr,
		&confirmedStr,
		&completedStr,
	)
	if err != nil {
		return nil, err
	}
	op.OperationType = OperationType(opTypeStr)
	op.Status = OperationStatus(statusStr)
	op.ResultJSON = resultJSON.String
	if op.ParamsJSON != "" {
		_ = json.Unmarshal([]byte(op.ParamsJSON), &op.Params)
	}
	if op.Params == nil {
		op.Params = map[string]any{}
	}
	if resultJSON.Valid && resultJSON.String != "" {
		var parsedResult any
		if err := json.Unmarshal([]byte(resultJSON.String), &parsedResult); err == nil {
			op.Result = parsedResult
		}
	}
	op.ErrorMessage = errorMsg.String
	op.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	op.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expiresStr)
	if confirmedStr.Valid && confirmedStr.String != "" {
		t, _ := time.Parse(time.RFC3339Nano, confirmedStr.String)
		op.ConfirmedAt = &t
	}
	if completedStr.Valid && completedStr.String != "" {
		t, _ := time.Parse(time.RFC3339Nano, completedStr.String)
		op.CompletedAt = &t
	}
	return &op, nil
}
