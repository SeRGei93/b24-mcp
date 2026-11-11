---
name: bitrix24-crm
description: >
  Operate Bitrix24 (Битрикс24) CRM and Smart Processes (SPA) through the bitrix24 MCP server.
  Triggers on: "сделки", "лиды", "контакты", "компании", "предложения", "счета", "смарт-процесс",
  "карточка клиента", "досье на клиента", "расскажи про клиента", "найди сделку", "создай сделку",
  "обнови сделку", "переведи стадию", "удали сделку", "товары в сделке", "deals", "leads",
  "contacts", "companies", "quotes", "smart invoices", "smart process", "client dossier",
  "move stage", "update CRM".
---

# Bitrix24 CRM Operations (`bitrix24-crm`)

Follow this workflow whenever interacting with a live Bitrix24 portal via the `bitrix_*` MCP tools.

---

## 1. Core UX & Execution Rules

1. **Read Requests — Execute Immediately**:
   - When the user asks to show, find, list, or check CRM records, execute the read tool calls immediately without asking permission first.
   - Present results as clean Markdown tables or structured bullet lists using business terms (Сделка, Стадия, Сумма, Ответственный, Компания), never raw JSON dumps.
2. **Proactive Insights on Every Read**:
   - Automatically highlight anomalies when displaying deals or leads:
     - `💤 Без движения >14 дней` — active items whose `updatedTime` is older than 14 days.
     - `⚠️ Сумма не указана (0)` — active deals with `opportunity == 0`.
     - `⏰ Просрочена дата закрытия` — active deals whose `closedate` is in the past.
3. **Write Requests — Strict Two-Step Confirmation**:
   - Never execute a mutation without preparing it first (`bitrix_prepare_*`), showing a concise human-readable preview, and receiving explicit user approval before calling `bitrix_confirm_operation`.

---

## 2. Entity Type ID Reference (`entityTypeId`)

Always pass the numeric `entityTypeId` to CRM tools:

| `entityTypeId` | Entity | REST 3.0 Slug | Categories (Funnels) | Stages |
| :--- | :--- | :--- | :--- | :--- |
| `1` | **Lead** (Лид) | `lead` | No | Yes (`STATUS_ID`) |
| `2` | **Deal** (Сделка) | `deal` | Yes (`CATEGORY_ID`) | Yes (`STAGE_ID`) |
| `3` | **Contact** (Контакт) | `contact` | No | No |
| `4` | **Company** (Компания) | `company` | No | No |
| `7` | **Quote** (Предложение) | `quote` | No | Yes (`STATUS_ID`) |
| `31` | **Smart Invoice** (Счёт) | `smartinvoice` | Yes (`CATEGORY_ID`) | Yes (`STAGE_ID`) |
| `>= 128` | **Smart Process / SPA** (Смарт-процесс) | `smartprocess<id>` | Yes (`categoryId`) | Yes (`stageId`) |

If the user mentions a custom Smart Process by name and you do not know its `entityTypeId`, call `bitrix_list_crm_entities` (or `bitrix_discover_portal`) first.

---

## 3. Discovery-First Rule (Never Guess IDs or Codes)

1. **Resolve `connectionId`**:
   - If `connectionId` is not yet known in the conversation, call `bitrix_list_connections` first.
2. **Inspect schema before filtering by funnel, stage, or custom fields**:
   - Call `bitrix_describe_entity` for the target `entityTypeId`. In a single call it returns:
     - `fields` (including custom `UF_CRM_*` fields and their labels/enum values),
     - `categories` (funnels; note that the default Deal funnel has `id: 0`),
     - `stages` (grouped by category, with `SORT` and `SEMANTICS`).
   - **Never filter by human-readable stage names** (e.g. `"В работе"`, `"Новая"`, `"Успешно"`). Always look up the exact `STATUS_ID` (e.g. `"NEW"`, `"PREPARATION"`, `"C3:PREPAYMENT_INVOICE"`, `"WON"`) via `bitrix_describe_entity` or `bitrix_list_stages`.
   - **Category-less entities (`1`, `3`, `4`, `7`)**: do not pass `categoryId` when querying Leads, Contacts, Companies, or Quotes.

---

## 4. Fast Reads Without N+1 Queries (REST 3.0 Read Engine)

The MCP server uses Bitrix24 REST 3.0 by default, which supports **relational dot-notation fields** in `select` and up to **1,000 items per request**.

### Use Relational `select` Fields
Instead of making extra `bitrix_get_item` or `bitrix_list_users` calls to resolve company names, contact names, stage names, or responsible user names, request them directly in `select`:

- **Deals (`2`)**:
  ```json
  ["id", "title", "stageId", "stage.name", "categoryId", "category.name", "opportunity", "currencyId", "companyId", "company.title", "contactId", "contact.name", "contact.lastName", "assignedById", "assignedBy.firstName", "assignedBy.lastName", "begindate", "closedate", "createdTime", "updatedTime"]
  ```
- **Leads (`1`)**:
  ```json
  ["id", "title", "name", "lastName", "statusId", "stage.name", "opportunity", "currencyId", "assignedById", "assignedBy.firstName", "assignedBy.lastName", "createdTime"]
  ```
- **Quotes (`7`) & Smart Invoices (`31`)**:
  ```json
  ["id", "title", "stageId", "stage.name", "opportunity", "currencyId", "companyId", "company.title", "contactId", "contact.name", "assignedById", "assignedBy.firstName", "assignedBy.lastName"]
  ```

### Custom Fields (`UF_CRM_*`), Multi-Fields & UTM
- Wildcard `*` is forbidden in REST 3.0. Always explicitly list required fields in `select`:
  - Custom fields: both `UF_CRM_12345` and camelCase `ufCrm12345` are accepted and normalized to `UF_CRM_*`.
  - Multi-fields on Contacts/Companies/Leads: explicitly include `PHONE`, `EMAIL`, `WEB`, `IM` in `select` when needed.
  - Marketing attribution fields: explicitly include `SOURCE_ID`, `SOURCE_DESCRIPTION`, `UTM_SOURCE`, `UTM_MEDIUM`, `UTM_CAMPAIGN`, `UTM_CONTENT`, `UTM_TERM` in `select`.

### Bulk Fetch by IDs & Complete Order Details
- When fetching multiple known records, call `bitrix_get_item` once with `ids: [101, 102, 103]` (supports up to 1,000 IDs in REST 3.0 and preserves the requested ID order) instead of calling `bitrix_get_item` in a loop.
- By default:
  - `bitrix_get_item` (`includeDetails: true`) automatically enriches order/CRM items with `productRows` (REST 3.0 `crm.<slug>.productrow.list` with fallback to `crm.item.productrow.list`), `payments` (`crm.item.payment.list` — actual payment status `paid: "Y"/"N"`, `datePaid`, `sum`, `paySystemName`), `deliveries` (`crm.item.delivery.list` — shipment status `deducted: "Y"/"N"`, `priceDelivery`, `deliveryName`), custom `ufCrm_*` fields, and linked `contact` details (`name`, `lastName`, `fm` phones/emails).
  - `bitrix_search_items` (`includeDetails: false`, `includeProductRows: false`) runs as a fast single-request REST 3.0 search (up to 1,000 items); pass `includeDetails: true` or `includeProductRows: true` explicitly when line items, payment statuses, or shipment details are needed in search results.

---

## 5. Filter Syntax Reference (`bitrix_search_items` & `bitrix_count_items`)

`filter` accepts three formats:

1. **Legacy Operator-Prefixed Map** (automatically translated to REST 3.0):
   ```json
   {
     "=CATEGORY_ID": 0,
     "@STAGE_ID": ["NEW", "PREPARATION"],
     ">=OPPORTUNITY": 100000,
     "!=ASSIGNED_BY_ID": 1
   }
   ```
   Supported prefixes: `=` (exact), `!=` / `!` (not equal), `>`, `>=`, `<`, `<=`, `@` (`in` array), `%` (substring match).

2. **REST 3.0 Expression Array**:
   ```json
   [
     ["categoryId", "=", 0],
     ["stageId", "in", ["NEW", "PREPARATION"]],
     ["opportunity", ">=", 100000]
   ]
   ```

3. **Logical `OR` / `AND` Filter Group**:
   ```json
   {
     "type": "filter",
     "logic": "or",
     "conditions": [
       ["stageId", "=", "NEW"],
       ["opportunity", ">=", 500000]
     ]
   }
   ```

4. **Substring Search (`%` / `like`)**:
   - Use `{"%TITLE": "Acme"}` or `[["title", "like", "%Acme%"]]`.
   - Note: Bitrix24 REST 3.0 does not natively support substring matching; the MCP server automatically falls back to legacy `crm.item.list` (up to 50 items per page) when `%` or `like` is present in the filter.

5. **Pagination**:
   - Pass `limit` (default `50`, max `1000`) and `page` (`1`, `2`, ...) to `bitrix_search_items`.
   - If you only need the total number of matching items, call **`bitrix_count_items`** instead of `bitrix_search_items`.

---

## 6. Ready-Made Scenario: Client / Company 360° Dossier ("расскажи про клиента X")

When the user asks for full context on a client, contact, or company:

1. **Locate the Company (`entityTypeId=4`) and/or Contact (`entityTypeId=3`)**:
   - Search with `{"%TITLE": "<name>"}` for Company or `{"%LAST_NAME": "<name>"}` / `{"%NAME": "<name>"}` for Contact.
2. **Fetch Related Deals (`entityTypeId=2`)**:
   - Call `bitrix_search_items` with `filter: [["companyId", "=", <id>]]` (or `contactId`), requesting relational `select` fields (`stage.name`, `category.name`, `opportunity`, `currencyId`, `assignedBy.firstName`, `assignedBy.lastName`, `updatedTime`, `closedate`).
3. **Fetch Line Items (`bitrix_list_product_rows`)**:
   - If deals are found, call `bitrix_list_product_rows(dealIds=[...])` in a single batch to see what products/services the client bought or is negotiating.
4. **Present the 360° Dossier**:
   - **🏢 Карточка клиента**: название, контакты, ответственный менеджер.
   - **💰 Активные сделки**: стадия, воронка, сумма, дата обновления (подсветить зависшие `💤`).
   - **✅ История закрытых сделок**: сумма выигранных (`WON`) и проигранных (`LOSE`) сделок.
   - **📦 Товары и услуги**: основные позиции из сделок.

---

## 7. Bitrix24 CRM Pitfalls & Gotchas

- **Lead Conversion (`CONVERTED`)**: When a Lead (`1`) is converted, Bitrix24 creates a Deal (`2`), Contact (`3`), and/or Company (`4`). If a lead is in status `CONVERTED`, also check Deals filtered by `LEAD_ID` to see current progress.
- **Repeat Inquiries**: Repeat messages or calls from an existing phone/email often attach directly to the existing Contact and open a new Deal rather than creating a new Lead.
- **Unknown Relational Fields in REST 3.0**: Passing a non-existent relational path in `select` (e.g. `foo.bar`) causes Bitrix24 REST 3.0 to return `UNKNOWNDTOPROPERTYEXCEPTION`. Stick to verified relational paths (`stage.name`, `category.name`, `company.title`, `contact.name`, `contact.lastName`, `assignedBy.firstName`, `assignedBy.lastName`) or check `bitrix_describe_entity`.

---

## 8. Two-Step Mutation Protocol (Strict Human-in-the-Loop)

All write and delete operations in Bitrix24 are protected by a two-step confirmation flow (`prepare` → user approval → `confirm`):

1. **Step 1 — Prepare**:
   Call the appropriate preparation tool:
   - `bitrix_prepare_create_item`
   - `bitrix_prepare_update_item`
   - `bitrix_prepare_move_item_stage`
   - `bitrix_prepare_delete_item`
2. **Step 2 — Present Preview & Wait**:
   - Show the user a clear summary of the pending operation:
     - `operation_id`
     - Action (`create`, `update`, `move_stage`, `delete`)
     - Target entity (`entityTypeId`, item `id` and title)
     - Exact fields/values or stage transition being applied
   - **STOP and wait for the user's explicit confirmation.** Never call `bitrix_confirm_operation` in the same turn as `bitrix_prepare_*` unless the user has already explicitly approved that exact `operation_id`.
3. **Step 3 — Confirm or Cancel**:
   - Once the user confirms, call `bitrix_confirm_operation(operationId=...)` (executes with `Idempotency-Key` protection).
   - If the user declines, call `bitrix_cancel_operation(operationId=...)`.
