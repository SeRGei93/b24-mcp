import { describe, expect, it } from "vitest";
import { normalizePortalDomain, validateClientEndpoint } from "../src/bitrix/oauth.js";

describe("normalizePortalDomain", () => {
  it("normalizes hosted Bitrix24 domains", () => {
    expect(normalizePortalDomain("Example.BITRIX24.com")).toBe("example.bitrix24.com");
    expect(normalizePortalDomain("https://example.bitrix24.com/")).toBe("example.bitrix24.com");
    expect(normalizePortalDomain("example.bitrix24.com.br")).toBe("example.bitrix24.com.br");
  });

  it("accepts only an exact operator-allowlisted custom domain", () => {
    expect(normalizePortalDomain("https://crm.hub-bnk.com/", ["crm.hub-bnk.com"])).toBe("crm.hub-bnk.com");
    expect(() => normalizePortalDomain("other.hub-bnk.com", ["crm.hub-bnk.com"])).toThrow();
    expect(() => normalizePortalDomain("crm.hub-bnk.com")).toThrow();
  });

  it.each([
    "localhost",
    "127.0.0.1",
    "https://example.com",
    "http://example.bitrix24.com",
    "https://example.bitrix24.com:8443",
    "https://example.bitrix24.com/rest/",
    "https://example.bitrix24.com@evil.example",
  ])("rejects unsafe portal value %s", (domain) => {
    expect(() => normalizePortalDomain(domain)).toThrow();
  });
});

describe("validateClientEndpoint", () => {
  it("uses the REST endpoint, not the OAuth response domain, as the portal identity", () => {
    expect(validateClientEndpoint("https://example.bitrix24.com/rest/", "example.bitrix24.com")).toEqual({
      domain: "example.bitrix24.com",
      endpoint: "https://example.bitrix24.com/rest/",
    });
  });

  it("rejects mismatched, non-Bitrix24, and non-REST endpoints", () => {
    expect(() => validateClientEndpoint("https://other.bitrix24.com/rest/", "example.bitrix24.com")).toThrow();
    expect(() => validateClientEndpoint("https://evil.example/rest/")).toThrow();
    expect(() => validateClientEndpoint("https://example.bitrix24.com/oauth/token/")).toThrow();
  });

  it("accepts a custom REST endpoint only when it is exactly allowlisted", () => {
    expect(validateClientEndpoint(
      "https://crm.hub-bnk.com/rest/",
      "crm.hub-bnk.com",
      ["https://crm.hub-bnk.com/"],
    )).toEqual({ domain: "crm.hub-bnk.com", endpoint: "https://crm.hub-bnk.com/rest/" });
    expect(() => validateClientEndpoint(
      "https://other.hub-bnk.com/rest/",
      undefined,
      ["crm.hub-bnk.com"],
    )).toThrow();
  });
});
