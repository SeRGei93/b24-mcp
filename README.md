# Битрикс24 MCP Сервер

Лёгкий [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) сервер на Go для интеграции AI-ассистентов с **Битрикс24** (CRM и смарт-процессы).

Собирается в один статический бинарник (`CGO_ENABLED=0`) со встроенной SQLite (`modernc.org/sqlite`) — запускается без внешних баз данных, Redis или дополнительной инфраструктуры.

---

## Контроль доступа и безопасность

В отличие от прямого использования входящего вебхука, сервер добавляет базовый слой контроля:

- **Двухэтапная запись (Human-in-the-loop)**: по умолчанию включён режим `read_only`. Операции создания, изменения или смены стадии сделок сначала подготавливаются (`prepare_*`), показываются пользователю и выполняются только после явного подтверждения (`confirm_operation`).
- **Без опасных операций**: методов удаления данных (`delete`), изменения прав или настроек портала в сервере нет.
- **Безопасная работа с токенами**: стандартный OAuth 2.0 с автоматическим обновлением токенов вместо открытого вебхука с бессрочным доступом в конфигах. На диске токены хранятся в зашифрованном виде (AES-256-GCM).
- **Разделение пользователей**: поддержка MCP OAuth 2.1 позволяет разным сотрудникам или агентам работать под своими учётными записями с изолированным доступом.

---

## Возможности и инструменты

Инструменты самодокументируемы и передаются клиентам по протоколу MCP (`tools/list`). Они сгруппированы по 4 направлениям:

1. **Подключения и обнаружение**: список подключений, автоопределение сущностей CRM и смарт-процессов (`bitrix_list_connections`, `bitrix_discover_portal`, `bitrix_list_crm_entities`, `bitrix_describe_entity`).
2. **Схема и структура**: чтение метаданных полей (включая `ufCrm...`), воронок и стадий с кэшированием в SQLite (`bitrix_list_fields`, `bitrix_list_categories`, `bitrix_list_stages`, `bitrix_refresh_schema_cache`).
3. **Чтение данных**: поиск по фильтрам, выборка карточек и выгрузка товарных позиций через batch-запросы (`bitrix_search_items`, `bitrix_get_item`, `bitrix_list_product_rows`, `bitrix_list_users`).
4. **Двухшаговая запись**: подготовка изменений (`bitrix_prepare_create_item`, `bitrix_prepare_update_item`, `bitrix_prepare_move_item_stage`), их подтверждение (`bitrix_confirm_operation`) или отмена (`bitrix_cancel_operation`).

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
