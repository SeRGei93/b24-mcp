export const CONNECTION_MODES = ["read_only", "write_with_confirmation"] as const;

export type ConnectionMode = (typeof CONNECTION_MODES)[number];

export interface ConnectionRecord {
  id: string;
  domain: string;
  memberId: string;
  bitrixUserId: string;
  clientEndpoint: string;
  accessTokenEncrypted: string;
  refreshTokenEncrypted: string;
  scope: string;
  expiresAt: Date;
  mode: ConnectionMode;
  adminToolsEnabled: boolean;
  status: "active" | "disabled" | "revoked";
  createdAt: Date;
  updatedAt: Date;
}

export interface PublicConnection {
  id: string;
  domain: string;
  memberId: string;
  bitrixUserId: string;
  scope: string;
  expiresAt: string;
  mode: ConnectionMode;
  adminToolsEnabled: boolean;
  status: ConnectionRecord["status"];
  createdAt: string;
  updatedAt: string;
}

export interface OAuthStateRecord {
  domain: string;
  mode: ConnectionMode;
  mcpRequestHash?: string;
}

export type OperationType = "create_item" | "update_item" | "move_item_stage";
export type OperationStatus = "pending" | "executing" | "completed" | "cancelled" | "expired" | "failed";

export interface PendingOperationRecord {
  id: string;
  connectionId: string;
  operationType: OperationType;
  method: string;
  params: Record<string, unknown>;
  humanSummary: string;
  status: OperationStatus;
  result: unknown;
  errorMessage: string | null;
  createdAt: Date;
  expiresAt: Date;
  confirmedAt: Date | null;
  completedAt: Date | null;
}

export interface BitrixResponse<T = unknown> {
  result: T;
  next?: number;
  total?: number;
  time?: Record<string, unknown>;
}

export interface AppLogger {
  debug(bindings: Record<string, unknown>, message?: string): void;
  info(bindings: Record<string, unknown>, message?: string): void;
  warn(bindings: Record<string, unknown>, message?: string): void;
  error(bindings: Record<string, unknown>, message?: string): void;
}

export function toPublicConnection(connection: ConnectionRecord): PublicConnection {
  return {
    id: connection.id,
    domain: connection.domain,
    memberId: connection.memberId,
    bitrixUserId: connection.bitrixUserId,
    scope: connection.scope,
    expiresAt: connection.expiresAt.toISOString(),
    mode: connection.mode,
    adminToolsEnabled: connection.adminToolsEnabled,
    status: connection.status,
    createdAt: connection.createdAt.toISOString(),
    updatedAt: connection.updatedAt.toISOString(),
  };
}
