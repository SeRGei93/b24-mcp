import { randomBytes } from "node:crypto";
import type { FastifyInstance, FastifyReply, FastifyRequest } from "fastify";
import { z } from "zod/v4";
import type { AppContainer } from "../container.js";
import { McpOAuthProtocolError, type PendingAuthorizationInfo } from "../mcp/oauth/service.js";
import { isValidBearerToken, isValidSecret } from "../security/auth.js";

const registrationSchema = z.object({
  redirect_uris: z.array(z.url()).min(1).max(10),
  token_endpoint_auth_method: z.string().optional(),
  grant_types: z.array(z.string()).optional(),
  response_types: z.array(z.string()).optional(),
  client_name: z.string().max(200).optional(),
  client_uri: z.url().optional(),
  logo_uri: z.union([z.url(), z.literal("")]).optional(),
  scope: z.string().optional(),
  software_id: z.string().max(200).optional(),
  software_version: z.string().max(100).optional(),
});

const authorizationSchema = z.object({
  response_type: z.string(),
  client_id: z.string().min(1),
  redirect_uri: z.string().optional(),
  code_challenge: z.string().min(1),
  code_challenge_method: z.string().min(1),
  state: z.string().optional(),
  scope: z.string().optional(),
  resource: z.string().optional(),
});

const authorizationDecisionSchema = z.object({
  request_token: z.string().min(1),
  csrf_token: z.string().min(1),
  decision: z.enum(["approve", "deny"]),
});

const authorizationCodeTokenSchema = z.object({
  grant_type: z.literal("authorization_code"),
  client_id: z.string().min(1),
  code: z.string().min(1),
  code_verifier: z.string().min(1),
  redirect_uri: z.string().optional(),
  resource: z.string().optional(),
});

const refreshTokenSchema = z.object({
  grant_type: z.literal("refresh_token"),
  client_id: z.string().min(1),
  refresh_token: z.string().min(1),
  scope: z.string().optional(),
  resource: z.string().optional(),
});

const revokeSchema = z.object({ token: z.string().min(1) });

export function registerMcpOAuthRoutes(app: FastifyInstance, container: AppContainer): void {
  const limiter = new WindowRateLimiter();
  const metadata = async (_request: FastifyRequest, reply: FastifyReply) => {
    setMetadataHeaders(reply);
    return container.mcpOAuth.protectedResourceMetadata();
  };

  app.get("/.well-known/oauth-protected-resource", metadata);
  app.get("/.well-known/oauth-protected-resource/mcp", metadata);
  app.get("/.well-known/oauth-authorization-server", async (_request, reply) => {
    setMetadataHeaders(reply);
    return container.mcpOAuth.authorizationServerMetadata();
  });

  for (const path of ["/register", "/token", "/revoke"] as const) {
    app.options(path, async (_request, reply) => {
      setCorsHeaders(reply);
      return reply.code(204).send();
    });
  }

  app.post("/register", async (request, reply) => {
    setOAuthHeaders(reply);
    try {
      enforceRateLimit(limiter, request, "register", 20, 60 * 60_000);
      const input = registrationSchema.parse(request.body);
      const registered = await container.mcpOAuth.registerClient({
        redirectUris: input.redirect_uris,
        tokenEndpointAuthMethod: input.token_endpoint_auth_method,
        grantTypes: input.grant_types,
        responseTypes: input.response_types,
        clientName: input.client_name,
        clientUri: input.client_uri,
        logoUri: input.logo_uri || undefined,
        scope: input.scope,
        softwareId: input.software_id,
        softwareVersion: input.software_version,
      });
      return reply.code(201).send(registered);
    } catch (error) {
      return sendOAuthError(reply, normalizeOAuthError(error, "invalid_client_metadata"));
    }
  });

  app.get("/authorize", async (request, reply) => {
    setOAuthHeaders(reply);
    try {
      enforceRateLimit(limiter, request, "authorize", 100, 15 * 60_000);
      const input = authorizationSchema.parse(request.query);
      const pending = await container.mcpOAuth.beginAuthorization({
        responseType: input.response_type,
        clientId: input.client_id,
        redirectUri: input.redirect_uri,
        codeChallenge: input.code_challenge,
        codeChallengeMethod: input.code_challenge_method,
        state: input.state,
        scope: input.scope,
        resource: input.resource,
      });
      if (isValidBearerToken(request.headers.authorization, container.config.mcpAuthToken)) {
        return await redirectToBitrixAuthorization(container, request, reply, pending.requestHash);
      }
      return sendAuthorizationConsentPage(container, reply, pending);
    } catch (error) {
      const normalized = normalizeOAuthError(error, "invalid_request");
      if (normalized.safeRedirectUri) return reply.redirect(oauthErrorRedirect(normalized));
      return sendOAuthError(reply, normalized);
    }
  });

  app.post("/authorize", async (request, reply) => {
    setOAuthHeaders(reply);
    try {
      enforceRateLimit(limiter, request, "authorize", 100, 15 * 60_000);
      const input = authorizationDecisionSchema.parse(request.body);
      const csrfCookie = readCookie(request.headers.cookie, CONSENT_CSRF_COOKIE);
      clearConsentCookie(container, reply);
      if (!csrfCookie || !isValidSecret(input.csrf_token, csrfCookie)) {
        throw new McpOAuthProtocolError("invalid_request", "Invalid or missing OAuth consent CSRF token", 403);
      }
      const pending = await container.mcpOAuth.getPendingAuthorization(input.request_token);
      if (input.decision !== "approve") {
        const location = await container.mcpOAuth.failBitrixAuthorization(
          pending.requestHash,
          "access_denied",
          "The user denied the authorization request",
        );
        return reply.redirect(location);
      }
      return await redirectToBitrixAuthorization(container, request, reply, pending.requestHash);
    } catch (error) {
      const normalized = normalizeOAuthError(error, "invalid_request");
      if (normalized.safeRedirectUri) return reply.redirect(oauthErrorRedirect(normalized));
      return sendOAuthError(reply, normalized);
    }
  });

  app.post("/token", async (request, reply) => {
    setOAuthHeaders(reply);
    try {
      enforceRateLimit(limiter, request, "token", 50, 15 * 60_000);
      const grantType = z.object({ grant_type: z.string() }).parse(request.body).grant_type;
      const tokens = grantType === "authorization_code"
        ? await exchangeAuthorizationCode(container, authorizationCodeTokenSchema.parse(request.body))
        : grantType === "refresh_token"
          ? await exchangeRefreshToken(container, refreshTokenSchema.parse(request.body))
          : (() => { throw new McpOAuthProtocolError("unsupported_grant_type", "Unsupported OAuth grant type"); })();
      return reply.code(200).send(tokens);
    } catch (error) {
      return sendOAuthError(reply, normalizeOAuthError(error, "invalid_request"));
    }
  });

  app.post("/revoke", async (request, reply) => {
    setOAuthHeaders(reply);
    try {
      enforceRateLimit(limiter, request, "revoke", 50, 15 * 60_000);
      const { token } = revokeSchema.parse(request.body);
      await container.mcpOAuth.revokeToken(token);
      return reply.code(200).send();
    } catch (error) {
      return sendOAuthError(reply, normalizeOAuthError(error, "invalid_request"));
    }
  });
}

async function exchangeAuthorizationCode(
  container: AppContainer,
  input: z.infer<typeof authorizationCodeTokenSchema>,
) {
  return container.mcpOAuth.exchangeAuthorizationCode({
    clientId: input.client_id,
    code: input.code,
    codeVerifier: input.code_verifier,
    redirectUri: input.redirect_uri,
    resource: input.resource,
  });
}

async function exchangeRefreshToken(
  container: AppContainer,
  input: z.infer<typeof refreshTokenSchema>,
) {
  return container.mcpOAuth.exchangeRefreshToken({
    clientId: input.client_id,
    refreshToken: input.refresh_token,
    scope: input.scope,
    resource: input.resource,
  });
}

function normalizeOAuthError(error: unknown, fallbackCode: string): McpOAuthProtocolError {
  if (error instanceof McpOAuthProtocolError) return error;
  if (error instanceof z.ZodError) {
    return new McpOAuthProtocolError(fallbackCode, "OAuth request validation failed");
  }
  return new McpOAuthProtocolError("server_error", "The authorization server could not process the request", 500);
}

function sendOAuthError(reply: FastifyReply, error: McpOAuthProtocolError) {
  setOAuthHeaders(reply);
  return reply.code(error.statusCode).send({ error: error.errorCode, error_description: error.message });
}

function oauthErrorRedirect(error: McpOAuthProtocolError): string {
  const target = new URL(error.safeRedirectUri!);
  target.searchParams.set("error", error.errorCode);
  target.searchParams.set("error_description", error.message);
  if (error.state) target.searchParams.set("state", error.state);
  return target.toString();
}

function setMetadataHeaders(reply: FastifyReply): void {
  reply.header("cache-control", "public, max-age=300");
  reply.header("content-type", "application/json; charset=utf-8");
}

function setOAuthHeaders(reply: FastifyReply): void {
  setCorsHeaders(reply);
  reply.header("cache-control", "no-store");
  reply.header("pragma", "no-cache");
}

function setCorsHeaders(reply: FastifyReply): void {
  reply.header("access-control-allow-origin", "*");
  reply.header("access-control-allow-methods", "POST, OPTIONS");
  reply.header("access-control-allow-headers", "Authorization, Content-Type, MCP-Protocol-Version");
}

class WindowRateLimiter {
  private readonly entries = new Map<string, { count: number; resetAt: number }>();

  constructor(private readonly maxEntries = 8_192) {}

  consume(key: string, limit: number, windowMs: number): boolean {
    const now = Date.now();
    const current = this.entries.get(key);
    if (!current || current.resetAt <= now) {
      if (!current && this.entries.size >= this.maxEntries) {
        this.deleteExpired(now);
        if (this.entries.size >= this.maxEntries) return false;
      }
      this.entries.set(key, { count: 1, resetAt: now + windowMs });
      return true;
    }
    current.count += 1;
    return current.count <= limit;
  }

  private deleteExpired(now: number): void {
    for (const [key, entry] of this.entries) {
      if (entry.resetAt <= now) this.entries.delete(key);
    }
  }
}

const CONSENT_CSRF_COOKIE = "mcp_oauth_csrf";

async function redirectToBitrixAuthorization(
  container: AppContainer,
  request: FastifyRequest,
  reply: FastifyReply,
  requestHash: string,
) {
  try {
    const location = await container.oauth.createMcpAuthorizationUrl(requestHash);
    return reply.redirect(location);
  } catch (error) {
    request.log.error({ err: error }, "Could not start Bitrix24 authorization");
    const location = await container.mcpOAuth.failBitrixAuthorization(
      requestHash,
      "temporarily_unavailable",
      "Bitrix24 authorization is not available for this MCP server",
    );
    return reply.redirect(location);
  }
}

function sendAuthorizationConsentPage(
  container: AppContainer,
  reply: FastifyReply,
  pending: PendingAuthorizationInfo,
) {
  const csrfToken = randomBytes(32).toString("base64url");
  const secureAttr = container.config.appUrl.startsWith("https://") ? "; Secure" : "";
  reply.header(
    "set-cookie",
    `${CONSENT_CSRF_COOKIE}=${csrfToken}; HttpOnly; SameSite=Lax; Path=/authorize; Max-Age=${container.config.mcpOAuthRequestTtlSeconds}${secureAttr}`,
  );
  reply.header("x-frame-options", "DENY");
  reply.header(
    "content-security-policy",
    "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'",
  );
  reply.type("text/html; charset=utf-8");

  const clientLabel = escapeHtml(pending.clientName ?? pending.clientId);
  const clientId = escapeHtml(pending.clientId);
  const redirectOrigin = escapeHtml(new URL(pending.redirectUri).origin);
  const redirectUri = escapeHtml(pending.redirectUri);
  const scope = escapeHtml(pending.scope);
  const requestToken = escapeHtml(pending.requestToken);
  const escapedCsrf = escapeHtml(csrfToken);

  return reply.code(200).send(
    `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Authorize MCP Client</title>`
    + `<style>body{font-family:system-ui,-apple-system,sans-serif;max-width:480px;margin:48px auto;padding:0 16px;color:#111}card{display:block;border:1px solid #ddd;border-radius:8px;padding:24px}code{background:#f4f4f5;padding:2px 6px;border-radius:4px;word-break:break-all}.actions{display:flex;gap:12px;margin-top:20px}button{padding:10px 18px;border-radius:6px;border:1px solid #ccc;cursor:pointer;font-weight:600}button[value="approve"]{background:#111;color:#fff;border-color:#111}</style></head>`
    + `<body><card><h1>Authorize Bitrix24 MCP Access</h1>`
    + `<p>Client <strong>${clientLabel}</strong> (<code>${clientId}</code>) is requesting access to your Bitrix24 MCP server.</p>`
    + `<p>Redirect origin: <code>${redirectOrigin}</code><br>Callback URI: <code>${redirectUri}</code><br>Scope: <code>${scope}</code></p>`
    + `<form method="post" action="/authorize">`
    + `<input type="hidden" name="request_token" value="${requestToken}">`
    + `<input type="hidden" name="csrf_token" value="${escapedCsrf}">`
    + `<div class="actions"><button type="submit" name="decision" value="approve">Approve</button>`
    + `<button type="submit" name="decision" value="deny">Deny</button></div>`
    + `</form></card></body></html>`,
  );
}

function clearConsentCookie(container: AppContainer, reply: FastifyReply): void {
  const secureAttr = container.config.appUrl.startsWith("https://") ? "; Secure" : "";
  reply.header("set-cookie", `${CONSENT_CSRF_COOKIE}=; HttpOnly; SameSite=Lax; Path=/authorize; Max-Age=0${secureAttr}`);
}

function readCookie(cookieHeader: string | undefined, name: string): string | undefined {
  if (!cookieHeader) return undefined;
  for (const part of cookieHeader.split(";")) {
    const [rawKey, ...rest] = part.trim().split("=");
    if (rawKey === name) return rest.join("=");
  }
  return undefined;
}

function escapeHtml(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll("\"", "&quot;")
    .replaceAll("'", "&#39;");
}

function enforceRateLimit(
  limiter: WindowRateLimiter,
  request: FastifyRequest,
  bucket: string,
  limit: number,
  windowMs: number,
): void {
  if (!limiter.consume(`${bucket}:source:${request.ip}`, limit, windowMs)) {
    throw new McpOAuthProtocolError("temporarily_unavailable", "Too many OAuth requests", 429);
  }
  const globalLimit = Math.max(limit * 25, 100);
  if (!limiter.consume(`${bucket}:global`, globalLimit, windowMs)) {
    throw new McpOAuthProtocolError("temporarily_unavailable", "Too many OAuth requests", 429);
  }
}
