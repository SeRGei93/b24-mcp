# Исследование актуальной документации Bitrix24 (REST 3.0, MCP, Лимиты, Авторизация) и план модернизации `bitrix24-mcp-server`

> **Дата исследования:** 2026-11-08  
> **Первоисточник документации:** Официальная документация Bitrix24 REST API (`https://apidocs.bitrix24.ru`)  
> **Анализируемая кодовая база:** `bitrix24-mcp-server` (`cmd/b24/`, `internal/bitrix/`, `internal/mcp/`, `internal/server/`, `internal/store/`, `internal/config/`)

---

## Резюме (Executive Summary)

1. **Официальный MCP-сервер Битрикс24 (`b24-dev-mcp`) не конкурирует с нашим `bitrix24-mcp-server`, а дополняет его.**  
   Официальный сервер Битрикс24 (`https://mcp-dev.bitrix24.tech/mcp`, описан на [`/ai-tools/mcp.html`](https://apidocs.bitrix24.ru/ai-tools/mcp.html)) — это **публичный справочный MCP-сервер по документации REST API** для IDE и AI-ассистентов разработчика (Cursor, Claude Code, Codex, Antigravity и др.). Он не подключается к порталам клиентов, не принимает токены/вебхуки и не читает/изменяет данные CRM. Наш проект (`bitrix24-mcp-server`) — это **runtime MCP-сервер исполнения бизнес-операций** над живыми данными портала Битрикс24 с OAuth 2.1, шифрованием токенов AES-256-GCM и двухшаговым подтверждением записи (`prepare_*` -> `confirm_operation`). Подключение `b24-dev-mcp` в `.mcp.json` нашего репозитория позволит AI-агентам при разработке сверять сигнатуры методов без галлюцинаций.

2. **Выход Bitrix24 REST 3.0 (`/rest/api/`) меняет фундаментальные контракты работы с CRM.**  
   Наш слой интеграции в `internal/bitrix/` полностью построен на предыдущей версии API (`/rest/{method}.json` и универсальных методах `crm.item.*`), которая согласно [`/api-reference/crm/migration.html`](https://apidocs.bitrix24.ru/api-reference/crm/migration.html) больше не развивается (новые возможности появляются только в REST 3.0). Переход на **REST 3.0** (`/rest/api/crm.<type>.<action>`) даёт радикальный прирост производительности и выразительности, но требует учёта жёстких изменений протокола:
   - **Пагинация до 1000 элементов за один HTTP-запрос** (`pagination: {"limit": 1000, "page": 1}`) вместо 20 батч-страниц по 50 элементов (`start`) ([`crm-entity-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-list-rest-v3.html)).
   - **Запрет символа `*` в `select` и возврат только `{"id": ...}` при пропуске `select`** ([`crm-entity-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-list-rest-v3.html), [`crm-entity-get-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-get-rest-v3.html), [`crm-entity-product-row-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/product-rows/crm-entity-product-row-list-rest-v3.html)). Текущий дефолт `select: ["*"]` в `internal/bitrix/crm.go:92-95` в REST 3.0 вызовет HTTP 400 (`BITRIX_REST_V3_EXCEPTION_UNKNOWNDTOPROPERTYEXCEPTION`), а `GetItem` без `select` (`internal/bitrix/crm.go:170-186`) вернёт только `id`.
   - **Реляционная выборка связанных сущностей за 1 запрос через точку** (`select: ["id", "title", "stage.name", "category.name", "assignedBy.firstName", "assignedBy.lastName", "company.title", "contact.name"]`) без дополнительных вызовов `user.get` или `crm.company.get` ([`rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/rest-v3.html), [`crm-entity-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-list-rest-v3.html)).
   - **Новый формат `filter` в виде массива троек и логических групп** `[["categoryId", "=", 69], ["opportunity", ">=", 10000]]` вместо JSON-объекта с префиксами операторов `{"=categoryId": 69, ">=opportunity": 10000}` ([`rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/rest-v3.html)).
   - **Новая объектная структура ошибок** `{"error": {"code": "...", "message": "...", "validation": [{"field": "...", "message": "..."}]}}` ([`rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/rest-v3.html)), которая сейчас не десериализуется нашей структурой `BitrixResponse` (`Error string` в `internal/bitrix/rest_client.go:37-43`).
   - **Нативный `Idempotency-Key` в HTTP-заголовках** ([`rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/rest-v3.html)), который идеально ложится на UUID операции из `OperationService` (`internal/bitrix/operation.go:123`).
   - **Гибридный режим (`/rest/api/` + `/rest/`) обязателен**: в REST 3.0 пока отсутствуют методы получения списка смарт-процессов (`crm.type.list`), стадий лидов/счетов/предложений (`crm.status.list`), воронок счетов (`crm.category.list`), а также методы `user.get`, `app.info`, `profile` ([`/api-reference/crm/index.html`](https://apidocs.bitrix24.ru/api-reference/crm/index.html), [`/api-reference/crm/migration.html`](https://apidocs.bitrix24.ru/api-reference/crm/migration.html)).

3. **Лимиты и авторизация требуют точечных доработок в `internal/bitrix/`.**
   - Наш клиент не парсит блок `time` (`operating`, `operating_reset_at`) в ответах Битрикс24 и при ошибке `OPERATION_TIME_LIMIT` (HTTP 429) делает короткий ретрай до 5 секунд вместо ожидания `operating_reset_at` ([`/limits.html`](https://apidocs.bitrix24.ru/limits.html), `internal/bitrix/rest_client.go:200-238`).
   - В `internal/bitrix/bitrix.go:520` захардкожен устаревший адрес `https://oauth.bitrix.info/oauth/token/`, тогда как актуальная документация указывает `https://oauth.bitrix24.tech/oauth/token/` ([`/settings/oauth/index.html`](https://apidocs.bitrix24.ru/settings/oauth/index.html)).
   - Поддержка **входящих вебхуков** ([`/local-integrations/local-webhooks.html`](https://apidocs.bitrix24.ru/local-integrations/local-webhooks.html)) как опционального режима подключения (наряду с OAuth 2.0) существенно упростит локальный запуск (`stdio` / single-tenant) без регистрации локального приложения.

---

## 1. Официальный MCP-сервер Битрикс24 (`b24-dev-mcp`) vs наш `bitrix24-mcp-server`

### 1.1. Что представляет собой официальный MCP Битрикс24

Согласно первоисточнику [`https://apidocs.bitrix24.ru/ai-tools/mcp.html`](https://apidocs.bitrix24.ru/ai-tools/mcp.html), компания Битрикс24 предоставляет публичный удалённый MCP-сервер **`b24-dev-mcp`**:

- **Endpoint:** `https://mcp-dev.bitrix24.tech/mcp`
- **Транспорт:** Streamable HTTP (`POST` с обязательным заголовком `Accept: application/json, text/event-stream`; без `text/event-stream` сервер возвращает `406 Not Acceptable`; устаревший `/sse` не поддерживается).
- **Назначение:** Доступ AI-ассистентов (Cursor, Claude Code, Codex, GitHub Copilot, Gemini CLI, Google Antigravity) к **актуальной справочной базе документации REST API Битрикс24** во время написания кода интеграций, чтобы модель не выдумывала несуществующие методы и параметры.
- **Авторизация и доступ к данным:** Не требует авторизации, **не подключается к порталам Битрикс24**, не принимает токены или вебхуки и не выполняет запросов к рабочим данным CRM.
- **Предоставляемые инструменты (5 tools):**
  1. `bitrix-search` — полнотекстовый/семантический поиск по документации (параметры: `query`, `doc_type` ∈ `[method, event, other, app_development_docs]`, `limit`).
  2. `bitrix-method-details` — детальная спецификация метода REST API (параметры, пример запроса/ответа; поддерживает фильтрацию разделов через `field` и `filter`).
  3. `bitrix-event-details` — описание события Битрикс24.
  4. `bitrix-article-details` — получение полной обучающей статьи или справочника (типы данных, ошибки, виджеты).
  5. `bitrix-app-development-doc-details` — документация по разработке тиражных и локальных приложений.

*(Примечание: пакет `@bitrix24/b24-mcp` или домен `mcp-b24-dev.bitrix24.tech` в актуальной редакции [`/ai-tools/mcp.html`](https://apidocs.bitrix24.ru/ai-tools/mcp.html) не используются — официальное имя сервера в конфигурации клиентов `b24-dev-mcp`, а канонический URL — `https://mcp-dev.bitrix24.tech/mcp`.)*

### 1.2. Сравнение `b24-dev-mcp` и нашего `bitrix24-mcp-server`

| Критерий | Официальный `b24-dev-mcp` ([`/ai-tools/mcp.html`](https://apidocs.bitrix24.ru/ai-tools/mcp.html)) | Наш `bitrix24-mcp-server` (`internal/mcp/server.go`, `internal/server/server.go`) |
| :--- | :--- | :--- |
| **Категория / Роль** | **Design-time / Dev-time Documentation MCP** (справочник по API для разработчика) | **Runtime Business Data MCP** (выполнение операций в живом портале Битрикс24) |
| **Целевой пользователь** | Разработчик интеграций в IDE (Cursor, Claude Code, Antigravity) | Бизнес-пользователь / AI-агент (Claude, ChatGPT, корпоративный агент), работающий с CRM |
| **Объект взаимодействия** | Статическая база знаний `apidocs.bitrix24.ru` | Живой портал клиента (`https://<portal>.bitrix24.ru/rest/...`) |
| **Аутентификация** | Отсутствует (публичный read-only endpoint) | OAuth 2.1 + PKCE для MCP-клиентов (`internal/mcp/oauth.go`) + OAuth 2.0 Bitrix24 с шифрованием токенов AES-256-GCM (`internal/store/store.go`) |
| **Инструменты (Tools)** | `bitrix-search`, `bitrix-method-details`, `bitrix-event-details`, `bitrix-article-details`, `bitrix-app-development-doc-details` | 18 инструментов работы с порталом (`bitrix_list_connections`, `bitrix_discover_portal`, `bitrix_list_crm_entities`, `bitrix_describe_entity`, `bitrix_list_fields`, `bitrix_list_categories`, `bitrix_list_stages`, `bitrix_refresh_schema_cache`, `bitrix_search_items`, `bitrix_get_item`, `bitrix_list_product_rows`, `bitrix_list_users`, `bitrix_prepare_create_item`, `bitrix_prepare_update_item`, `bitrix_prepare_move_item_stage`, `bitrix_confirm_operation`, `bitrix_cancel_operation`, `bitrix_list_pending_operations`) в `internal/mcp/server.go:39-654` |
| **Безопасность мутаций** | Только чтение документации | Политики доступа по соединениям (`internal/bitrix/policy.go`) + обязательный двухшаговый цикл `prepare_*` → `bitrix_confirm_operation` (`internal/bitrix/operation.go`) без методов удаления (`delete`) |
| **Транспорт** | Streamable HTTP (`https://mcp-dev.bitrix24.tech/mcp`) | Stateless Streamable HTTP (`POST /mcp`, `internal/server/server.go:606-619`) и `stdio` (`cmd/b24/main.go`) |

### 1.3. Как они дополняют друг друга

1. **Синергия при разработке `bitrix24-mcp-server`:** Добавление `b24-dev-mcp` (`https://mcp-dev.bitrix24.tech/mcp`) в конфигурацию `.mcp.json` / `.cursor/mcp.json` нашего репозитория позволяет AI-агентам во время рефакторинга Go-кода мгновенно запрашивать точные схемы методов REST 3.0 через `bitrix-method-details`.
2. **Чёткое позиционирование в `README.md`:** В документации нашего проекта стоит явно указать различие между официальным документационным MCP Битрикс24 и нашим runtime CRM MCP-сервером, чтобы у пользователей не возникало путаницы.

---

## 2. Сравнение текущего слоя API (`/rest/` + `crm.item.*`) и Bitrix24 REST 3.0 (`/rest/api/` + `crm.<type>.*`)

### Сводная таблица различий

| Аспект | Предыдущая версия API (`/rest/`) | Bitrix24 REST 3.0 (`/rest/api/`) | Текущая реализация в `bitrix24-mcp-server` |
| :--- | :--- | :--- | :--- |
| **URL-префикс** | `/rest/{method}.json` | `/rest/api/{method}` (регистрозависимый, только нижний регистр) | `ClientEndpoint` (`/rest/`) + `method + ".json"` в `internal/bitrix/rest_client.go:151`, `bitrix.go:138,423,473,488` |
| **HTTP-метод и формат** | `GET` или `POST` (`form-data` / `json`) | Только `POST` с `Content-Type: application/json` (query-параметры для методов с аргументами игнорируются) | Используется `POST` с `application/json` (`rest_client.go:173-178`) — **совместимо** |
| **Передача OAuth-токена** | Поле `"auth"` в теле или query | Поле `"auth"` в JSON-теле запроса | Передаётся `"auth"` в JSON-теле (`rest_client.go:157`) — **совместимо** |
| **Методы элементов CRM** | Универсальные `crm.item.list`, `crm.item.get`, `crm.item.add`, `crm.item.update`, `crm.item.delete` с параметром `entityTypeId` | Типизированные `crm.<type>.<action>`, где `<type>`: `lead`, `deal`, `contact`, `company`, `quote`, `smartinvoice`, `mycompany`, `smartprocess<entityTypeId>` | Везде используется `crm.item.*` с `entityTypeId` (`internal/bitrix/crm.go`, `operation.go`, `schema_discovery.go`) |
| **Структура успешного ответа** | Разнородная: `result.items`, `result.item`, `result.categories`, `result.productRows`, `result.fields`, либо корневой массив в `result` | Единый стандарт `{"result": ...}`: для списков (включая поля, воронки, стадии, товары) всегда `result.items`; для `get` — `result.item`; для `add` — `result.id`; для `update`/`delete` — `result.result: true` | Кастомные парсеры под каждый старый формат (`crm.go`, `schema_discovery.go`) |
| **Структура ошибки** | Плоские строки: `{"error": "CODE", "error_description": "Text"}` | Вложенный объект: `{"error": {"code": "...", "message": "...", "validation": [{"field": "...", "message": "..."}]}}` | `BitrixResponse.Error` имеет тип `string` (`rest_client.go:39`) — **сломается при unmarshal ошибки REST 3.0** |
| **Параметр `select`** | По умолчанию возвращает все поля; поддерживает `"*"` и `"UF_*"` | **`"*"` запрещён (HTTP 400)**; если `select` не передан, возвращается **только `{"id": ...}`**; поддержка вложенных полей через точку (`stage.name`, `assignedBy.lastName`, `company.title`) | При пустом `select` в `SearchItems` подставляется `[]string{"*"}` (`crm.go:93-95`); в `GetItem`/`GetItems`/`ListProductRows` `select` вообще не передаётся — **несовместимо с REST 3.0** |
| **Имена кастомных полей** | В `crm.item.*` — camelCase (`ufCrm_...`) | Верхний регистр `UF_CRM_...` | Ориентировано на ответы `crm.item.fields` (`schema_discovery.go:105-132`) |
| **Формат `filter`** | Объект с префиксами в ключах: `{"=stageId": "NEW", ">=opportunity": 1000}` | Массив троек и логических групп: `[["stageId", "=", "NEW"], ["opportunity", ">=", 1000]]` | `map[string]any` (`crm.go:100-110, 264-270`) |
| **Пагинация списков** | По 50 элементов через `start`; в ответе приходят `total` и `next` | Объект `pagination: {"limit": 1..1000, "page": 1..N}` (или `offset`); **до 1000 записей за 1 запрос**; поля `total` и `next` в ответе отсутствуют | `CollectPages` по 50 записей + до 20 команд в `CallBatch` (`rest_client.go:337-407`) |
| **Подсчёт `total` и агрегаты** | Возвращался в поле `total` каждого `list`-ответа | Отдельный метод `crm.<type>.aggregate` (`{"select": {"count": ["id"]}}`) | Читается из `firstResp.Total` (`rest_client.go:354`) |
| **Пакетные запросы (`batch`)** | `/rest/batch.json` с объектом `"cmd"` и URL-encoded строками `"method?k=v"` | `/rest/api/batch` с JSON-массивом объектов `[{"method": "...", "query": {...}}]` без URL-кодирования | Кодирует параметры в `url.Values` (`appendBatchParams` / `BuildBatchCommand` в `rest_client.go:52-108`) |
| **Идемпотентность записи** | Отсутствует на уровне протокола Битрикс24 | Заголовок `Idempotency-Key` (кэш на 24 часа, ответ `Idempotent-Replayed: true`) | Только локальный статус операции в SQLite (`internal/bitrix/operation.go:115-123`) без передачи `Idempotency-Key` в Битрикс24 |

---

### 2.1. Точка входа, протокол вызова и сосуществование `/rest/` и `/rest/api/`

- **Первоисточники:** [`https://apidocs.bitrix24.ru/api-reference/rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/rest-v3.html), [`https://apidocs.bitrix24.ru/api-reference/crm/migration.html`](https://apidocs.bitrix24.ru/api-reference/crm/migration.html).
- **Требования REST 3.0:**
  - Базовый путь для всех методов новой версии: `/rest/api/<method>` (для OAuth-приложений) или `/rest/<user_id>/<webhook_code>/api/<method>` (для входящих вебхуков).
  - Запросы выполняются методом `POST` с заголовком `Content-Type: application/json`. Если передать параметры в query-строке `GET`-запроса для метода, требующего параметров, сервер вернёт ошибку валидации `BITRIX_REST_V3_EXCEPTION_VALIDATION_REQUESTVALIDATIONEXCEPTION`.
  - Старый (`/rest/`) и новый (`/rest/api/`) эндпоинты работают параллельно над одной и той же базой данных и **могут комбинироваться в одном приложении** ([`migration.html`](https://apidocs.bitrix24.ru/api-reference/crm/migration.html)).
- **Состояние в нашем коде:**
  - В [`internal/bitrix/rest_client.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/rest_client.go#L151) метод `doPost` формирует URL как `fmt.Sprintf("%s/rest/%s.json", strings.TrimRight(endpoint, "/"), method)`.
  - Аналогично в [`internal/bitrix/bitrix.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/bitrix.go#L138) (`fetchPortalMetadata`) и [`internal/bitrix/bitrix.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/bitrix.go#L423) (`CallMethod`) жёстко зашит префикс `/rest/%s.json`.

### 2.2. Именование методов CRM и матрица типов сущностей (`crm.<type>.<action>`)

- **Первоисточники:** [`https://apidocs.bitrix24.ru/api-reference/crm/index.html`](https://apidocs.bitrix24.ru/api-reference/crm/index.html), [`https://apidocs.bitrix24.ru/api-reference/crm/migration.html`](https://apidocs.bitrix24.ru/api-reference/crm/migration.html).
- **Правила REST 3.0:**
  - Вместо передачи параметра `entityTypeId` в универсальные методы `crm.item.*` тип объекта встраивается непосредственно в имя метода в нижнем регистре: `crm.<type>.<action>`. Использование заглавных букв (например, `crm.smartProcess128.get`) приводит к HTTP 404 `BITRIX_REST_V3_EXCEPTION_METHODNOTFOUNDEXCEPTION`.
  - Таблица соответствия `entityTypeId` и `<type>` в REST 3.0:
    - `1` (Лид) → `lead` (`crm.lead.list`, `crm.lead.get`, `crm.lead.add`, `crm.lead.update`, `crm.lead.delete`, `crm.lead.aggregate`)
    - `2` (Сделка) → `deal` (`crm.deal.*`)
    - `3` (Контакт) → `contact` (`crm.contact.*`)
    - `4` (Компания) → `company` (`crm.company.*`)
    - `7` (Коммерческое предложение) → `quote` (`crm.quote.*`)
    - `31` (Счёт) → `smartinvoice` (`crm.smartinvoice.*`)
    - Реквизиты вашей компании → `mycompany` (`crm.mycompany.*`)
    - Динамический смарт-процесс с `entityTypeId = N` (например, `1038`) → `smartprocess1038` (`crm.smartprocess1038.*`). Сам список смарт-процессов и их `entityTypeId` по-прежнему запрашивается через `/rest/crm.type.list` ([`/api-reference/crm/index.html`](https://apidocs.bitrix24.ru/api-reference/crm/index.html)).
- **Расхождение в нашем коде:**
  - В [`internal/bitrix/schema_discovery.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/schema_discovery.go#L23-L29) список `BuiltInTypes` содержит `1` (Lead), `2` (Deal), `3` (Contact), `4` (Company), `31` (Smart Invoice), но **пропускает `7` (Quote / Предложение)**, хотя в [`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go#L23-L35) в `ToOwnerTypeCode` тип `7 -> "Q"` присутствует.

### 2.3. Унифицированный формат ответов и объектная структура ошибок в REST 3.0

- **Первоисточники:** [`https://apidocs.bitrix24.ru/api-reference/rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/rest-v3.html), [`crm-entity-get-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-get-rest-v3.html), [`crm-entity-add-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-add-rest-v3.html), [`crm-entity-update-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-update-rest-v3.html), [`crm-entity-aggregate-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-aggregate-rest-v3.html).
- **Успешные ответы в REST 3.0:**
  - `crm.<type>.get` → `{"result": {"item": {"id": 15, ...}}, "time": {...}}`
  - `crm.<type>.list`, `crm.<type>.field.list`, `crm.<type>.field.custom.list`, `crm.<type>.category.list`, `crm.<type>.category.stage.list`, `crm.<type>.productrow.list` → всегда `{"result": {"items": [...]}, "time": {...}}`
  - `crm.<type>.add` → `{"result": {"id": 9023}, "time": {...}}` (в отличие от старого `crm.item.add`, который возвращал `{"result": {"item": {"id": ...}}}` и требовал парсинга в [`internal/bitrix/operation.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/operation.go#L412-L448))
  - `crm.<type>.update` и `crm.<type>.delete` → `{"result": {"result": true}, "time": {...}}`
  - `crm.<type>.aggregate` → `{"result": {"result": {"count": {"id": 3}}}, "time": {...}}`
- **Формат ошибок в REST 3.0 (критическая несовместимость с текущим кодом):**
  - В предыдущей версии `/rest/` ошибка возвращалась на верхнем уровне как две строки:
    ```json
    {"error": "ERROR_CODE", "error_description": "Human readable message"}
    ```
  - В REST 3.0 поле `error` — это **вложенный JSON-объект** с кодом исключения, сообщением и опциональным массивом детальных ошибок валидации по каждому полю:
    ```json
    {
      "error": {
        "code": "BITRIX_REST_V3_EXCEPTION_VALIDATION_REQUESTVALIDATIONEXCEPTION",
        "message": "Ошибка при валидации объекта запроса",
        "validation": [
          {
            "field": "id",
            "message": "Обязательное поле `id` не указано"
          }
        ]
      }
    }
    ```
  - В [`internal/bitrix/rest_client.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/rest_client.go#L37-L43) структура `BitrixResponse` объявлена так:
    ```go
    type BitrixResponse struct {
        Result           json.RawMessage `json:"result"`
        Total            int             `json:"total,omitempty"`
        Next             int             `json:"next,omitempty"`
        Error            string          `json:"error,omitempty"`
        ErrorDescription string          `json:"error_description,omitempty"`
    }
    ```
    При получении любой ошибки от `/rest/api/` вызов `json.Unmarshal(rawBody, &parsed)` в `rest_client.go:196` завершится ошибкой `json: cannot unmarshal object into Go struct field BitrixResponse.error of type string`, скрывая от MCP-клиента и LLM реальную причину ошибки и массив `validation`.

### 2.4. Правила выборки полей (`select`), запрет `*`, регистр `UF_CRM_*` и реляционная выборка через точку

- **Первоисточники:** [`https://apidocs.bitrix24.ru/api-reference/rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/rest-v3.html), [`crm-entity-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-list-rest-v3.html), [`crm-entity-get-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-get-rest-v3.html).
- **Жёсткие правила `select` в REST 3.0:**
  1. **Маска `"*"` не поддерживается.** Передача `"select": ["*"]` возвращает HTTP 400 с кодом `BITRIX_REST_V3_EXCEPTION_UNKNOWNDTOPROPERTYEXCEPTION` («Неизвестное поле `*` в объекте `Item`»).
  2. **Если `select` не передан или пуст, возвращается только `{"id": ...}`.** Это касается `crm.<type>.get`, `crm.<type>.list` и `crm.<type>.productrow.list`.
  3. **Поле `id` не добавляется автоматически**, если передан непустой `select` без `"id"` (например, `select: ["title"]` вернёт только `{"title": "..."}` без `id`).
  4. **Пользовательские поля в REST 3.0 указываются в верхнем регистре `UF_CRM_...`** (например, `UF_CRM_1771944046`), тогда как в старом `crm.item.*` использовался camelCase `ufCrm...` ([`crm-entity-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-list-rest-v3.html), [`crm-entity-add-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-add-rest-v3.html)).
- **Реляционная выборка связанных объектов (ключевая киллер-фича REST 3.0 для AI/MCP):**
  - В REST 3.0 параметр `select` позволяет за **один запрос** получать поля связанных сущностей (глубина — одна точка `.`):
    - Указание имени связи без суффикса `Id` (например, `"stage"`, `"category"`, `"assignedBy"`, `"company"`) возвращает вложенный объект со всеми доступными полями связанной сущности.
    - Указание точечной нотации (`"stage.name"`, `"category.name"`, `"assignedBy.id"`, `"assignedBy.firstName"`, `"assignedBy.lastName"`, `"company.title"`, `"contact.name"`) возвращает компактный вложенный объект только с нужными полями:
      ```json
      {
        "id": 15,
        "title": "Продажа оборудования",
        "stage": {"name": "Подготовка документов"},
        "assignedBy": {"id": 1, "firstName": "Иван", "lastName": "Иванов"},
        "company": {"title": "ООО Ромашка"}
      }
      ```
  - Для нашего MCP-сервера это означает, что LLM при вызове `list_items` / `get_item` / `search_crm` сразу видит человекочитаемые имена стадий, воронок, ответственных сотрудников и компаний без необходимости делать дополнительные вызовы `get_users` или `get_entity_schema`.
- **Состояние в нашем коде:**
  - В [`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go#L92-L95) в `ListItems`:
    ```go
    selectFields := params.Select
    if len(selectFields) == 0 {
        selectFields = []string{"*"}
    }
    ```
  - В `GetItem` ([`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go#L170-L186)) и `GetItems` ([`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go#L188-L233)) параметр `select` вообще не передаётся.
  - В REST 3.0 оба этих места перестанут работать без формирования осмысленного дефолтного набора полей (`defaultSelectFields(entityTypeId)` + реляционные поля `stage.name`, `category.name`, `assignedBy.firstName`, `assignedBy.lastName`) и автоматического включения `"id"`.

### 2.5. Новый формат фильтрации (`filter`)

- **Первоисточники:** [`https://apidocs.bitrix24.ru/api-reference/rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/rest-v3.html), [`crm-entity-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-list-rest-v3.html).
- **Как устроен `filter` в REST 3.0:**
  - Передача старого JSON-объекта `{"categoryId": 69}` вызывает ошибку `BITRIX_REST_V3_EXCEPTION_INVALIDFILTEREXCEPTION`.
  - Фильтр в REST 3.0 — это массив условий и логических групп:
    ```json
    "filter": [
      ["categoryId", "=", 69],
      ["stageId", "in", ["C69:NEW", "C69:PREPARATION"]],
      ["opportunity", "between", [10000, 100000]],
      {
        "type": "filter",
        "logic": "or",
        "negative": false,
        "conditions": [
          ["assignedById", "=", 1],
          ["assignedById", "=", 6]
        ]
      }
    ]
    ```
  - **Поддерживаемые операторы:** `=`, `!=`, `>`, `>=`, `<`, `<=`, `in`, `between`. *(Важно: оператор подстрочного поиска `like` / `%` в `filter` REST 3.0 не поддерживается!)*
  - **Фильтрация по связанным сущностям через точку:** поддерживается для связанных объектов CRM, например:
    - `["company.title", "=", "ООО Ромашка"]`
    - `["phone.value", "=", "+79991112233"]`
    - `["email.value", "=", "client@example.com"]`
    - `["utm.source", "=", "google"]`
    *(Фильтрация по вложенным полям системного пользователя вроде `assignedBy.name` не поддерживается — для пользователей используется `["assignedById", "=", 1]`).*
  - **Строгая типизация значений в фильтре:**
    - Булевы поля принимают только `true` / `false` (строки `"Y"` / `"N"`, числа `1` / `0` вызовут ошибку валидации).
    - Поля `date-time` обязаны содержать часовой пояс в формате ISO-8601 (`"2026-11-03T14:41:00+03:00"`), иначе возникает `BITRIX_REST_V3_EXCEPTION_INVALIDFILTEREXCEPTION`.
  - **Ограничения фильтра:** глубина вложенности групп — до 8 уровней; максимум 100 простых условий и 100 групп; в операторе `in` — до 1000 значений.
- **Состояние в нашем коде и влияние на `SearchCRM`:**
  - В [`internal/mcp/server.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/mcp/server.go#L142-L148) описание параметра `filter` в `list_items` ориентирует LLM на старый map-формат (`map[string]any`).
  - В [`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go#L235-L309) метод `SearchCRM` использует legacy-фильтр по подстроке `"%TITLE"`, `"%NAME"`, `"%LAST_NAME"` через `crm.item.list`. Поскольку в REST 3.0 нет оператора `like`/`%` в `filter`, для `SearchCRM` целесообразно:
    - при поиске по телефону/email или точному совпадению использовать REST 3.0 `[["phone.value", "=", query]]` / `[["email.value", "=", query]]`;
    - для полнотекстового/подстрочного поиска по названию оставить fallback на `/rest/crm.item.list` с `"%TITLE"` (либо комбинировать оба подхода), пока в REST 3.0 не появится оператор `like` или полнотекстовый поиск.

### 2.6. Пагинация до 1000 элементов (`pagination`) и агрегация (`aggregate`) vs `CollectPages`

- **Первоисточники:** [`crm-entity-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-list-rest-v3.html), [`crm-entity-aggregate-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-aggregate-rest-v3.html).
- **Как работает пагинация в REST 3.0:**
  - Старый параметр `start` игнорируется, а поля `total` и `next` в ответе `list` больше не возвращаются.
  - Вместо них передаётся объект `pagination`:
    ```json
    "pagination": {
      "limit": 1000,
      "page": 1
    }
    ```
    *(также поддерживается `"offset"` вместо `"page"`, но одновременная передача `page` и `offset` запрещена).*
  - Максимальный `limit` за один запрос — **1000 записей** (по умолчанию `50`).
  - Признак последней страницы: количество элементов в `result.items` меньше запрошенного `limit`.
  - Для получения общего количества записей (`total`) без выгрузки самих элементов используется специальный метод [`crm.<type>.aggregate`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-aggregate-rest-v3.html) (на данный момент принимает только `"select": {"count": ["id"]}`):
    ```json
    {
      "select": {
        "count": ["id"]
      },
      "filter": [["categoryId", "=", 0]]
    }
    ```
- **Выигрыш для нашего кода (`internal/bitrix/rest_client.go`):**
  - Сейчас `CollectPages` ([`internal/bitrix/rest_client.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/rest_client.go#L337-L407)) для выгрузки 1000 элементов (`BitrixMaxPages * BitrixBatchLimit = 20 * 50`) сначала делает 1 запрос первой страницы, а затем формирует `CallBatch` из 19 подзапросов.
  - В REST 3.0 выборка до 1000 элементов выполняется **за 1 прямой HTTP-запрос** с `pagination: {limit: 1000, page: 1}` без конструирования батча и URL-кодирования параметров!
  - Кроме того, `crm.<type>.aggregate` позволяет добавить в MCP-сервер быстрый подсчёт количества элементов по фильтру (`count: ["id"]`) за доли секунды без выгрузки самих записей в контекст LLM.

### 2.7. Массовое получение элементов по ID (`GetItems`): 1 запрос вместо 5 батчей по 50

- **Первоисточники:** [`crm-entity-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-list-rest-v3.html).
- **Сравнение:**
  - Сейчас `GetItems` в [`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go#L188-L233) ограничен `MaxGetItemsBatch = 250` и отправляет до 250 индивидуальных команд `crm.item.get` через `/rest/batch.json` (5 HTTP-вызовов батча по 50 команд).
  - В REST 3.0 оператор `"in"` в фильтре поддерживает до **1000 значений**, а `pagination.limit` — до **1000 записей**. Это значит, что `GetItems` в REST 3.0 может получить до 1000 карточек CRM по их ID **за один-единственный вызов `crm.<type>.list`**:
    ```json
    {
      "select": ["id", "title", ...],
      "filter": [["id", "in", [101, 102, 103]]],
      "pagination": {"limit": 1000, "page": 1}
    }
    ```

### 2.8. Новый формат пакетных запросов (`/rest/api/batch`)

- **Первоисточник:** [`https://apidocs.bitrix24.ru/api-reference/rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/rest-v3.html).
- **Как работает `/rest/api/batch` в REST 3.0:**
  - В старом `/rest/batch.json` команды передавались в объекте `"cmd"` в виде URL-encoded строк (`"cmd0": "crm.item.get?entityTypeId=2&id=15"`). При попытке отправить такую строку в `/rest/api/batch` сервер возвращает ошибку `BITRIX_REST_V3_EXCEPTION_INVALIDSELECTEXCEPTION`.
  - В REST 3.0 тело запроса к `/rest/api/batch` представляет собой **JSON-массив объектов** с нативным JSON внутри `"query"` (без превращения в query-string):
    ```json
    [
      {
        "method": "crm.deal.get",
        "query": {
          "id": 1,
          "select": ["id", "title", "stageId"]
        }
      },
      {
        "method": "crm.contact.list",
        "query": {
          "select": ["id", "name"],
          "filter": [["id", "in", [10, 20]]]
        }
      }
    ]
    ```
  - Ответ возвращается в виде позиционного массива `{"result": [ ... ], "time": { ... }}`.
- **Состояние в нашем коде:**
  - В [`internal/bitrix/rest_client.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/rest_client.go#L52-L108) написана сложная функция `encodeJSONToQuery`, рекурсивно сериализующая JSON-параметры в `url.Values` для `/rest/batch.json`. Для `/rest/api/batch` эта сериализация не нужна — параметры передаются как `json.RawMessage` / `map[string]any` напрямую в поле `"query"`.

### 2.9. Идемпотентность операций записи (`Idempotency-Key`)

- **Первоисточник:** [`https://apidocs.bitrix24.ru/api-reference/rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/rest-v3.html) (раздел «Идемпотентность»).
- **Механизм в REST 3.0:**
  - Для защиты от дублей при сетевых сбоях и таймаутах методы создания/изменения в `/rest/api/` поддерживают HTTP-заголовок:
    ```http
    Idempotency-Key: 550e8400-e29b-41d4-a716-446655440000
    ```
    *(длина от 1 до 255 ASCII-символов, рекомендуется UUID).*
  - При первом успешном выполнении Битрикс24 сохраняет результат на **24 часа**. При повторном запросе с тем же ключом операция не выполняется повторно, а клиенту возвращается сохранённый ответ с заголовком `Idempotent-Replayed: true`.
  - Если повторный запрос пришёл, пока первый ещё обрабатывается на сервере, возвращается ошибка `BITRIX_REST_V3_EXCEPTION_IDEMPOTENCYOPERATIONINPROGRESSEXCEPTION`.
- **Интеграция с нашим `OperationService` (`internal/bitrix/operation.go`):**
  - В [`internal/bitrix/operation.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/operation.go#L123) каждая подготовленная операция (`prepare_create_item`, `prepare_update_item`, `prepare_delete_item`) уже получает уникальный UUID (`claimed.ID`).
  - Сейчас при вызове `Confirm` (`operation.go:236-308`), если HTTP-соединение оборвалось после того, как Битрикс24 создал сделку, но до того, как ответ дошёл до нашего сервера, повторная попытка может создать дубликат.
  - Передача `Idempotency-Key: <claimed.ID>` в HTTP-заголовке при вызове REST 3.0 из `executeOperation` (`operation.go:355-409`) обеспечивает **сквозную (end-to-end) идемпотентность** между нашим хранилищем и ядром Битрикс24.

### 2.10. Обнаружение схемы портала (`schema_discovery.go`) и таблица гибридной маршрутизации

- **Первоисточники:**
  - [`crm-entity-field-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/fields/crm-entity-field-list-rest-v3.html)
  - [`categories/index.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/categories/index.html)
  - [`crm-entity-category-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/categories/crm-entity-category-list-rest-v3.html)
  - [`crm-entity-category-stage-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/categories/crm-entity-category-stage-list-rest-v3.html)
  - [`migration.html`](https://apidocs.bitrix24.ru/api-reference/crm/migration.html)

#### Что переехало в REST 3.0, а что остаётся на `/rest/`:

| Задача в `schema_discovery.go` / `crm.go` | Текущий метод (`/rest/`) | Метод в REST 3.0 (`/rest/api/`) | Важные особенности и ограничения REST 3.0 |
| :--- | :--- | :--- | :--- |
| Список смарт-процессов (SPA) | `/rest/crm.type.list` | **Нет в REST 3.0** — остаётся `/rest/crm.type.list` | Согласно [`/api-reference/crm/index.html`](https://apidocs.bitrix24.ru/api-reference/crm/index.html) и [`migration.html`](https://apidocs.bitrix24.ru/api-reference/crm/migration.html), `entityTypeId` смарт-процессов по-прежнему получается через `/rest/crm.type.list`. |
| Список всех полей сущности | `/rest/crm.item.fields` (возвращает map `result.fields`) | `/rest/api/crm.<type>.field.list` (возвращает массив `result.items`) | Каждый элемент содержит `name`, `type`, `elementType`, `multiple`, `title`, `description`, `validationRules`, `requiredGroups`, `filterable`, `sortable`, `editable` ([`crm-entity-field-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/fields/crm-entity-field-list-rest-v3.html)). Для значений списков пользовательских полей есть `/rest/api/crm.<type>.field.custom.list`. |
| Список воронок (категорий) | `/rest/crm.category.list` | `/rest/api/crm.<type>.category.list` (для `deal`, `contact`, `company`, `smartprocess<id>`) | Возвращает `result.items`. **Исключение:** для счетов (`smartinvoice`, `entityTypeId=31`) метод в REST 3.0 отсутствует — нужно вызывать `/rest/crm.category.list` ([`migration.html`](https://apidocs.bitrix24.ru/api-reference/crm/migration.html)). |
| Список стадий воронки (Сделки и Смарт-процессы) | `/rest/crm.status.list` с фильтром по `ENTITY_ID` | `/rest/api/crm.<type>.category.stage.list` (для `deal` и `smartprocess<id>`) | **Внимание на специфику фильтра!** В [`crm-entity-category-stage-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/categories/crm-entity-category-stage-list-rest-v3.html) указано, что `filter` здесь передаётся как **плоский массив из 3 элементов** `"filter": ["categoryId", "=", 127]`, а не вложенный `[["categoryId", "=", 127]]` (вложенный массив вызовет ошибку фильтрации). |
| Список стадий Лидов (`1`), Предложений (`7`) и Счетов (`31`) | `/rest/crm.status.list` | **Нет в REST 3.0** — остаётся `/rest/crm.status.list` | Согласно [`categories/index.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/categories/index.html) и [`migration.html`](https://apidocs.bitrix24.ru/api-reference/crm/migration.html), стадии лидов (`STATUS`), счетов (`SMART_INVOICE_STAGE_*`) и коммерческих предложений (`QUOTE_STATUS`) запрашиваются только через старый `/rest/crm.status.list`. |
| Пользователи и метаданные портала | `/rest/user.get`, `/rest/app.info`, `/rest/profile` | **Нет в REST 3.0** (или в разработке) — остаются на `/rest/` | Методы `/rest/api/rest.scope.list` и `/rest/api/rest.documentation.openapi` доступны в REST 3.0 для интроспекции прав и OpenAPI-схемы ([`rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/rest-v3.html)). |

### 2.11. Товарные позиции (`crm.<type>.productrow.list`)

- **Первоисточник:** [`https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/product-rows/crm-entity-product-row-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/product-rows/crm-entity-product-row-list-rest-v3.html).
- **Сравнение с текущей реализацией (`ListProductRows` в `internal/bitrix/crm.go:311-365`):**
  - Сейчас `ListProductRows` вызывает `/rest/crm.item.productrow.list` с фильтром `{"=ownerType": ownerTypeCode, "=ownerId": itemID}` (где `ownerTypeCode` — краткий символьный код `"D"`, `"L"`, `"SI"`, `"Tb..."` из `ToOwnerTypeCode` в `crm.go:19-46`) и парсит `result.productRows`.
  - В REST 3.0 вызывается `/rest/api/crm.<type>.productrow.list` (поддерживаются `deal`, `lead`, `quote`, `smartinvoice`, `smartdocument`, `smartprocess<entityTypeId>`):
    - Символьный код `ownerType` больше не нужен, так как тип сущности указан в URL метода!
    - Параметр `filter` обязателен и принимает ровно один `ownerId` с оператором `=`: `"filter": [["ownerId", "=", 2823]]`.
    - **Критично:** без параметра `select` метод возвращает только `id` товарной позиции! Необходимо явно передавать:
      ```json
      "select": [
        "id", "ownerId", "productId", "productName",
        "price", "priceAccount", "priceExclusive", "priceNetto", "priceBrutto",
        "quantity", "discountTypeId", "discountRate", "discountSum",
        "taxRate", "taxIncluded", "customized",
        "measureCode", "measureName", "sort", "xmlId", "type", "storeId", "reserveId", "dateReserveEnd", "reserveQuantity"
      ]
      ```
    - Ответ возвращается в `result.items`, а `pagination.limit` поддерживает до `1000` позиций за один запрос.

---

## 3. Лимиты REST API, контроль `operating` и сравнение OAuth 2.0 vs Входящие вебхуки

### 3.1. Лимиты запросов (`QUERY_LIMIT_EXCEEDED`, `OPERATION_TIME_LIMIT`) и диагностика `time`

- **Первоисточник:** [`https://apidocs.bitrix24.ru/limits.html`](https://apidocs.bitrix24.ru/limits.html).
- **Два независимых контура лимитирования в Битрикс24:**
  1. **Лимит интенсивности запросов (Leaky Bucket) → HTTP `503` `QUERY_LIMIT_EXCEEDED`:**
     - Считается суммарно по порталу с одного IP-адреса для всех вебхуков и приложений.
     - На обычных тарифах: скорость вытекания `Y = 2 запроса/сек`, объём ведра (burst) `X = 50 запросов`. На тарифе «Энтерпрайз»: `Y = 5 запросов/сек`, `X = 250 запросов`.
     - В `batch`-запросе учитывается только сам вызов `batch` как 1 запрос по интенсивности (но время выполнения всех команд внутри батча суммируется во втором лимите).
  2. **Лимит времени выполнения (`operating`) → HTTP `429` `OPERATION_TIME_LIMIT`:**
     - Максимальное время одного запроса (включая `batch`) — **60 секунд**.
     - Суммарное время выполнения **конкретного метода** за скользящее окно в **10 минут** (10 одноминутных слотов) ограничено **480 секундами** (в облаке; в коробке — 420 с).
     - В каждом ответе Битрикс24 возвращает объект `time`:
       ```json
       "time": {
         "start": 1720626993.413939,
         "finish": 1720626993.83034,
         "duration": 0.4164009094238281,
         "processing": 0.03223896026611328,
         "date_start": "2024-07-10T18:56:33+03:00",
         "date_finish": "2024-07-10T18:56:33+03:00",
         "operating_reset_at": 1720627593,
         "operating": 3.317324161529541
       }
       ```
     - Если `operating` достигает 480 секунд, метод блокируется с кодом `OPERATION_TIME_LIMIT` (HTTP 429) **до момента `operating_reset_at`** (Unix timestamp).
- **Уязвимые места в нашей реализации (`internal/bitrix/rest_client.go`):**
  1. В [`internal/bitrix/rest_client.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/rest_client.go#L37-L43) структура `BitrixResponse` **не парсит поле `time`**. Мы теряем информацию о `duration`, `operating` и `operating_reset_at`.
  2. В `isRetryableBitrixError` (`rest_client.go:250-256`) при `OPERATION_TIME_LIMIT` вызывается `sleepBackoff` (`rest_client.go:631-678`), который ждёт максимум `BitrixMaxBackoff = 5s` и делает 3 попытки. Но блокировка `OPERATION_TIME_LIMIT` длится от 1 до 10 минут (до `operating_reset_at`)! Три быстрых ретрая за 8 секунд гарантированно завершатся неудачей.
  3. **Что нужно сделать:**
     - Добавить структуру `BitrixTime` в `BitrixResponse`.
     - Логировать предупреждение (`slog.Warn`), когда `time.operating > 350` секунд (проактивный сигнал о приближении к лимиту).
     - При ошибке `OPERATION_TIME_LIMIT`, если до `operating_reset_at` осталось больше разумного бюджета ожидания запроса (например, > 15 секунд), немедленно возвращать клиенту понятную ошибку с временем разблокировки (`operating_reset_at`), не тратя впустую 3 ретрая; если осталось ≤ 15 секунд — ждать до `operating_reset_at`.

### 3.2. Сервер авторизации OAuth 2.0: `oauth.bitrix24.tech` vs `oauth.bitrix.info`

- **Первоисточник:** [`https://apidocs.bitrix24.ru/settings/oauth/index.html`](https://apidocs.bitrix24.ru/settings/oauth/index.html).
- **Что изменилось в документации Битрикс24:**
  - Официальный сервер авторизации OAuth 2.0 в документации — **`https://oauth.bitrix24.tech/oauth/token/`** (в ответах также возвращается `"server_domain": "oauth.bitrix24.tech"` и `"server_endpoint": "https://oauth.bitrix24.tech/rest/"`).
  - Срок жизни `code` (authorization code) составляет **30 секунд**, `access_token` — **1 час (3600 с)**, `refresh_token` — **180 дней**.
- **Состояние в нашем коде:**
  - В [`internal/bitrix/bitrix.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/bitrix.go#L520) (`defaultOAuthTokenURL`) и в [`internal/server/server.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/server/server.go#L353) при проверке/обмене токенов захардкожен старый домен `https://oauth.bitrix.info/oauth/token/`.
  - Хотя `oauth.bitrix.info` пока сохраняет обратную совместимость, рекомендуется вынести URL сервера OAuth в конфигурацию (по умолчанию `https://oauth.bitrix24.tech/oauth/token/`) или сохранять `auth_server_url` из `server_endpoint` конкретного портала при первичном обмене кода.

### 3.3. OAuth 2.0 vs Входящие вебхуки (`local-webhooks.html`)

- **Первоисточники:** [`https://apidocs.bitrix24.ru/settings/oauth/index.html`](https://apidocs.bitrix24.ru/settings/oauth/index.html), [`https://apidocs.bitrix24.ru/local-integrations/local-webhooks.html`](https://apidocs.bitrix24.ru/local-integrations/local-webhooks.html), [`https://apidocs.bitrix24.ru/limits.html`](https://apidocs.bitrix24.ru/limits.html).
- **Сравнение подходов:**
  - **OAuth 2.0 (наша текущая реализация в `internal/bitrix/bitrix.go`):**
    - Требует регистрации локального или тиражного приложения (`BITRIX_CLIENT_ID`, `BITRIX_CLIENT_SECRET`) и публичного HTTPS callback URL для приёма `code`.
    - Токены живут 1 час (`access_token`) и 180 дней (`refresh_token`), автоматически обновляются нашим сервером (`refreshPortalAuth` в `bitrix.go:200-317`).
    - Идеально подходит для **multi-user / enterprise** сценариев, когда MCP-сервер развёрнут централизованно.
  - **Входящий вебхук (`https://<portal>/rest/<user_id>/<webhook_secret>/`):**
    - Создаётся администратором/пользователем портала в 2 клика без настройки `client_id`, `client_secret` и публичного callback-домена.
    - Не требует обновления токенов (`refresh_token`), работает как с классическим `/rest/<user_id>/<code>/<method>`, так и с REST 3.0 `/rest/<user_id>/<code>/api/<method>`.
    - Лимиты REST API у вебхуков и OAuth-приложений **одинаковые** ([`/limits.html`](https://apidocs.bitrix24.ru/limits.html)).
    - **Вывод для `bitrix24-mcp-server`:** Добавление поддержки входящего вебхука (например, через переменную окружения `BITRIX_WEBHOOK_URL` или добавление webhook-соединения в веб-интерфейсе `/admin`) кардинально снизит порог входа для разработчиков и небольших команд, использующих режим `-stdio` (`cmd/b24/main.go:57`), при полном сохранении текущей OAuth 2.0 архитектуры для продакшена.

---

## 4. Приоритизированный план доработок (`План доработок`)

Ниже приведён пошаговый архитектурный план модернизации `bitrix24-mcp-server` для перехода на **Bitrix24 REST 3.0** с сохранением обратной совместимости и гибридной маршрутизации.

### Фаза 1. Модернизация транспортного ядра (`internal/bitrix/rest_client.go`, `internal/bitrix/bitrix.go`) — Приоритет P0

1. **Поддержка эндпоинтов `/rest/api/` и `/rest/` в `RestClient` ([`internal/bitrix/rest_client.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/rest_client.go#L120-L248)):**
   - Разделить вызовы или добавить маршрутизацию по версии API (`APIVersionV1` -> `/rest/%s.json`, `APIVersionV3` -> `/rest/api/%s`), например, метод `CallV3(ctx, endpoint, accessToken, method, params, opts...)`.
   - Добавить поддержку опциональных HTTP-заголовков запроса (в частности, `Idempotency-Key` для мутаций REST 3.0).
2. **Полиморфный парсер ошибок и метаданных `time` ([`internal/bitrix/rest_client.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/rest_client.go#L20-L50)):**
   - Заменить `Error string` в `BitrixResponse` на кастомный тип `FlexBitrixError`, который в `UnmarshalJSON` проверяет первый байт:
     - если `"..."` (строка) — читает legacy-ошибку и `error_description`;
     - если `{...}` (объект) — десериализует структуру REST 3.0 `{Code string, Message string, Validation []ValidationError}`.
   - Пробрасывать `Validation []ValidationError` в `BitrixAPIError.Error()`, чтобы LLM в ответе MCP-инструмента видела точное имя поля и причину ошибки валидации (`validation[].field`, `validation[].message`) и могла самостоятельно исправить запрос.
   - Добавить поле `Time *BitrixTime` (`duration`, `operating`, `operating_reset_at`) в `BitrixResponse`. Учитывать `operating_reset_at` при обработке HTTP 429 `OPERATION_TIME_LIMIT`.
3. **Поддержка нативного JSON-батча `/rest/api/batch` ([`internal/bitrix/rest_client.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/rest_client.go#L260-L332)):**
   - Добавить метод `BatchCallV3`, отправляющий `POST /rest/api/batch` с массивом `[{"method": "...", "query": {...}}]` (и передачей токена авторизации) без URL-кодирования `encodeJSONToQuery`.
4. **Актуализация OAuth-сервера и поддержка вебхуков ([`internal/bitrix/bitrix.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/bitrix.go#L520), [`internal/config/config.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/config/config.go)):**
   - Обновить дефолтный `OAuthTokenURL` на `https://oauth.bitrix24.tech/oauth/token/` (с возможностью переопределения через `BITRIX_OAUTH_TOKEN_URL`).
   - Опционально поддержать подключение по входящему вебхуку (`AuthMode: "oauth" | "webhook"`), при котором `accessToken` не добавляется в тело запроса, а базовый URL уже включает `/rest/<user_id>/<webhook_code>/` (для v1) и `/rest/<user_id>/<webhook_code>/api/` (для v3).

---

### Фаза 2. Маппинг сущностей CRM и переход чтения/поиска на REST 3.0 (`internal/bitrix/crm.go`) — Приоритет P0

1. **Резолвер кода сущности REST 3.0 (`ToRestV3EntityType` в [`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go#L19-L46)):**
   - Добавить функцию преобразования `entityTypeId int -> (string, error)`:
     - `1 -> "lead"`
     - `2 -> "deal"`
     - `3 -> "contact"`
     - `4 -> "company"`
     - `7 -> "quote"`
     - `31 -> "smartinvoice"`
     - `id >= 128 -> fmt.Sprintf("smartprocess%d", id)`
   - Добавить `7` (`Quote` / Коммерческое предложение) в `BuiltInTypes` в [`internal/bitrix/schema_discovery.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/schema_discovery.go#L23-L29).
2. **Умное формирование `select` и реляционных полей ([`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go#L92-L95,L170-L233)):**
   - Убрать подстановку `[]string{"*"}` (запрещена в REST 3.0).
   - Если `params.Select` пуст (или содержит `"*"`), формировать дефолтный список ключевых полей для данного `entityTypeId` (или брать список полей из кэша `SchemaSource`) + автоматически добавлять реляционные поля REST 3.0 (`"stage.name"`, `"category.name"`, `"assignedBy.firstName"`, `"assignedBy.lastName"`, `"company.title"`, `"contact.name"` для тех сущностей, где эти связи существуют).
   - Всегда гарантировать наличие `"id"` в массиве `select`.
3. **Адаптер фильтрации (`Filter` в [`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go#L68-L167)):**
   - Поддержать в `ListItems` нативный формат фильтра REST 3.0 (массив условий `[["field", "op", value]]` и логических групп `{"type": "filter", "logic": "or", ...}`).
   - Для обратной совместимости написать конвертер `convertLegacyFilterToV3(map[string]any) ([]any, bool)`, который автоматически переводит простые map-фильтры вида `{"=stageId": "NEW", ">=opportunity": 1000, "@id": [1,2]}` в `[["stageId", "=", "NEW"], ["opportunity", ">=", 1000], ["id", "in", [1,2]]]`. Если в фильтре встретился неподдерживаемый в v3 оператор `%` (подстрока), прозрачно выполнять fallback-вызов к `/rest/crm.item.list`.
4. **Быстрая пагинация `ListItems` и подсчёт `total` через `aggregate` ([`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go#L78-L168)):**
   - Перевести `ListItems` на `/rest/api/crm.<type>.list` с параметром `pagination: {"limit": limit, "page": 1}` (до 1000 записей за 1 HTTP-запрос вместо `CollectPages` из 20 батчей).
   - При необходимости получения точного `total` вызывать `/rest/api/crm.<type>.aggregate` с `{"select": {"count": ["id"]}, "filter": ...}` (можно объединять `list` + `aggregate` в один `/rest/api/batch` из 2 элементов).
5. **Оптимизация `GetItem` и `GetItems` ([`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go#L170-L233)):**
   - `GetItem`: вызывать `/rest/api/crm.<type>.get` с обязательным явным `select`.
   - `GetItems`: вместо конструирования до 250 вызовов `crm.item.get` в батче, выполнять **один вызов** `/rest/api/crm.<type>.list` с `filter: [["id", "in", itemIDs]]` и `pagination: {"limit": len(itemIDs), "page": 1}` (позволяет поднять лимит `MaxGetItemsBatch` с 250 до 1000 за 1 запрос!).
6. **Перевод `ListProductRows` на REST 3.0 ([`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go#L311-L365)):**
   - Вызывать `/rest/api/crm.<type>.productrow.list` с `"filter": [["ownerId", "=", itemID]]`, явным полным списком полей в `"select"` и `"pagination": {"limit": 1000, "page": 1}`, разбирая `result.items`.

---

### Фаза 3. Перевод мутаций и `OperationService` на REST 3.0 с `Idempotency-Key` (`internal/bitrix/operation.go`) — Приоритет P1

1. **Использование типизированных методов записи в `OperationService` ([`internal/bitrix/operation.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/operation.go#L34-L157)):**
   - `PrepareCreate` (`OpCreateItem`): формировать метод `crm.<type>.add` с `{"fields": fields}`. Ответ в REST 3.0 имеет формат `{"result": {"id": 9023}}`.
   - `PrepareUpdate` (`OpUpdateItem`) и `PrepareMove` (`OpMoveItemStage`): формировать метод `crm.<type>.update` с `{"id": id, "fields": fields}`. Ответ в REST 3.0 имеет формат `{"result": {"result": true}}`.
2. **Сквозная идемпотентность через `Idempotency-Key`:**
   - При вызове `c.client.Call` внутри `Confirm` ([`internal/bitrix/operation.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/operation.go#L123)) передавать HTTP-заголовок `Idempotency-Key: claimed.ID`. Это полностью исключит дублирование сущностей в Битрикс24 при сетевых ретраях (ответ кэшируется на стороне Битрикс24 на 24 часа и при повторе возвращает `Idempotent-Replayed: true`).

---

### Фаза 4. Гибридное обнаружение схемы портала (`internal/bitrix/schema_discovery.go`) — Приоритет P1

1. **Поля сущностей (`crm.<type>.field.list`):**
   - Перевести загрузку полей на `/rest/api/crm.<type>.field.list` (разбор массива `result.items` с атрибутами `name`, `type`, `elementType`, `multiple`, `title`, `filterable`, `sortable`, `editable`, `requiredGroups`, `validationRules`).
   - При необходимости получения вариантов значений (`enum`) пользовательских полей догружать их через `/rest/api/crm.<type>.field.custom.list`.
2. **Воронки и стадии (гибридная маршрутизация согласно [`migration.html`](https://apidocs.bitrix24.ru/api-reference/crm/migration.html)):**
   - Смарт-процессы: оставить `/rest/crm.type.list`.
   - Воронки (`categories`): для `deal`, `contact`, `company`, `smartprocess<id>` использовать `/rest/api/crm.<type>.category.list`; для `smartinvoice` (`31`) оставить `/rest/crm.category.list`.
   - Стадии (`stages`): для `deal` и `smartprocess<id>` использовать `/rest/api/crm.<type>.category.stage.list` с плоским фильтром `"filter": ["categoryId", "=", categoryID]`; для `lead` (`1`), `quote` (`7`) и `smartinvoice` (`31`) оставить `/rest/crm.status.list`.

---

### Фаза 5. Развитие MCP-инструментов и DX разработчика (`internal/mcp/server.go`, `.mcp.json`) — Приоритет P2

1. **Новый MCP-инструмент `aggregate_crm` (`internal/mcp/server.go`):**
   - Обернуть метод REST 3.0 [`crm.<type>.aggregate`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-aggregate-rest-v3.html) в отдельный read-only инструмент MCP `aggregate_crm` (параметры: `connection_id`, `entity_type_id`, `select` с агрегатными функциями `count`/`sum`/`avg`/`min`/`max`, `filter`). Это позволит AI-ассистенту мгновенно отвечать на аналитические вопросы («Сколько сделок в работе и на какую сумму?») без перегрузки контекста тысячами карточек.
2. **Обновление описаний (prompts/schemas) инструментов в [`internal/mcp/server.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/mcp/server.go#L130-L240):**
   - Обновить документацию параметров `select` и `filter` в `list_items` и `get_item`:
     - явно указать поддержку реляционных полей через точку (`stage.name`, `category.name`, `assignedBy.lastName`, `company.title`);
     - описать формат `filter` REST 3.0 (`[["field", "=", value]]`), строгую типизацию `bool` (`true`/`false`), обязательный часовой пояс в `date-time` и верхний регистр пользовательских полей `UF_CRM_*`.
3. **Подключение официального `b24-dev-mcp` в репозиторий:**
   - Добавить конфигурацию сервера `b24-dev-mcp` (`https://mcp-dev.bitrix24.tech/mcp`) в `.mcp.json` проекта согласно [`https://apidocs.bitrix24.ru/ai-tools/mcp.html`](https://apidocs.bitrix24.ru/ai-tools/mcp.html) для ускорения разработки и проверки методов REST 3.0.

---

## 5. Реестр использованных первоисточников

### Официальная документация Bitrix24 (`https://apidocs.bitrix24.ru`)
1. **Официальный MCP-сервер документации (`b24-dev-mcp`):** [`https://apidocs.bitrix24.ru/ai-tools/mcp.html`](https://apidocs.bitrix24.ru/ai-tools/mcp.html)
2. **Спецификация Bitrix24 REST 3.0 (`/rest/api/`, `select`, `filter`, `batch`, `Idempotency-Key`, формат ошибок):** [`https://apidocs.bitrix24.ru/api-reference/rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/rest-v3.html)
3. **Руководство по миграции CRM со старого `/rest/` (`crm.item.*`) на REST 3.0 (`/rest/api/`):** [`https://apidocs.bitrix24.ru/api-reference/crm/migration.html`](https://apidocs.bitrix24.ru/api-reference/crm/migration.html)
4. **Обзор раздела CRM в REST 3.0 и таблица типов объектов `<type>`:** [`https://apidocs.bitrix24.ru/api-reference/crm/index.html`](https://apidocs.bitrix24.ru/api-reference/crm/index.html)
5. **Получение списка элементов `crm.<type>.list` (пагинация до 1000, запрет `*`, реляционный `select`):** [`https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-list-rest-v3.html)
6. **Агрегация данных `crm.<type>.aggregate` (`count`, `sum`, `avg`, `min`, `max`):** [`https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-aggregate-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-aggregate-rest-v3.html)
7. **Получение элемента `crm.<type>.get`:** [`https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-get-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-get-rest-v3.html)
8. **Создание элемента `crm.<type>.add`:** [`https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-add-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-add-rest-v3.html)
9. **Обновление элемента `crm.<type>.update`:** [`https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-update-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/items/crm-entity-update-rest-v3.html)
10. **Поля сущностей `crm.<type>.field.list`:** [`https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/fields/crm-entity-field-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/fields/crm-entity-field-list-rest-v3.html)
11. **Воронки и стадии в REST 3.0 (обзор и ограничения):** [`https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/categories/index.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/categories/index.html)
12. **Список воронок `crm.<type>.category.list`:** [`https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/categories/crm-entity-category-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/categories/crm-entity-category-list-rest-v3.html)
13. **Список стадий `crm.<type>.category.stage.list`:** [`https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/categories/crm-entity-category-stage-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/categories/crm-entity-category-stage-list-rest-v3.html)
14. **Товарные позиции `crm.<type>.productrow.list`:** [`https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/product-rows/crm-entity-product-row-list-rest-v3.html`](https://apidocs.bitrix24.ru/api-reference/crm/rest-v3/product-rows/crm-entity-product-row-list-rest-v3.html)
15. **Лимиты REST API (`QUERY_LIMIT_EXCEEDED`, `OPERATION_TIME_LIMIT`, `operating`, `operating_reset_at`):** [`https://apidocs.bitrix24.ru/limits.html`](https://apidocs.bitrix24.ru/limits.html)
16. **Протокол OAuth 2.0 в Битрикс24 (`oauth.bitrix24.tech`):** [`https://apidocs.bitrix24.ru/settings/oauth/index.html`](https://apidocs.bitrix24.ru/settings/oauth/index.html)
17. **Входящие вебхуки:** [`https://apidocs.bitrix24.ru/local-integrations/local-webhooks.html`](https://apidocs.bitrix24.ru/local-integrations/local-webhooks.html)

### Файлы кодовой базы `bitrix24-mcp-server`
- [`cmd/b24/main.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/cmd/b24/main.go) — точка входа, инициализация сервисов и режимов транспорта (`http` / `stdio`).
- [`internal/config/config.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/config/config.go) — конфигурация приложения и переменных окружения.
- [`internal/bitrix/rest_client.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/rest_client.go) — низкоуровневый HTTP-клиент к Битрикс24 (`Call`, `BatchCall`, `CollectPages`, обработка ошибок и ретраев).
- [`internal/bitrix/bitrix.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/bitrix.go) — управление подключениями порталов, обмен и обновление OAuth-токенов (`oauth.bitrix.info`).
- [`internal/bitrix/crm.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/crm.go) — чтение и поиск сущностей CRM (`ListItems`, `GetItem`, `GetItems`, `SearchCRM`, `ListProductRows`, `GetUsers`).
- [`internal/bitrix/schema_discovery.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/schema_discovery.go) — обнаружение и кэширование схемы портала (`BuiltInTypes`, смарт-процессы, поля, воронки, стадии).
- [`internal/bitrix/operation.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/operation.go) — двухшаговые операции записи (`PrepareCreateItem`, `PrepareUpdateItem`, `PrepareDeleteItem`, `Confirm`, `Cancel`).
- [`internal/bitrix/policy.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/bitrix/policy.go) — политики доступа соединений по типам сущностей и действиям.
- [`internal/mcp/server.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/mcp/server.go) — регистрация и обработчики 15 инструментов MCP.
- [`internal/mcp/oauth.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/mcp/oauth.go) — сервер авторизации OAuth 2.1 + PKCE для MCP-клиентов.
- [`internal/server/server.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/server/server.go) — HTTP-сервер, админ-панель, OAuth callbacks и Streamable HTTP `/mcp`.
- [`internal/store/store.go`](file:///Users/serg/PhpstormProjects/bitrix24-mcp-server/internal/store/store.go) — SQLite-хранилище с AES-256-GCM шифрованием токенов.
