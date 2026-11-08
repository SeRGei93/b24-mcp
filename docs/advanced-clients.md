# Дополнительные режимы подключения (CLI, статический токен, Stdio, `b24-dev-mcp`)

В этом файле собраны дополнительные сценарии подключения для разработчиков, локальной отладки и CLI-инструментов. Основные способы подключения через OAuth 2.1 (ChatGPT, Codex, Claude, OpenCode) описаны в главном [README.md](../README.md).

---

## 1. Подключение со статическим токеном (`MCP_AUTH_TOKEN`)

Если вместо интерактивного OAuth 2.1 используется административный токен `MCP_AUTH_TOKEN`:

### OpenCode (`oauth: false`)
```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "bitrix24": {
      "type": "remote",
      "url": "https://your-server.example.com/mcp",
      "oauth": false,
      "headers": {
        "Authorization": "Bearer {env:MCP_AUTH_TOKEN}"
      }
    }
  }
}
```

### Cursor / Windsurf / Claude Desktop
```json
{
  "mcpServers": {
    "bitrix24": {
      "url": "https://your-server.example.com/mcp",
      "headers": {
        "Authorization": "Bearer <MCP_AUTH_TOKEN>"
      }
    }
  }
}
```

### Claude Code CLI
```bash
claude mcp add --transport http bitrix24 https://your-server.example.com/mcp \
  --header "Authorization: Bearer <MCP_AUTH_TOKEN>"
```

---

## 2. Локальный режим `stdio` (без открытия сетевых портов)

Подходит для запуска бинарника `b24` напрямую как дочернего процесса MCP-клиента с входящим вебхуком:

```json
{
  "mcpServers": {
    "bitrix24": {
      "command": "/path/to/b24",
      "env": {
        "MCP_TRANSPORT": "stdio",
        "BITRIX_WEBHOOK_URL": "https://your-portal.bitrix24.ru/rest/1/abcdefghijklmnop/",
        "DATABASE_PATH": "/path/to/data/bitrix24-mcp.sqlite",
        "TOKEN_ENCRYPTION_KEY": "<hex-key>",
        "MCP_AUTH_TOKEN": "<hex-token>",
        "BITRIX_MCP_DEFAULT_MODE": "write_with_confirmation"
      }
    }
  }
}
```

---

## 3. Совместное использование с документационным сервером `b24-dev-mcp`

При разработке интеграций можно параллельно подключить официальный справочный MCP-сервер документации Битрикс24 (`https://mcp-dev.bitrix24.tech/mcp`):

```json
{
  "mcpServers": {
    "bitrix24": {
      "url": "https://your-server.example.com/mcp"
    },
    "b24-dev": {
      "url": "https://mcp-dev.bitrix24.tech/mcp"
    }
  }
}
```
