import { describe, expect, it } from "vitest";
import { PolicyGuard } from "../src/policy/guard.js";
import type { ConnectionRecord } from "../src/types.js";

function connection(overrides: Partial<ConnectionRecord> = {}): ConnectionRecord {
  return {
    id: "00000000-0000-4000-8000-000000000001",
    domain: "example.bitrix24.com",
    memberId: "member",
    bitrixUserId: "1",
    clientEndpoint: "https://example.bitrix24.com/rest/",
    accessTokenEncrypted: "encrypted",
    refreshTokenEncrypted: "encrypted",
    scope: "crm",
    expiresAt: new Date(Date.now() + 60_000),
    mode: "read_only",
    adminToolsEnabled: false,
    status: "active",
    createdAt: new Date(),
    updatedAt: new Date(),
    ...overrides,
  };
}

describe("PolicyGuard", () => {
  const guard = new PolicyGuard();
  it("allows reads on an active read-only connection", () => expect(() => guard.assert(connection(), "read")).not.toThrow());
  it("denies writes and preparation in read-only mode", () => {
    expect(() => guard.assert(connection(), "write", true)).toThrow(/read-only/i);
    expect(() => guard.assertCanPrepareWrite(connection())).toThrow(/read-only/i);
  });
  it("requires confirmation for write-capable connections", () => {
    const writable = connection({ mode: "write_with_confirmation" });
    expect(() => guard.assertCanPrepareWrite(writable)).not.toThrow();
    expect(() => guard.assert(writable, "write")).toThrow(/confirmation/i);
    expect(() => guard.assert(writable, "write", true)).not.toThrow();
  });
  it("always denies dangerous calls and gated admin calls", () => {
    const writable = connection({ mode: "write_with_confirmation" });
    expect(() => guard.assert(writable, "dangerous", true)).toThrow(/not exposed/i);
    expect(() => guard.assert(writable, "admin", true)).toThrow(/disabled/i);
  });
});
