import type { ConnectionRecord } from "../types.js";
import { PolicyError } from "../errors.js";

export type Capability = "read" | "write" | "admin" | "dangerous";

export class PolicyGuard {
  assert(connection: ConnectionRecord, capability: Capability, confirmed = false): void {
    if (connection.status !== "active") {
      throw new PolicyError(`Connection '${connection.id}' is ${connection.status}`);
    }
    if (capability === "read") return;
    if (capability === "dangerous") {
      throw new PolicyError("Dangerous raw Bitrix methods are not exposed by this server");
    }
    if (connection.mode === "read_only") {
      throw new PolicyError("This connection is read-only; reconnect it in write_with_confirmation mode to prepare writes");
    }
    if (capability === "admin" && !connection.adminToolsEnabled) {
      throw new PolicyError("Administrative tools are disabled for this connection");
    }
    if (confirmed) return;
    throw new PolicyError("This operation requires explicit confirmation");
  }

  assertCanPrepareWrite(connection: ConnectionRecord): void {
    if (connection.status !== "active") throw new PolicyError(`Connection '${connection.id}' is ${connection.status}`);
    if (connection.mode !== "write_with_confirmation") {
      throw new PolicyError("This connection is read-only; write operations cannot be prepared");
    }
  }
}
