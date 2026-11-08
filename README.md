# Битрикс24 MCP Сервер

Self-hosted [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) сервер для **Битрикс24 CRM** и смарт-процессов (SPA). Написан на чистом Go (`CGO_ENABLED=0`) со встроенной базой данных SQLite (`modernc.org/sqlite`) — не требует внешних СУБД или фоновых сервисов.

---

## Защитный шлюз vs прямой вебхук

Сервер работает как **интеллектуальный шлюз безопасности (Security Gateway & Proxy)** между LLM и Битрикс24, устраняя риски прямого вебхука:

- **Защита от галлюцинаций и prompt injection**: режим `read_only` по умолчанию. При записи операция сначала подготавливается (`prepare_*`), описывается человеку и выполняется только после явного подтверждения (`confirm_operation`). Деструктивные методы (`delete`, изменение прав и настроек портала) отсутствуют на уровне архитектуры.
- **Безопасность токенов**: вместо вечного открытого вебхука — официальный OAuth 2.0 с автоматической ротацией короткоживущих токенов и шифрованием на диске (**AES-256-GCM**).
- **Защита от лимитов API**: пакетный движок автоматически объединяет запросы в `batch` до 50 команд (защита от лимита Битрикс24 в 2 req/s) и пагинирует выборки.
- **Многопользовательская изоляция**: поддержка MCP OAuth 2.1 — каждый клиент или агент работает под своей учётной записью Битрикс24 с изолированным доступом и аудитом действий.
- **Кэширование схемы**: типы сущностей, поля, воронки и стадии кэшируются в локальной SQLite, снижая нагрузку на портал.

---

## Архитектура инструментов MCP

Инструменты самодокументируемы и передаются AI-клиентам динамически по протоколу MCP (`tools/list`). Они разделены на 4 группы:

1. **Подключения и обнаружение**: получение списка подключений, автоматическое обнаружение системных сущностей (Лиды, Сделки, Контакты, Компании, Счета) и смарт-процессов (`bitrix_list_connections`, `bitrix_discover_portal`, `bitrix_list_crm_entities`, `bitrix_describe_entity`).
2. **Схема и структура**: чтение метаданных полей (включая `ufCrm...`), воронок и стадий с кэшированием в SQLite (`bitrix_list_fields`, `bitrix_list_categories`, `bitrix_list_stages`, `bitrix_refresh_schema_cache`).
3. **Чтение данных и Batch API**: универсальный поиск по фильтрам, пакетное чтение карточек и выгрузка товарных позиций (`crm.item.productrow.list`) для десятков сделок за один запрос (`bitrix_search_items`, `bitrix_get_item`, `bitrix_list_product_rows`, `bitrix_list_users`).
4. **Двухшаговая безопасная запись**: подготовка изменений (`bitrix_prepare_create_item`, `bitrix_prepare_update_item`, `bitrix_prepare_move_item_stage`), их подтверждение (`bitrix_confirm_operation`) или отмена (`bitrix_cancel_operation`).

---

## Быстрый старт

### 1. Подготовка конфигурации

```bash
cp .env.example .env
```

Сгенерируйте секреты и укажите их в `.env`:

```bash
# Ключ шифрования токенов в SQLite (32 байта base64)
openssl rand -base64 32

# Статический Bearer-токен для доступа к MCP
openssl rand -hex 32
```

Минимально необходимые переменные в `.env`:
```dotenv
APP_URL=https://your-server.example.com
BITRIX_CLIENT_ID=local.xxxxxxxxxxxxxxxx.xxxxxxxx
BITRIX_CLIENT_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
TOKEN_ENCRYPTION_KEY=<ключ_из_openssl_base64>
MCP_AUTH_TOKEN=<токен_из_openssl_hex>
```

### 2. Запуск сервера

Через Docker Compose:

```bash
docker compose up --build -d
```

Или сборка бинарника Go:

```bash
CGO_ENABLED=0 go build -o b24 ./cmd/b24
./b24
```

Проверка состояния:
```bash
curl http://localhost:3000/health
# {"database":"connected","status":"ok"}
```

---

## Подключение MCP-клиентов

### Claude Code

Через интерактивный OAuth 2.1:
```bash
claude mcp add --transport http bitrix24 https://your-server.example.com/mcp
```

Или со статическим Bearer-токеном:
```bash
claude mcp add --transport http bitrix24 https://your-server.example.com/mcp \
  --header "Authorization: Bearer <MCP_AUTH_TOKEN>"
```

### Cursor / Windsurf / Claude Desktop

Добавьте в конфигурационный JSON:

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

### Режим Stdio (локальный процесс)

```json
{
  "mcpServers": {
    "bitrix24": {
      "command": "/path/to/b24",
      "env": {
        "MCP_TRANSPORT": "stdio",
        "APP_URL": "http://localhost:3000",
        "DATABASE_PATH": "/path/to/data/bitrix24-mcp.sqlite",
        "BITRIX_CLIENT_ID": "local.xxxx",
        "BITRIX_CLIENT_SECRET": "xxxx",
        "TOKEN_ENCRYPTION_KEY": "<base64-key>",
        "MCP_AUTH_TOKEN": "<hex-token>"
      }
    }
  }
}
```

---

## Документация

- [**Настройка Битрикс24 и авторизация**](docs/setup.md) — создание локального приложения в Битрикс24, способы авторизации портала и управление токенами.
- [**Конфигурация и переменные окружения**](docs/configuration.md) — полный справочник переменных `.env`, таймаутов, лимитов и режимов безопасности.
- [**Деплой и эксплуатация**](docs/deployment.md) — Docker Compose, продакшн-деплой в Dokploy, резервное копирование SQLite и тесты.
