import "dotenv/config";
import { z } from "zod/v4";
import { CONNECTION_MODES } from "../types.js";

const booleanFromEnv = z
  .enum(["true", "false", "1", "0", "yes", "no"])
  .default("false")
  .transform((value) => value === "true" || value === "1" || value === "yes");

const positiveInteger = (fallback: number) =>
  z.coerce.number().int().positive().default(fallback);

const publicUrl = z.url().refine((value) => {
  const url = new URL(value);
  return url.protocol === "https:" || (
    url.protocol === "http:" && ["localhost", "127.0.0.1", "::1"].includes(url.hostname)
  );
}, { message: "URL must use HTTPS except on localhost or a loopback address" });

const appBaseUrl = publicUrl.refine((value) => {
  const url = new URL(value);
  return (url.pathname === "/" || url.pathname === "") && !url.search && !url.hash;
}, { message: "APP_URL must be an origin without a path, query, or fragment" });

const envSchema = z.object({
  NODE_ENV: z.enum(["development", "test", "production"]).default("development"),
  HOST: z.string().default("0.0.0.0"),
  PORT: z.coerce.number().int().min(1).max(65_535).default(3000),
  APP_URL: appBaseUrl,
  DATABASE_URL: z.string().min(1),
  DATABASE_SSL: booleanFromEnv,
  BITRIX_CLIENT_ID: z.string().min(1),
  BITRIX_CLIENT_SECRET: z.string().min(1),
  BITRIX_PORTAL_DOMAIN: z.preprocess(
    (value) => value === "" ? undefined : value,
    z.string().min(1).optional(),
  ),
  BITRIX_MCP_DEFAULT_MODE: z.enum(CONNECTION_MODES).default("read_only"),
  BITRIX_MCP_ADMIN_TOOLS: booleanFromEnv,
  MCP_AUTH_TOKEN: z.string().min(32, "MCP_AUTH_TOKEN must contain at least 32 characters"),
  TOKEN_ENCRYPTION_KEY: z.string().min(1),
  MCP_TRANSPORT: z.enum(["http", "stdio"]).default("http"),
  LOG_LEVEL: z.enum(["fatal", "error", "warn", "info", "debug", "trace", "silent"]).default("info"),
  OAUTH_STATE_TTL_SECONDS: positiveInteger(600),
  MCP_OAUTH_REQUEST_TTL_SECONDS: positiveInteger(600),
  MCP_OAUTH_CODE_TTL_SECONDS: positiveInteger(300),
  MCP_OAUTH_ACCESS_TOKEN_TTL_SECONDS: positiveInteger(3_600),
  MCP_OAUTH_REFRESH_TOKEN_TTL_SECONDS: positiveInteger(2_592_000),
  SCHEMA_CACHE_TTL_SECONDS: positiveInteger(900),
  PENDING_OPERATION_TTL_SECONDS: positiveInteger(600),
  BITRIX_REQUEST_TIMEOUT_MS: positiveInteger(15_000),
  BITRIX_MAX_RETRIES: z.coerce.number().int().min(0).max(8).default(3),
  BITRIX_MAX_PAGES: z.coerce.number().int().min(1).max(100).default(20),
});

export interface AppConfig {
  nodeEnv: "development" | "test" | "production";
  host: string;
  port: number;
  appUrl: string;
  databaseUrl: string;
  databaseSsl: boolean;
  bitrixClientId: string;
  bitrixClientSecret: string;
  bitrixPortalDomain?: string;
  defaultMode: "read_only" | "write_with_confirmation";
  adminToolsEnabled: boolean;
  mcpAuthToken: string;
  tokenEncryptionKey: string;
  transport: "http" | "stdio";
  logLevel: string;
  oauthStateTtlSeconds: number;
  mcpOAuthRequestTtlSeconds: number;
  mcpOAuthCodeTtlSeconds: number;
  mcpOAuthAccessTokenTtlSeconds: number;
  mcpOAuthRefreshTokenTtlSeconds: number;
  schemaCacheTtlSeconds: number;
  pendingOperationTtlSeconds: number;
  bitrixRequestTimeoutMs: number;
  bitrixMaxRetries: number;
  bitrixMaxPages: number;
}

export function loadConfig(environment: NodeJS.ProcessEnv = process.env): AppConfig {
  const parsed = envSchema.safeParse(environment);
  if (!parsed.success) {
    const details = parsed.error.issues.map((issue) => `${issue.path.join(".")}: ${issue.message}`).join("; ");
    throw new Error(`Invalid environment configuration: ${details}`);
  }

  const env = parsed.data;
  return {
    nodeEnv: env.NODE_ENV,
    host: env.HOST,
    port: env.PORT,
    appUrl: env.APP_URL.replace(/\/$/, ""),
    databaseUrl: env.DATABASE_URL,
    databaseSsl: env.DATABASE_SSL,
    bitrixClientId: env.BITRIX_CLIENT_ID,
    bitrixClientSecret: env.BITRIX_CLIENT_SECRET,
    bitrixPortalDomain: env.BITRIX_PORTAL_DOMAIN,
    defaultMode: env.BITRIX_MCP_DEFAULT_MODE,
    adminToolsEnabled: env.BITRIX_MCP_ADMIN_TOOLS,
    mcpAuthToken: env.MCP_AUTH_TOKEN,
    tokenEncryptionKey: env.TOKEN_ENCRYPTION_KEY,
    transport: env.MCP_TRANSPORT,
    logLevel: env.LOG_LEVEL,
    oauthStateTtlSeconds: env.OAUTH_STATE_TTL_SECONDS,
    mcpOAuthRequestTtlSeconds: env.MCP_OAUTH_REQUEST_TTL_SECONDS,
    mcpOAuthCodeTtlSeconds: env.MCP_OAUTH_CODE_TTL_SECONDS,
    mcpOAuthAccessTokenTtlSeconds: env.MCP_OAUTH_ACCESS_TOKEN_TTL_SECONDS,
    mcpOAuthRefreshTokenTtlSeconds: env.MCP_OAUTH_REFRESH_TOKEN_TTL_SECONDS,
    schemaCacheTtlSeconds: env.SCHEMA_CACHE_TTL_SECONDS,
    pendingOperationTtlSeconds: env.PENDING_OPERATION_TTL_SECONDS,
    bitrixRequestTimeoutMs: env.BITRIX_REQUEST_TIMEOUT_MS,
    bitrixMaxRetries: env.BITRIX_MAX_RETRIES,
    bitrixMaxPages: env.BITRIX_MAX_PAGES,
  };
}
