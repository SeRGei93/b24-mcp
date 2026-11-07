import type { AppConfig } from "../config/env.js";
import type { AuditRepository, ConnectionRepository, PendingOperationRepository } from "../db/repositories.js";
import { AppError, PolicyError, safeErrorMessage } from "../errors.js";
import { PolicyGuard } from "../policy/guard.js";
import { BitrixRestClient } from "../bitrix/rest-client.js";

export class OperationService {
  constructor(
    private readonly config: AppConfig,
    private readonly connections: ConnectionRepository,
    private readonly operations: PendingOperationRepository,
    private readonly audit: AuditRepository,
    private readonly guard: PolicyGuard,
    private readonly bitrix: BitrixRestClient,
  ) {}

  async prepareCreate(connectionId: string, entityTypeId: number, fields: Record<string, unknown>) {
    await this.assertPrepare(connectionId);
    if (!Object.keys(fields).length) throw new AppError("At least one field is required", "EMPTY_FIELDS");
    return this.createPending(connectionId, "create_item", "crm.item.add", { entityTypeId, fields },
      `Create an item in CRM entity type ${entityTypeId}.`);
  }

  async prepareUpdate(connectionId: string, entityTypeId: number, id: number, fields: Record<string, unknown>) {
    await this.assertPrepare(connectionId);
    if (!Object.keys(fields).length) throw new AppError("At least one field is required", "EMPTY_FIELDS");
    return this.createPending(connectionId, "update_item", "crm.item.update", { entityTypeId, id, fields },
      `Update item #${id} in CRM entity type ${entityTypeId}.`);
  }

  async prepareMove(connectionId: string, entityTypeId: number, id: number, stageId: string) {
    await this.assertPrepare(connectionId);
    return this.createPending(connectionId, "move_item_stage", "crm.item.update", { entityTypeId, id, fields: { stageId } },
      `Move item #${id} in CRM entity type ${entityTypeId} to stage '${stageId}'.`);
  }

  async confirm(operationId: string, authorizedConnectionId?: string) {
    const current = await this.operations.get(operationId);
    this.assertOperationAccess(current.connectionId, authorizedConnectionId);
    const connection = await this.connections.get(current.connectionId);
    this.guard.assert(connection, "write", true);
    const claimed = await this.operations.claim(operationId);
    if (!claimed) throw new AppError("Operation is not pending or has expired", "OPERATION_NOT_CONFIRMABLE", 409);
    try {
      const response = await this.bitrix.call(claimed.connectionId, claimed.method, claimed.params);
      await this.operations.complete(claimed.id, response.result);
      await this.audit.record({
        connectionId: claimed.connectionId,
        operationId: claimed.id,
        action: claimed.operationType,
        outcome: "completed",
        details: { method: claimed.method, summary: claimed.humanSummary },
      });
      return { operationId: claimed.id, status: "completed", summary: claimed.humanSummary, result: response.result };
    } catch (error) {
      const message = safeErrorMessage(error);
      await this.operations.fail(claimed.id, message);
      await this.audit.record({
        connectionId: claimed.connectionId,
        operationId: claimed.id,
        action: claimed.operationType,
        outcome: "failed",
        details: { method: claimed.method, error: message },
      });
      throw error;
    }
  }

  async cancel(operationId: string, authorizedConnectionId?: string) {
    const current = await this.operations.get(operationId);
    this.assertOperationAccess(current.connectionId, authorizedConnectionId);
    const cancelled = await this.operations.cancel(operationId);
    if (!cancelled) throw new AppError("Operation is not pending and cannot be cancelled", "OPERATION_NOT_CANCELLABLE", 409);
    await this.audit.record({
      connectionId: cancelled.connectionId,
      operationId: cancelled.id,
      action: cancelled.operationType,
      outcome: "cancelled",
    });
    return { operationId: cancelled.id, status: cancelled.status, summary: cancelled.humanSummary };
  }

  async list(connectionId?: string) {
    return this.operations.list(connectionId);
  }

  private async assertPrepare(connectionId: string): Promise<void> {
    this.guard.assertCanPrepareWrite(await this.connections.get(connectionId));
  }

  private assertOperationAccess(connectionId: string, authorizedConnectionId?: string): void {
    if (authorizedConnectionId && connectionId !== authorizedConnectionId) {
      throw new PolicyError("This MCP token cannot access an operation from another Bitrix24 connection");
    }
  }

  private async createPending(
    connectionId: string,
    operationType: "create_item" | "update_item" | "move_item_stage",
    method: string,
    params: Record<string, unknown>,
    summary: string,
  ) {
    const operation = await this.operations.create({
      connectionId,
      operationType,
      method,
      params,
      humanSummary: summary,
      expiresAt: new Date(Date.now() + this.config.pendingOperationTtlSeconds * 1_000),
    });
    await this.audit.record({
      connectionId,
      operationId: operation.id,
      action: operationType,
      outcome: "prepared",
      details: { method, summary },
    });
    return {
      requires_confirmation: true,
      operation_id: operation.id,
      summary,
      expires_at: operation.expiresAt.toISOString(),
    };
  }
}
