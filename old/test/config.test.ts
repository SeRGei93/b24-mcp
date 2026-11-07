import { randomBytes } from "node:crypto";
import { describe, expect, it } from "vitest";
import { loadConfig } from "../src/config/env.js";

function environment(appUrl: string): NodeJS.ProcessEnv {
  return {
    APP_URL: appUrl,
    DATABASE_PATH: "./data/test.sqlite",
    BITRIX_CLIENT_ID: "client",
    BITRIX_CLIENT_SECRET: "secret",
    MCP_AUTH_TOKEN: "a".repeat(32),
    TOKEN_ENCRYPTION_KEY: randomBytes(32).toString("base64"),
  };
}

describe("loadConfig", () => {
  it("derives the callback URL and safe defaults", () => {
    const config = loadConfig(environment("http://localhost:3000"));
    expect(config.defaultMode).toBe("read_only");
    expect(config.adminToolsEnabled).toBe(false);
    expect(config.bitrixPortalDomain).toBeUndefined();
    expect(config.mcpOAuthAccessTokenTtlSeconds).toBe(3_600);
    expect(config.databasePath).toBe("./data/test.sqlite");
  });

  it("defaults DATABASE_PATH when omitted and normalizes sqlite/file URLs", () => {
    const base = environment("http://localhost:3000");
    delete base.DATABASE_PATH;
    expect(loadConfig(base).databasePath).toBe("./data/bitrix24-mcp.sqlite");

    const fromUrl = environment("http://localhost:3000");
    delete fromUrl.DATABASE_PATH;
    fromUrl.DATABASE_URL = "sqlite://./data/custom.sqlite";
    expect(loadConfig(fromUrl).databasePath).toBe("./data/custom.sqlite");

    const inMemory = environment("http://localhost:3000");
    inMemory.DATABASE_PATH = ":memory:";
    expect(loadConfig(inMemory).databasePath).toBe(":memory:");
  });

  it("accepts a fixed Bitrix24 portal and treats an empty optional value as absent", () => {
    const configured = environment("http://localhost:3000");
    configured.BITRIX_PORTAL_DOMAIN = "example.bitrix24.com";
    expect(loadConfig(configured).bitrixPortalDomain).toBe("example.bitrix24.com");

    const empty = environment("http://localhost:3000");
    empty.BITRIX_PORTAL_DOMAIN = "";
    expect(loadConfig(empty).bitrixPortalDomain).toBeUndefined();
  });

  it.each([
    "http://example.com",
    "http://localhost.evil.example",
    "https://mcp.example.com/a/path",
    "https://mcp.example.com/?query=yes",
  ])("rejects an unsafe or non-origin APP_URL: %s", (url) => {
    expect(() => loadConfig(environment(url))).toThrow(/APP_URL/);
  });
});
