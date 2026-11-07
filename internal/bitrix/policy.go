package bitrix

import (
	"fmt"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/store"
)

type Capability string

const (
	CapabilityRead      Capability = "read"
	CapabilityWrite     Capability = "write"
	CapabilityAdmin     Capability = "admin"
	CapabilityDangerous Capability = "dangerous"
)

type PolicyGuard struct{}

func NewPolicyGuard() *PolicyGuard {
	return &PolicyGuard{}
}

func (g *PolicyGuard) Assert(conn *store.Connection, capability Capability, confirmed bool) error {
	if conn.Status != "active" {
		return config.NewPolicyError(fmt.Sprintf("Connection '%s' is %s", conn.ID, conn.Status))
	}
	if capability == CapabilityRead {
		return nil
	}
	if capability == CapabilityDangerous {
		return config.NewPolicyError("Dangerous raw Bitrix methods are not exposed by this server")
	}
	if conn.Mode == config.ModeReadOnly {
		return config.NewPolicyError("This connection is read-only; reconnect it in write_with_confirmation mode to prepare writes")
	}
	if capability == CapabilityAdmin && !conn.AdminToolsEnabled {
		return config.NewPolicyError("Administrative tools are disabled for this connection")
	}
	if confirmed {
		return nil
	}
	return config.NewPolicyError("This operation requires explicit confirmation")
}

func (g *PolicyGuard) AssertCanPrepareWrite(conn *store.Connection) error {
	if conn.Status != "active" {
		return config.NewPolicyError(fmt.Sprintf("Connection '%s' is %s", conn.ID, conn.Status))
	}
	if conn.Mode != config.ModeWriteWithConfirmation {
		return config.NewPolicyError("This connection is read-only; write operations cannot be prepared")
	}
	return nil
}
