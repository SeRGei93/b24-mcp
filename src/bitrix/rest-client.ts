import { randomUUID } from "node:crypto";
import type { AppConfig } from "../config/env.js";
import type { AppLogger, BitrixResponse } from "../types.js";
import { BitrixApiError } from "../errors.js";
import { TokenManager } from "./token-manager.js";

const RETRYABLE_CODES = new Set(["QUERY_LIMIT_EXCEEDED", "OPERATION_TIME_LIMIT", "TOO_MANY_REQUESTS"]);
const TOKEN_CODES = new Set(["expired_token", "INVALID_TOKEN", "invalid_token"]);

export class BitrixRestClient {
  constructor(
    private readonly config: AppConfig,
    private readonly tokens: TokenManager,
    private readonly logger: AppLogger,
  ) {}

  async call<T = unknown>(connectionId: string, method: string, params: Record<string, unknown> = {}): Promise<BitrixResponse<T>> {
    if (!/^[a-z][a-z0-9_.]+$/i.test(method)) throw new BitrixApiError("Invalid Bitrix REST method name", "INVALID_METHOD", 400);
    const requestId = randomUUID();
    let refreshedForRequest = false;
    let forceRefreshNext = false;
    let staleAccessToken: string | undefined;

    for (let attempt = 0; attempt <= this.config.bitrixMaxRetries; attempt += 1) {
      const credentials = await this.tokens.get(connectionId, forceRefreshNext, staleAccessToken);
      forceRefreshNext = false;
      staleAccessToken = undefined;
      const endpoint = new URL(`${method}.json`, credentials.connection.clientEndpoint).toString();
      this.logger.debug({ requestId, connectionId, method, attempt }, "Calling Bitrix24 REST API");

      let response: Response;
      try {
        response = await fetch(endpoint, {
          method: "POST",
          headers: { "content-type": "application/json", accept: "application/json" },
          body: JSON.stringify({ ...params, auth: credentials.accessToken }),
          signal: AbortSignal.timeout(this.config.bitrixRequestTimeoutMs),
        });
      } catch (error) {
        if (attempt < this.config.bitrixMaxRetries) {
          await backoff(attempt);
          continue;
        }
        throw new BitrixApiError("Bitrix24 REST request failed at the network layer", "NETWORK_ERROR");
      }

      const payload = await response.json().catch(() => null) as (BitrixResponse<T> & { error?: string; error_description?: string }) | null;
      const code = payload?.error ?? (response.ok ? undefined : `HTTP_${response.status}`);
      if (!code && payload && Object.prototype.hasOwnProperty.call(payload, "result")) return payload;

      if (code && TOKEN_CODES.has(code) && !refreshedForRequest) {
        refreshedForRequest = true;
        forceRefreshNext = true;
        staleAccessToken = credentials.accessToken;
        continue;
      }
      if ((response.status === 429 || response.status >= 500 || (code && RETRYABLE_CODES.has(code))) && attempt < this.config.bitrixMaxRetries) {
        this.logger.warn({ requestId, connectionId, method, attempt, bitrixCode: code }, "Retrying Bitrix24 request");
        await backoff(attempt, response.headers.get("retry-after"));
        continue;
      }

      this.logger.warn({ requestId, connectionId, method, bitrixCode: code }, "Bitrix24 REST request rejected");
      throw new BitrixApiError(`Bitrix24 rejected ${method} (${code ?? "invalid_response"})`, code ?? "INVALID_RESPONSE", response.status >= 400 ? response.status : 502);
    }
    throw new BitrixApiError("Bitrix24 request exhausted its retry budget", "RETRY_EXHAUSTED");
  }
}

async function backoff(attempt: number, retryAfter: string | null = null): Promise<void> {
  const parsed = retryAfter ? Number(retryAfter) * 1_000 : Number.NaN;
  const delay = Number.isFinite(parsed) ? Math.min(parsed, 10_000) : Math.min(250 * 2 ** attempt + Math.random() * 200, 5_000);
  await new Promise((resolve) => setTimeout(resolve, delay));
}
