import { mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { DatabaseSync, type SQLInputValue } from "node:sqlite";
import type { AppConfig } from "../config/env.js";
import type { AppLogger } from "../types.js";
import { migrations } from "./migrations.js";

export interface QueryResult<T = Record<string, unknown>> {
  rows: T[];
  rowCount: number;
}

export class SqlitePool {
  readonly #db: DatabaseSync;
  readonly #locks = new Map<string, Promise<void>>();
  #closed = false;

  constructor(databasePath: string) {
    if (databasePath !== ":memory:") {
      const resolvedPath = resolve(databasePath);
      mkdirSync(dirname(resolvedPath), { recursive: true });
      this.#db = new DatabaseSync(resolvedPath);
      this.#db.exec("PRAGMA journal_mode = WAL;");
    } else {
      this.#db = new DatabaseSync(":memory:");
    }
    this.#db.exec("PRAGMA foreign_keys = ON;");
    this.#db.exec("PRAGMA busy_timeout = 5000;");
  }

  async query<T = Record<string, unknown>>(sql: string, params: readonly unknown[] = []): Promise<QueryResult<T>> {
    const normalizedParams = params.map(toSqlInputValue);
    const statement = this.#db.prepare(sql);
    if (returnsRows(sql)) {
      const rawRows = statement.all(...normalizedParams) as Record<string, unknown>[];
      const rows = rawRows.map((row) => ({ ...row }) as T);
      return { rows, rowCount: rows.length };
    }
    const result = statement.run(...normalizedParams);
    return { rows: [], rowCount: Number(result.changes) };
  }

  exec(sql: string): void {
    this.#db.exec(sql);
  }

  transaction<T>(work: (client: SqlitePool) => T): T {
    this.#db.exec("BEGIN IMMEDIATE");
    try {
      const result = work(this);
      this.#db.exec("COMMIT");
      return result;
    } catch (error) {
      try {
        this.#db.exec("ROLLBACK");
      } catch {
        // Ignore rollback failure when transaction is already rolled back
      }
      throw error;
    }
  }

  async withLock<T>(key: string, work: (client: SqlitePool) => Promise<T>): Promise<T> {
    const previous = this.#locks.get(key) ?? Promise.resolve();
    let release!: () => void;
    const current = new Promise<void>((resolveLock) => {
      release = resolveLock;
    });
    const chain = previous.then(() => current);
    this.#locks.set(key, chain);

    await previous;
    try {
      return await work(this);
    } finally {
      release();
      if (this.#locks.get(key) === chain) {
        this.#locks.delete(key);
      }
    }
  }

  async end(): Promise<void> {
    if (!this.#closed) {
      this.#closed = true;
      this.#db.close();
    }
  }
}

export type DbPool = SqlitePool;
export type DbClient = SqlitePool;

export function createPool(config: Pick<AppConfig, "databasePath">): DbPool {
  return new SqlitePool(config.databasePath);
}

export async function runMigrations(pool: DbPool, logger: AppLogger): Promise<void> {
  pool.exec(`
    CREATE TABLE IF NOT EXISTS schema_migrations (
      id TEXT PRIMARY KEY,
      applied_at TEXT NOT NULL
    );
  `);

  for (const migration of migrations) {
    const exists = await pool.query<{ id: string }>(
      "SELECT id FROM schema_migrations WHERE id = ?1",
      [migration.id],
    );
    if (exists.rowCount > 0) continue;

    pool.transaction(() => {
      pool.exec(migration.sql);
    });
    await pool.query(
      "INSERT INTO schema_migrations (id, applied_at) VALUES (?1, ?2)",
      [migration.id, new Date().toISOString()],
    );
    logger.info({ migrationId: migration.id }, "Database migration applied");
  }
}

function toSqlInputValue(value: unknown): SQLInputValue {
  if (value === undefined || value === null) return null;
  if (value instanceof Date) return value.toISOString();
  if (typeof value === "boolean") return value ? 1 : 0;
  if (typeof value === "number" || typeof value === "bigint" || typeof value === "string" || value instanceof Uint8Array) {
    return value;
  }
  return JSON.stringify(value);
}

function returnsRows(sql: string): boolean {
  const trimmed = sql.trimStart();
  return /^(SELECT|PRAGMA|WITH)\b/i.test(trimmed) || /\bRETURNING\b/i.test(trimmed);
}
