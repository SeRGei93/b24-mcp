import type { AppConfig } from "./config/env.js";
import type { AppLogger } from "./types.js";
import { createPool, type DbPool } from "./db/client.js";
import {
  AuditRepository,
  ConnectionRepository,
  OAuthStateRepository,
  PendingOperationRepository,
  SchemaCacheRepository,
} from "./db/repositories.js";
import { TokenCipher } from "./security/encryption.js";
import { BitrixOAuthService } from "./bitrix/oauth.js";
import { TokenManager } from "./bitrix/token-manager.js";
import { BitrixRestClient } from "./bitrix/rest-client.js";
import { SchemaDiscoveryService } from "./bitrix/schema-discovery.js";
import { CrmService } from "./bitrix/crm.js";
import { PolicyGuard } from "./policy/guard.js";
import { OperationService } from "./operations/service.js";
import { SqliteMcpOAuthStore } from "./mcp/oauth/repository.js";
import { McpOAuthService } from "./mcp/oauth/service.js";

export interface AppContainer {
  config: AppConfig;
  pool: DbPool;
  connections: ConnectionRepository;
  oauth: BitrixOAuthService;
  tokens: TokenManager;
  bitrix: BitrixRestClient;
  schema: SchemaDiscoveryService;
  crm: CrmService;
  operations: OperationService;
  mcpOAuth: McpOAuthService;
}

export function createContainer(config: AppConfig, logger: AppLogger, pool = createPool(config)): AppContainer {
  const connections = new ConnectionRepository(pool);
  const states = new OAuthStateRepository(pool);
  const schemaCache = new SchemaCacheRepository(pool);
  const pendingOperations = new PendingOperationRepository(pool);
  const audit = new AuditRepository(pool);
  const mcpOAuthStore = new SqliteMcpOAuthStore(pool);
  const cipher = new TokenCipher(config.tokenEncryptionKey);
  const oauth = new BitrixOAuthService(config, states, connections, cipher);
  const tokens = new TokenManager(connections, oauth, cipher, logger);
  const bitrix = new BitrixRestClient(config, tokens, logger);
  const guard = new PolicyGuard();
  return {
    config,
    pool,
    connections,
    oauth,
    tokens,
    bitrix,
    schema: new SchemaDiscoveryService(config, schemaCache, bitrix),
    crm: new CrmService(config, bitrix),
    operations: new OperationService(config, connections, pendingOperations, audit, guard, bitrix),
    mcpOAuth: new McpOAuthService(config, mcpOAuthStore),
  };
}
