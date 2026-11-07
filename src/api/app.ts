import Fastify, { LogController, type FastifyReply, type FastifyRequest } from "fastify";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { z } from "zod/v4";
import type { AppContainer } from "../container.js";
import { AppError, AuthenticationError, safeErrorMessage } from "../errors.js";
import { toPublicConnection } from "../types.js";
import { createMcpServer } from "../mcp/server.js";
import { registerMcpOAuthRoutes } from "./mcp-oauth-routes.js";
import { BitrixOAuthContinuationError } from "../bitrix/oauth.js";
import type { McpOAuthAccessInfo } from "../mcp/oauth/types.js";

const callbackQuery = z.object({
  code: z.string().optional(),
  state: z.string().optional(),
  domain: z.string().optional(),
  member_id: z.string().optional(),
  error: z.string().optional(),
});

export function buildHttpApp(container: AppContainer) {
  const logger = container.config.logLevel === "silent" ? false : {
    level: container.config.logLevel,
    redact: {
      paths: [
        "req.headers.authorization",
        "headers.authorization",
        "body.auth",
        "body.access_token",
        "body.refresh_token",
        "body.code",
        "body.code_verifier",
        "body.token",
        "client_secret",
      ],
      censor: "[REDACTED]",
    },
  };
  const app = Fastify({
    logger,
    bodyLimit: 1_048_576,
    trustProxy: true,
    logController: new LogController({ disableRequestLogging: true }),
  });
  app.addContentTypeParser(
    "application/x-www-form-urlencoded",
    { parseAs: "string" },
    (_request, body, done) => {
      try {
        const parsed: Record<string, unknown> = {};
        const auth: Record<string, string> = {};
        for (const [key, value] of new URLSearchParams(body.toString())) {
          const match = /^auth\[([^\]]+)\]$/.exec(key);
          if (match?.[1]) auth[match[1]] = value;
          else parsed[key] = value;
        }
        if (Object.keys(auth).length) parsed.auth = auth;
        done(null, parsed);
      } catch (error) {
        done(error as Error);
      }
    },
  );
  const allowedOrigin = new URL(container.config.appUrl).origin;
  const accessByRequest = new WeakMap<FastifyRequest, McpOAuthAccessInfo>();

  const authenticate = async (request: FastifyRequest) => {
    const access = await container.mcpOAuth.verifyAuthorizationHeader(request.headers.authorization);
    if (!access) {
      throw new AuthenticationError();
    }
    accessByRequest.set(request, access);
    if (request.headers.origin && request.headers.origin !== allowedOrigin) {
      throw new AppError("Request origin is not allowed", "ORIGIN_NOT_ALLOWED", 403);
    }
  };

  app.get("/", async () => ({
    name: "bitrix24-mcp-server",
    version: "0.3.1",
    mcp_endpoint: "/mcp",
    oauth_start: "/oauth/bitrix/start?domain=your-portal.bitrix24.com",
    bitrix_oauth_callback: "/oauth/bitrix/callback",
    oauth_metadata: "/.well-known/oauth-authorization-server",
  }));

  registerMcpOAuthRoutes(app, container);

  app.get("/health", async (_request, reply) => {
    try {
      await container.pool.query("SELECT 1");
      return { status: "ok", database: "connected" };
    } catch {
      reply.code(503);
      return { status: "unavailable", database: "disconnected" };
    }
  });

  app.get("/oauth/bitrix/start", { preHandler: authenticate }, async (request, reply) => {
    const query = z.object({
      domain: z.string().min(1),
      mode: z.enum(["read_only", "write_with_confirmation"]).optional(),
    }).parse(request.query);
    const location = await container.oauth.createAuthorizationUrl(query.domain, query.mode);
    return reply.redirect(location);
  });

  app.get("/oauth/bitrix/callback", async (request, reply) => {
    reply.header("cache-control", "no-store");
    try {
      const result = await container.oauth.handleCallbackWithContext(callbackQuery.parse(request.query));
      if (result.mcpRequestHash) {
        const location = await container.mcpOAuth.completeBitrixAuthorization(
          result.mcpRequestHash,
          result.connection.id,
        );
        return reply.redirect(location);
      }
      return {
        connected: true,
        connection: toPublicConnection(result.connection),
        next: "Use this connection.id with the Bitrix24 MCP tools.",
      };
    } catch (error) {
      if (error instanceof BitrixOAuthContinuationError) {
        const location = await container.mcpOAuth.failBitrixAuthorization(
          error.mcpRequestHash,
          error.oauthErrorCode,
          error.message,
        );
        return reply.redirect(location);
      }
      throw error;
    }
  });

  app.post("/oauth/bitrix/install", async (request, reply) => {
    const body = z.object({ auth: z.unknown() }).parse(request.body);
    const connection = await container.oauth.handleInstallation(body.auth);
    reply.header("cache-control", "no-store");
    return { status: "success", connected: true, connection: toPublicConnection(connection) };
  });

  app.post("/oauth/bitrix/refresh", { preHandler: authenticate }, async (request) => {
    const body = z.object({ connection_id: z.uuid() }).parse(request.body);
    const refreshed = await container.tokens.get(body.connection_id, true);
    return { refreshed: true, connection: toPublicConnection(refreshed.connection) };
  });

  app.post("/mcp", { preHandler: authenticate }, async (request, reply) => {
    const server = createMcpServer(container, accessByRequest.get(request)?.connectionId);
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    reply.hijack();
    try {
      await server.connect(transport);
      await transport.handleRequest(request.raw, reply.raw, request.body);
    } finally {
      await transport.close().catch(() => undefined);
      await server.close().catch(() => undefined);
    }
  });

  const methodNotAllowed = async (_request: FastifyRequest, reply: FastifyReply) => {
    return reply.code(405).send({ jsonrpc: "2.0", error: { code: -32_000, message: "Method not allowed" }, id: null });
  };
  app.get("/mcp", { preHandler: authenticate }, methodNotAllowed);
  app.delete("/mcp", { preHandler: authenticate }, methodNotAllowed);

  app.setErrorHandler((error, request, reply) => {
    const statusCode = error instanceof AppError ? error.statusCode : error instanceof z.ZodError ? 400 : 500;
    const code = error instanceof AppError ? error.code : error instanceof z.ZodError ? "INVALID_REQUEST" : "INTERNAL_ERROR";
    request.log.error({ err: error, code }, "Request failed");
    if (statusCode === 401) reply.header("www-authenticate", container.mcpOAuth.challenge("invalid_token"));
    reply.code(statusCode).send({ error: code, message: error instanceof z.ZodError ? "Request validation failed" : safeErrorMessage(error) });
  });

  return app;
}
