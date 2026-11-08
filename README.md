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

Создайте локальное приложение в Битрикс24 (**Разработчикам** → **Другое** → **Локальное приложение**, права `crm` и `user`) и задайте переменные окружения `.env`:

```dotenv
APP_URL=https://your-server.example.com
BITRIX_CLIENT_ID=local.xxxxxxxxxxxxxxxx.xxxxxxxx
BITRIX_CLIENT_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
BITRIX_PORTAL_DOMAIN=your-portal.bitrix24.ru
TOKEN_ENCRYPTION_KEY=<32-байтный_ключ_base64_или_hex>
MCP_AUTH_TOKEN=<случайная_строка_>=32_символов>
BITRIX_MCP_DEFAULT_MODE=write_with_confirmation
```

Запуск через Docker Compose:
```bash
docker compose up -d
```

---

## Подключение MCP-клиентов

Все клиенты подключаются к серверу по протоколу **Streamable HTTP + OAuth 2.1** (`https://your-server.example.com/mcp`) с авторизацией через браузер в вашем портале Битрикс24.

### ChatGPT (в браузере `chatgpt.com` и приложении ChatGPT)

1. Откройте **Settings (Настройки)** → **Connectors (Коннекторы / Приложения)**.
2. Перейдите в **Advanced (Дополнительно)** и включите **Developer mode (Режим разработчика)**.
3. Нажмите **Create (Создать)** и заполните поля:
   - **Name**: `Bitrix24 CRM`
   - **Description**: `Управление CRM Битрикс24: поиск и аналитика сделок, лидов, контактов, компаний, предложений, счетов и смарт-процессов, двухэтапное создание и изменение записей.`
   - **MCP Server URL**: `https://your-server.example.com/mcp`
   - **Authentication**: выберите **OAuth** (поля Client ID / Client Secret оставьте пустыми — регистрация происходит автоматически).
4. Нажмите **Create** и подтвердите вход в ваш портал Битрикс24 в открывшемся окне.

### Приложение Codex / ChatGPT Desktop

В настройках MCP-интеграций приложения укажите удалённый сервер с OAuth-авторизацией:

```json
{
  "mcpServers": {
    "bitrix24": {
      "url": "https://your-server.example.com/mcp"
    }
  }
}
```
При первом обращении приложение откроет браузер для входа через ваш портал Битрикс24.

### Claude (в браузере `claude.ai` и приложении Claude Desktop)

1. Откройте **Settings (Настройки)** → **Integrations (Интеграции / Connectors)** → **Add custom integration (Добавить интеграцию)**.
2. Укажите имя `Bitrix24` и URL:
   ```text
   https://your-server.example.com/mcp
   ```
3. Нажмите **Connect** и завершите вход в ваш портал Битрикс24 в браузере.

### OpenCode

Добавьте в `~/.config/opencode/opencode.json` (или локальный `opencode.json`):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "bitrix24": {
      "type": "remote",
      "url": "https://your-server.example.com/mcp"
    }
  }
}
```
При подключении откроется браузер для прохождения OAuth-авторизации в Битрикс24.

---

## Готовые AI-скиллы (`SKILL.md`)

В репозитории в папке [`.agents/skills/`](.agents/skills/) находятся два специализированных скилла для AI-агентов:

- **[`bitrix24-crm`](.agents/skills/bitrix24-crm/SKILL.md)** — операционная работа с CRM и смарт-процессами: таблица `entityTypeId`, правило Discovery-First (`bitrix_describe_entity` перед фильтрацией по стадиям и `UF_CRM_*`), реляционные dot-notation поля REST 3.0 без лишних N+1 запросов, сценарий «Досье на клиента 360°», шпаргалка по сложным фильтрам и строгий регламент двухшаговых мутаций (`prepare_*` → превью пользователю → `confirm_operation`).
- **[`bitrix24-analytics`](.agents/skills/bitrix24-analytics/SKILL.md)** — аналитика воронки продаж и аудит CRM: быстрые подсчёты через `bitrix_count_items` (`crm.<slug>.aggregate`), расчёт конверсии по стадиям, отчёты по эффективности менеджеров, UTM-атрибуция, товарная аналитика (`bitrix_list_product_rows`) и поиск «зависших» сделок.

---

## Документация

- [**Настройка Битрикс24 и авторизация**](docs/setup.md) — создание локального приложения в Битрикс24, способы авторизации портала и управление токенами.
- [**Конфигурация и переменные окружения**](docs/configuration.md) — полный справочник переменных `.env`, таймаутов, лимитов, режимов `BITRIX_API_VERSION` и безопасности.
- [**Деплой и эксплуатация**](docs/deployment.md) — Docker Compose, продакшн-деплой в Dokploy, резервное копирование SQLite и тесты.
- [**Дополнительные режимы подключения**](docs/advanced-clients.md) — локальный режим `stdio`, подключение со статическим токеном, CLI и `b24-dev-mcp`.
