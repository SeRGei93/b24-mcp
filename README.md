# Битрикс24 MCP Сервер

Лёгкий [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) сервер на Go для интеграции AI-ассистентов с **Битрикс24** (CRM и смарт-процессы).

Поддерживает новейший протокол **Bitrix24 REST 3.0** с автоматическим прозрачным фоллбэком на Legacy REST v2 для коробочных (On-Premise) версий. Собирается в один статический бинарник (`CGO_ENABLED=0`) со встроенной SQLite (`modernc.org/sqlite`) — запускается без внешних баз данных, Redis или дополнительной инфраструктуры.

---

## Контроль доступа и безопасность

Сервер добавляет строгий слой контроля доступа и защиты данных Битрикс24:

- **Двухэтапные мутации (Human-in-the-loop)**: по умолчанию включён режим `read_only`. Операции создания, изменения, смены стадии и удаления сделок/элементов сначала подготавливаются (`prepare_*`), показываются пользователю и выполняются только после явного подтверждения (`confirm_operation`) с передачей заголовка `Idempotency-Key` для предотвращения повторного выполнения.
- **Политика контролируемого удаления**: операция удаления (`bitrix_prepare_delete_item`) также защищена двухэтапным подтверждением и обязательной записью в аудит-лог SQLite.
- **Два режима авторизации**:
  - **Полноценный OAuth 2.0 / MCP OAuth 2.1** с автоматической ротацией токенов каждый час и шифрованием токенов на диске (AES-256-GCM).
  - **Входящий вебхук (`BITRIX_WEBHOOK_URL`)** для быстрого развёртывания без регистрации OAuth-приложений и без необходимости в публичных туннелях (ngrok/localtunnel).
- **Разделение пользователей**: поддержка MCP OAuth 2.1 позволяет разным сотрудникам или агентам работать под своими учётными записями с изолированным доступом.

---

## Возможности и инструменты

Сервер предоставляет 20 инструментов по протоколу MCP (`tools/list`), сгруппированных по 4 направлениям:

1. **Подключения и обнаружение**: список подключений, автоопределение сущностей CRM (Лиды, Сделки, Контакты, Компании, Счета, встроенные Предложения `Quote` и смарт-процессы) (`bitrix_list_connections`, `bitrix_discover_portal`, `bitrix_list_crm_entities`, `bitrix_describe_entity`).
2. **Схема и структура**: чтение метаданных полей (включая автоматическую нормализацию пользовательских полей `UF_CRM_*`), воронок и стадий через REST 3.0 с плоскими фильтрами и кэшированием в SQLite (`bitrix_list_fields`, `bitrix_list_categories`, `bitrix_list_stages`, `bitrix_refresh_schema_cache`).
3. **Чтение данных (REST 3.0 Read Engine)**:
   - Реляционные dot-notation поля в `select` (`stage.name`, `category.name`, `company.title`, `contact.name`, `assignedBy.firstName`, `assignedBy.lastName`).
   - Оптимизированные списки полей по умолчанию (wildcard `*` никогда не отправляется в REST 3.0).
   - Трансляция фильтров (поддержка операторов `=`, `!=`, `>`, `>=`, `<`, `<=`, `@` -> `in`, с автоматическим фоллбэком на legacy `crm.item.list` при substring-поиске `%` или `like`).
   - Пагинация до 1 000 записей за 1 HTTP-запрос (без циклов по 50 элементов).
   - Быстрый подсчёт через `crm.<slug>.aggregate` (`bitrix_count_items`).
   - Выгрузка товарных позиций (`bitrix_list_product_rows`) и пользователей (`bitrix_list_users`).
4. **Двухшаговые идемпотентные мутации**:
   - `bitrix_prepare_create_item` — подготовка создания элемента CRM;
   - `bitrix_prepare_update_item` — подготовка обновления полей элемента;
   - `bitrix_prepare_move_item_stage` — подготовка перевода сделки/лида/смарт-процесса на другую стадию;
   - `bitrix_prepare_delete_item` — подготовка удаления элемента CRM;
   - `bitrix_confirm_operation` — подтверждение и выполнение с заголовком `Idempotency-Key`;
   - `bitrix_cancel_operation` — отмена запланированной операции.

---

## Быстрый старт

### Вариант 1: Быстрый старт через входящий вебхук (Zero-Tunnel Setup)

Самый быстрый способ запустить сервер локально или в CLI без белого IP и туннелей:

1. В интерфейсе Битрикс24 перейдите: **Разработчикам** → **Другое** → **Входящий вебхук**.
2. В правах доступа отметьте **CRM** (`crm`) и **Пользователи** (`user`), сохраните вебхук.
3. Создайте `.env`:

```dotenv
BITRIX_WEBHOOK_URL=https://your-portal.bitrix24.ru/rest/1/abcdefghijklmnop/
TOKEN_ENCRYPTION_KEY=000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
MCP_AUTH_TOKEN=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
BITRIX_MCP_DEFAULT_MODE=write_with_confirmation
```

4. Запустите сервер:
```bash
go run ./cmd/b24
```

### Вариант 2: Полноценный OAuth 2.0 (Multi-Tenant)

Для использования в командных средах с ротацией токенов и разделением доступов:

```dotenv
APP_URL=https://your-server.example.com
BITRIX_CLIENT_ID=local.xxxxxxxxxxxxxxxx.xxxxxxxx
BITRIX_CLIENT_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
TOKEN_ENCRYPTION_KEY=<32-байтный_ключ_base64_или_hex>
MCP_AUTH_TOKEN=<статический_токен_>=32_символов>
BITRIX_MCP_DEFAULT_MODE=write_with_confirmation
```

---

## Подключение MCP-клиентов

### Claude Code

Через HTTP-транспорт со статическим Bearer-токеном:
```bash
claude mcp add --transport http bitrix24 https://your-server.example.com/mcp \
  --header "Authorization: Bearer <MCP_AUTH_TOKEN>"
```

### Cursor / Windsurf / Claude Desktop

Добавьте в конфигурационный JSON (`mcp.json` / `claude_desktop_config.json`):

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

### Режим Stdio (локальный процесс без открытия портов)

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

## Совместное использование с официальным сервером `b24-dev-mcp`

Официальный MCP-сервер от Битрикс24 ([`b24-dev-mcp`](https://mcp-dev.bitrix24.tech/mcp)) и данный сервер идеально дополняют друг друга в IDE (Cursor, Windsurf, Claude Desktop):

- **`b24-dev-mcp`** предоставляет контекст для разработчика: документацию по REST API Битрикс24, справочники методов и примеры SDK.
- **Данный сервер (`bitrix24`)** обеспечивает прямое защищённое взаимодействие с живым порталом: чтение данных в REST 3.0 с реляционными полями, аутентификацию через вебхук или OAuth, двухэтапные подтверждаемые мутации записей (`prepare_*` → `confirm`) и аудит действий в SQLite.

Пример совместной конфигурации:

```json
{
  "mcpServers": {
    "bitrix24": {
      "url": "https://your-server.example.com/mcp",
      "headers": {
        "Authorization": "Bearer <MCP_AUTH_TOKEN>"
      }
    },
    "b24-dev": {
      "url": "https://mcp-dev.bitrix24.tech/mcp"
    }
  }
}
```

---

## Документация

- [**Настройка Битрикс24 и авторизация**](docs/setup.md) — создание локального приложения, входящие вебхуки без туннелей, способы авторизации портала и управление токенами.
- [**Конфигурация и переменные окружения**](docs/configuration.md) — полный справочник переменных `.env`, таймаутов, лимитов, режимов `BITRIX_API_VERSION` и безопасности.
- [**Деплой и эксплуатация**](docs/deployment.md) — Docker Compose, продакшн-деплой в Dokploy, резервное копирование SQLite и тесты.
