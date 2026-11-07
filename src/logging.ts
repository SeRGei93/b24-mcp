import type { AppLogger } from "./types.js";

const REDACTED_KEYS = new Set([
  "access_token",
  "refresh_token",
  "accessToken",
  "refreshToken",
  "authorization",
  "client_secret",
  "bitrixClientSecret",
  "mcpAuthToken",
  "tokenEncryptionKey",
]);

function redact(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(redact);
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value).map(([key, nested]) => [key, REDACTED_KEYS.has(key) ? "[REDACTED]" : redact(nested)]),
    );
  }
  return value;
}

export const stderrLogger: AppLogger = {
  debug: (bindings, message) => write("debug", bindings, message),
  info: (bindings, message) => write("info", bindings, message),
  warn: (bindings, message) => write("warn", bindings, message),
  error: (bindings, message) => write("error", bindings, message),
};

function write(level: string, bindings: Record<string, unknown>, message?: string): void {
  process.stderr.write(`${JSON.stringify({ level, time: new Date().toISOString(), message, ...redact(bindings) as object })}\n`);
}
