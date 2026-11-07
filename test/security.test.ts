import { randomBytes } from "node:crypto";
import { describe, expect, it } from "vitest";
import { isValidBearerToken } from "../src/security/auth.js";
import { parseEncryptionKey, TokenCipher } from "../src/security/encryption.js";

describe("TokenCipher", () => {
  it("round-trips tokens without storing plaintext", () => {
    const cipher = new TokenCipher(randomBytes(32));
    const encrypted = cipher.encrypt("sensitive-token");
    expect(encrypted).not.toContain("sensitive-token");
    expect(cipher.decrypt(encrypted)).toBe("sensitive-token");
  });

  it("rejects tampered ciphertext", () => {
    const cipher = new TokenCipher(randomBytes(32));
    const encrypted = cipher.encrypt("token");
    const parts = encrypted.split(":");
    const ciphertext = parts[3]!;
    parts[3] = `${ciphertext.startsWith("A") ? "B" : "A"}${ciphertext.slice(1)}`;
    expect(() => cipher.decrypt(parts.join(":"))).toThrow();
  });

  it("accepts only 32-byte base64 or 64-character hex keys", () => {
    expect(parseEncryptionKey(randomBytes(32).toString("base64"))).toHaveLength(32);
    expect(parseEncryptionKey(randomBytes(32).toString("hex"))).toHaveLength(32);
    expect(() => parseEncryptionKey("too-short")).toThrow();
  });
});

describe("bearer authentication", () => {
  const token = "a".repeat(32);
  it("accepts an exact bearer token", () => expect(isValidBearerToken(`Bearer ${token}`, token)).toBe(true));
  it("rejects malformed and unequal tokens", () => {
    expect(isValidBearerToken(token, token)).toBe(false);
    expect(isValidBearerToken(`Bearer ${"b".repeat(32)}`, token)).toBe(false);
    expect(isValidBearerToken(`Bearer ${token}x`, token)).toBe(false);
  });
});
