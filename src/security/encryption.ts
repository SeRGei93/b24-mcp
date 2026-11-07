import { createCipheriv, createDecipheriv, randomBytes } from "node:crypto";

const ALGORITHM = "aes-256-gcm";
const VERSION = "v1";

export function parseEncryptionKey(value: string): Buffer {
  if (/^[a-f\d]{64}$/i.test(value)) return Buffer.from(value, "hex");

  let decoded: Buffer;
  try {
    decoded = Buffer.from(value, "base64");
  } catch {
    throw new Error("TOKEN_ENCRYPTION_KEY must be a 32-byte base64 or 64-character hexadecimal key");
  }
  if (decoded.length !== 32) {
    throw new Error("TOKEN_ENCRYPTION_KEY must be a 32-byte base64 or 64-character hexadecimal key");
  }
  return decoded;
}

export class TokenCipher {
  readonly #key: Buffer;

  constructor(key: string | Buffer) {
    this.#key = typeof key === "string" ? parseEncryptionKey(key) : key;
    if (this.#key.length !== 32) throw new Error("Token encryption key must contain exactly 32 bytes");
  }

  encrypt(plaintext: string): string {
    const iv = randomBytes(12);
    const cipher = createCipheriv(ALGORITHM, this.#key, iv);
    const ciphertext = Buffer.concat([cipher.update(plaintext, "utf8"), cipher.final()]);
    const tag = cipher.getAuthTag();
    return [VERSION, iv.toString("base64url"), tag.toString("base64url"), ciphertext.toString("base64url")].join(":");
  }

  decrypt(payload: string): string {
    const [version, ivValue, tagValue, ciphertextValue, extra] = payload.split(":");
    if (version !== VERSION || !ivValue || !tagValue || !ciphertextValue || extra !== undefined) {
      throw new Error("Encrypted token has an unsupported or invalid format");
    }
    const decipher = createDecipheriv(ALGORITHM, this.#key, Buffer.from(ivValue, "base64url"));
    decipher.setAuthTag(Buffer.from(tagValue, "base64url"));
    return Buffer.concat([
      decipher.update(Buffer.from(ciphertextValue, "base64url")),
      decipher.final(),
    ]).toString("utf8");
  }
}
