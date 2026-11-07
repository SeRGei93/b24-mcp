#!/usr/bin/env node
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { loadConfig } from "./config/env.js";
import { createContainer } from "./container.js";
import { runMigrations } from "./db/client.js";
import { stderrLogger } from "./logging.js";
import { createMcpServer } from "./mcp/server.js";
import { buildHttpApp } from "./api/app.js";

async function main(): Promise<void> {
  const config = loadConfig();
  const container = createContainer(config, stderrLogger);
  await runMigrations(container.pool, stderrLogger);

  if (config.transport === "stdio") {
    const server = createMcpServer(container);
    const transport = new StdioServerTransport();
    await server.connect(transport);
    const stop = async () => {
      await server.close();
      await container.pool.end();
    };
    process.once("SIGINT", () => void stop().finally(() => process.exit(0)));
    process.once("SIGTERM", () => void stop().finally(() => process.exit(0)));
    return;
  }

  const app = buildHttpApp(container);
  const stop = async (signal: string) => {
    stderrLogger.info({ signal }, "Shutting down");
    await app.close();
    await container.pool.end();
  };
  process.once("SIGINT", () => void stop("SIGINT").finally(() => process.exit(0)));
  process.once("SIGTERM", () => void stop("SIGTERM").finally(() => process.exit(0)));
  await app.listen({ host: config.host, port: config.port });
}

main().catch((error: unknown) => {
  stderrLogger.error({ error: error instanceof Error ? error.message : String(error) }, "Fatal startup error");
  process.exitCode = 1;
});
