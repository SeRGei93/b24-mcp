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

  const handleInstallRequest = async (request: FastifyRequest, reply: FastifyReply) => {
    const body = typeof request.body === "object" && request.body !== null
      ? (request.body as Record<string, unknown>)
      : {};
    const query = typeof request.query === "object" && request.query !== null
      ? (request.query as Record<string, unknown>)
      : {};
    const payload = body.auth !== undefined && Object.keys(query).length === 0
      ? body.auth
      : { ...query, ...body };
    const connection = await container.oauth.handleInstallation(payload);
    reply.header("cache-control", "no-store");
    if (request.headers.accept?.includes("text/html")) {
      reply.type("text/html; charset=utf-8");
      return `<!doctype html><html><head><meta charset="utf-8"><script src="https://api.bitrix24.com/api/v1/"></script></head><body><p>Bitrix24 connected (${connection.domain}).</p><script>if(window.BX24){BX24.init(function(){BX24.installFinish();});}</script></body></html>`;
    }
    return { status: "success", connected: true, connection: toPublicConnection(connection) };
  };

  app.post("/oauth/bitrix/install", handleInstallRequest);
  app.post("/oauth/bitrix/callback", handleInstallRequest);
  app.get("/oauth/bitrix/install", async () => ({
    status: "ok",
    message: "Bitrix24 installation endpoint is ready (expects POST from Bitrix24).",
  }));

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
    const details = error instanceof z.ZodError
      ? `Request validation failed: ${error.issues.map((issue) => `${issue.path.join(".") || "input"} (${issue.message})`).join(", ")}`
      : safeErrorMessage(error);
    reply.code(statusCode).send({ error: code, message: details });
  });

  return app;
}
