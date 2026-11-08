---
name: bitrix24-crm
description: Operate Bitrix24 CRM and Smart Processes (SPA) through the bitrix24 MCP server. Use when the user asks to find, inspect, create, update, move stages, or delete CRM items (Leads, Deals, Contacts, Companies, Quotes, Smart Invoices, or dynamic SPAs).
---

# Bitrix24 CRM Operations (`bitrix24-crm`)

Follow this workflow whenever interacting with a live Bitrix24 portal via the `bitrix_*` MCP tools.

---

## 1. Entity Type ID Reference (`entityTypeId`)

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

## 2. Discovery-First Rule (Never Guess IDs or Codes)

1. **Resolve `connectionId`**:
   - If `connectionId` is not yet known in the conversation, call `bitrix_list_connections` first.
2. **Inspect schema before filtering by funnel, stage, or custom fields**:
   - Call `bitrix_describe_entity` for the target `entityTypeId`. In a single call it returns:
     - `fields` (including custom `UF_CRM_*` fields and their labels/enum values),
     - `categories` (funnels),
     - `stages` (grouped by category).
   - **Never filter by human-readable stage names** (e.g. `"В работе"`, `"Новая"`, `"Успешно"`). Always look up the exact `STATUS_ID` (e.g. `"NEW"`, `"PREPARATION"`, `"C3:PREPAYMENT_INVOICE"`, `"WON"`) via `bitrix_describe_entity` or `bitrix_list_stages`.
   - **Category-less entities (`1`, `3`, `4`, `7`)**: do not pass `categoryId` when querying Leads, Contacts, Companies, or Quotes.

---

## 3. Fast Reads Without N+1 Queries (REST 3.0 Read Engine)

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

### Custom Fields (`UF_CRM_*`)
- Always include required `UF_CRM_*` codes explicitly in `select` when reading or searching (wildcard `*` is forbidden in REST 3.0).
- Both `UF_CRM_12345` and camelCase `ufCrm12345` are accepted and automatically normalized to `UF_CRM_*`.

### Bulk Fetch by IDs
- When fetching multiple known records, call `bitrix_get_item` once with `ids: [101, 102, 103]` (supports up to 1,000 IDs in REST 3.0 and preserves the requested ID order) instead of calling `bitrix_get_item` in a loop.

---

## 4. Filter Syntax Reference (`bitrix_search_items` & `bitrix_count_items`)

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

## 5. Two-Step Mutation Protocol (Strict Human-in-the-Loop)

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
