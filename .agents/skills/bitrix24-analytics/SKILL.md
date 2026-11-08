---
name: bitrix24-analytics
description: >
  Analyze Bitrix24 (Битрикс24) CRM pipelines, stage conversion rates, manager performance,
  UTM/marketing attribution, product rows, and pipeline hygiene using the bitrix24 MCP server.
  Triggers on: "воронка продаж", "конверсия", "отчёт по сделкам", "эффективность менеджеров",
  "отчёт по отделу", "аналитика CRM", "зависшие сделки", "брошенные сделки", "итоги недели",
  "итоги месяца", "откуда лиды", "UTM-метки", "источники трафика", "товарная аналитика",
  "аудит CRM", "sales funnel", "pipeline report", "manager scorecard", "stalled deals", "UTM report".
---

# Bitrix24 CRM Analytics & Pipeline Auditing (`bitrix24-analytics`)

Use this skill when building analytical reports, funnel breakdowns, manager scorecards, marketing attribution summaries, or CRM hygiene audits via the `bitrix_*` MCP tools.

---

## 1. Golden Rules for Fast & Accurate CRM Analytics

1. **Use `bitrix_count_items` for Pure Counts**:
   - Whenever you need item counts (total deals in a funnel, count per stage, count per manager, count created this month), call **`bitrix_count_items`**. It uses Bitrix24 REST 3.0 `crm.<slug>.aggregate` and returns the exact count in a single lightweight request without transferring item payloads.
2. **Fetch Up to 1,000 Records per Call for Revenue/Sum Aggregations**:
   - When summing `opportunity` (deal amount) or grouping by multiple dimensions in memory, call `bitrix_search_items` with `limit: 1000` and a minimal `select` list containing only the fields required for the report.
3. **Include Relational Names in `select`**:
   - Request `stage.name`, `category.name`, `company.title`, `assignedBy.firstName`, `assignedBy.lastName` directly in `bitrix_search_items` so you can render human-readable tables without extra lookup calls.
4. **Data Hygiene Rules (Prevent Distorted Reports)**:
   - **Filter or flag test records**: Deals or leads titled `"тест"`, `"test"`, `"проверка"` can heavily skew conversion rates and revenue. Identify and exclude them from business totals (or list them separately).
   - **State the date anchor explicitly**: Always specify in your report whether metrics are grouped by **creation date** (`createdTime` / `DATE_CREATE` — cohort view) or **close date** (`closedate` / `CLOSEDATE` — cashflow/realization view).
   - **Respect Stage Semantics (`SEMANTICS` / `STAGE_SEMANTIC_ID`)**:
     - `P` (Process / Active): deal/lead is currently in work.
     - `S` (Success / `WON` / `CONVERTED`): successfully closed.
     - `F` (Failure / `LOSE` / `APOLOGY` / `JUNK`): lost or disqualified.
   - **Preserve Portal Timezone**: Bitrix24 returns ISO 8601 timestamps in the portal's timezone (e.g. `+03:00`). Include timezone offsets when filtering by date boundaries.

---

## 2. Playbook 1: Sales Funnel & Stage Conversion Report ("воронка продаж")

When the user asks for a funnel breakdown or conversion report:

1. **Discover Funnel & Stages**:
   - Call `bitrix_describe_entity(entityTypeId=2)` (or the target SPA `entityTypeId`).
   - Identify the target funnel (`categoryId`; default Deal funnel is `0`) and order its stages by `SORT`.
   - Classify stages into **Active (`P`)**, **Won (`S`)**, and **Lost (`F`)**.
2. **Aggregate Counts & Amounts**:
   - If only counts are needed: call `bitrix_count_items` per stage or fetch the funnel items with `bitrix_search_items(entityTypeId=2, filter=[["categoryId", "=", <id>]], select=["id", "title", "stageId", "stage.name", "opportunity", "currencyId"], limit=1000)`.
3. **Present Structured Output**:
   - Render a Markdown table with columns: **Stage**, **Code (`STATUS_ID`)**, **Deals Count**, **Share of Funnel (%)**, and **Total Amount (`opportunity`)**.
   - Highlight:
     - **Win Rate (`Won / (Won + Lost)`)** by count and by revenue,
     - **Active Pipeline Value** (sum of `P` stages),
     - **Biggest Bottleneck** (active stage holding the most deals or highest stuck value).

---

## 3. Playbook 2: Manager Performance Scorecard ("эффективность менеджеров")

When evaluating sales reps or workload distribution:

1. **Fetch Active & Closed Deals for the Period**:
   - Filter by date range if requested (e.g. `[["createdTime", ">=", "2026-01-01T00:00:00"]]` or `[["closedate", ">=", "2026-01-01"]]`).
   - Use `select`:
     ```json
     ["id", "title", "stageId", "stage.name", "opportunity", "currencyId", "assignedById", "assignedBy.firstName", "assignedBy.lastName", "createdTime", "updatedTime", "closedate"]
     ```
2. **Compute Per-Manager Metrics**:
   - **Open Pipeline**: count and total `opportunity` of active deals (`P`).
   - **Stalled Deals (`💤`)**: count of active deals with no updates for 14+ days.
   - **Won Deals (`S`)**: count and revenue of won deals.
   - **Lost Deals (`F`)**: count and lost amount, plus **Win Rate (`Won / (Won + Lost)`)**.
   - **Average Deal Size (Средний чек)**: `Won Revenue / Won Count`.

---

## 4. Playbook 3: Marketing & UTM Attribution Report ("откуда лиды / отчёт по UTM")

When analyzing lead/deal sources and advertising effectiveness:

1. **Fetch Records with Explicit Attribution Fields**:
   - `UTM_*` fields are never returned unless explicitly listed in `select`. Request:
     ```json
     ["id", "title", "stageId", "stage.name", "opportunity", "currencyId", "SOURCE_ID", "SOURCE_DESCRIPTION", "UTM_SOURCE", "UTM_MEDIUM", "UTM_CAMPAIGN", "UTM_CONTENT", "UTM_TERM", "createdTime"]
     ```
2. **Group & Attribute**:
   - Group records by `UTM_SOURCE` -> `UTM_CAMPAIGN` (and fallback to `SOURCE_ID` when `UTM_SOURCE` is empty; label records with neither as `Не размечено (unknown)` rather than "organic").
   - Compute per channel/campaign: **Total Inquiries**, **Active in Work**, **Won (`WON` / `CONVERTED`)**, **Conversion Rate (%)**, and **Won Revenue**.

---

## 5. Playbook 4: Product & Line-Item Revenue Analysis ("товарная аналитика")

When analyzing which products or services are being sold inside deals:

1. **Find Target Deals**:
   - Call `bitrix_search_items` for the relevant funnel/stage/period and collect the deal `id`s.
2. **Batch-Fetch Product Rows**:
   - Call `bitrix_list_product_rows` passing `dealIds: [...]` (up to 50 owner IDs per call, or `dealId` for a single deal).
   - Aggregate by `productName` / `productId`: total quantity sold, total gross revenue (`price * quantity`), and average discount (`discountSum`).

---

## 6. Playbook 5: Executive Weekly / Monthly Summary & CRM Hygiene Audit ("итоги недели / зависшие сделки")

When asked for a management summary or pipeline health check:

1. **Key Period Totals**:
   - New deals/leads created in the period (`bitrix_count_items` with `>=createdTime`).
   - Won deals count & revenue in the period.
   - Lost deals count & main loss stages.
2. **Pipeline Hygiene Anomalies**:
   - **💤 Stalled Deals**: active deals (`!=STAGE_ID` for `WON`/`LOSE`) with `updatedTime <= <14 days ago>`, sorted by `opportunity` descending (highlight top 5–10 most valuable stuck deals).
   - **⚠️ Incomplete Cards**: active deals with `opportunity == 0`, missing `companyId`/`contactId`, or expired `closedate`.
3. **Safe Bulk Remediation**:
   - Present the list of problematic records in a table first.
   - If the user asks to move or update them, prepare changes via `bitrix_prepare_move_item_stage` / `bitrix_prepare_update_item`, present the `operation_id`s, and wait for explicit user approval before calling `bitrix_confirm_operation`.
