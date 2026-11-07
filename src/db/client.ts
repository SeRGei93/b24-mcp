import pg from "pg";
import type { AppConfig } from "../config/env.js";
import type { AppLogger } from "../types.js";
import { migrations } from "./migrations.js";

const { Pool } = pg;
export type DbPool = pg.Pool;
export type DbClient = pg.PoolClient;

export function createPool(config: AppConfig): DbPool {
  return new Pool({
    connectionString: config.databaseUrl,
    max: 10,
    idleTimeoutMillis: 30_000,
    connectionTimeoutMillis: 10_000,
    ssl: config.databaseSsl ? { rejectUnauthorized: true } : undefined,
  });
}

export async function runMigrations(pool: DbPool, logger: AppLogger): Promise<void> {
  const client = await pool.connect();
  try {
    await client.query("SELECT pg_advisory_lock($1)", [1_927_424_024]);
    await client.query(`CREATE TABLE IF NOT EXISTS schema_migrations (
      id text PRIMARY KEY,
      applied_at timestamptz NOT NULL DEFAULT now()
    )`);
    for (const migration of migrations) {
      const exists = await client.query<{ id: string }>("SELECT id FROM schema_migrations WHERE id = $1", [migration.id]);
      if (exists.rowCount) continue;
      await client.query("BEGIN");
      try {
        await client.query(migration.sql);
        await client.query("INSERT INTO schema_migrations (id) VALUES ($1)", [migration.id]);
        await client.query("COMMIT");
        logger.info({ migrationId: migration.id }, "Database migration applied");
      } catch (error) {
        await client.query("ROLLBACK");
        throw error;
      }
    }
  } finally {
    await client.query("SELECT pg_advisory_unlock($1)", [1_927_424_024]).catch(() => undefined);
    client.release();
  }
}
