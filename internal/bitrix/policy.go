package bitrix

import (
	"fmt"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
)

func AssertCanPrepareWrite(conn *store.Connection) error {
	if conn.Status != store.StatusActive {
		return config.NewPolicyError(fmt.Sprintf("Connection '%s' is %s", conn.ID, conn.Status))
	}
	if conn.Mode != config.ModeWriteWithConfirmation {
		return config.NewPolicyError("This connection is read-only; write operations cannot be prepared")
	}
	return nil
}

func AssertCanExecuteWrite(conn *store.Connection, confirmed bool) error {
	if conn.Status != store.StatusActive {
		return config.NewPolicyError(fmt.Sprintf("Connection '%s' is %s", conn.ID, conn.Status))
	}
	if conn.Mode == config.ModeReadOnly {
		return config.NewPolicyError("This connection is read-only; reconnect it in write_with_confirmation mode to prepare writes")
	}
	if !confirmed {
		return config.NewPolicyError("This operation requires explicit confirmation")
	}
	return nil
}
