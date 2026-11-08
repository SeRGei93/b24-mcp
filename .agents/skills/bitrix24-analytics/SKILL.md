---
name: bitrix24-analytics
description: Analyze Bitrix24 CRM pipelines, stage conversion rates, manager performance, product rows, and pipeline hygiene using the bitrix24 MCP server. Use when the user asks for reports, funnel statistics, revenue summaries, stalled deals, or CRM audits.
---

# Bitrix24 CRM Analytics & Pipeline Auditing (`bitrix24-analytics`)

Use this skill when building analytical reports, funnel breakdowns, manager scorecards, or CRM hygiene audits via the `bitrix_*` MCP tools.

---

## 1. Golden Rules for Fast CRM Analytics

1. **Use `bitrix_count_items` for Pure Counts**:
   - Whenever you need item counts (total deals in a funnel, count per stage, count per manager, count created this month), call **`bitrix_count_items`**. It uses Bitrix24 REST 3.0 `crm.<slug>.aggregate` and returns the exact count in a single lightweight request without transferring item payloads.
2. **Fetch Up to 1,000 Records per Call for Revenue/Sum Aggregations**:
   - When summing `opportunity` (deal amount) or grouping by multiple dimensions in memory, call `bitrix_search_items` with `limit: 1000` and a minimal `select` list containing only the fields required for the report.
3. **Include Relational Names in `select`**:
   - Request `stage.name`, `category.name`, `company.title`, `assignedBy.firstName`, `assignedBy.lastName` directly in `bitrix_search_items` so you can render human-readable tables without extra lookup calls.

---

## 2. Playbook 1: Sales Funnel & Stage Conversion Report

When the user asks for a funnel breakdown or conversion report:

1. **Discover Funnel & Stages**:
   - Call `bitrix_describe_entity(entityTypeId=2)` (or the target SPA `entityTypeId`).
   - Identify the target funnel (`categoryId`) and order its stages by `SORT`.
   - Classify stages into three groups:
     - **Active / In-Progress** (stages before `WON`),
     - **Won** (`STATUS_ID` ending in `WON` or `SEMANTICS = "S"`),
     - **Lost** (`STATUS_ID` ending in `LOSE` / `APOLOGY` or `SEMANTICS = "F"`).
2. **Aggregate Counts & Amounts**:
   - If only counts are needed: call `bitrix_count_items` per stage or fetch the funnel items with `bitrix_search_items(entityTypeId=2, filter=[["categoryId", "=", <id>]], select=["id", "stageId", "stage.name", "opportunity", "currencyId"], limit=1000)`.
3. **Present Structured Output**:
   - Render a Markdown table with columns: **Stage**, **Code (`STATUS_ID`)**, **Deals Count**, **Share of Funnel (%)**, and **Total Amount (`opportunity`)**.
   - Highlight conversion rate (`Won / (Won + Lost)`) and the stage with the largest bottleneck.

---

## 3. Playbook 2: Manager Performance Scorecard

When evaluating sales reps or workload distribution:

1. **Fetch Active & Closed Deals for the Period**:
   - Filter by date range if requested (e.g. `[[ "createdTime", ">=", "2026-01-01T00:00:00" ]]` or `[[ "closedate", ">=", "2026-01-01" ]]`).
   - Use `select`:
     ```json
     ["id", "title", "stageId", "stage.name", "opportunity", "currencyId", "assignedById", "assignedBy.firstName", "assignedBy.lastName", "createdTime", "closedate"]
     ```
2. **Compute Per-Manager Metrics**:
   - **Open Pipeline**: count and total `opportunity` of active deals.
   - **Won Deals**: count and revenue of won deals.
   - **Lost Deals**: count and lost amount, plus win rate (`Won / Closed`).
   - **Average Deal Size**: `Won Revenue / Won Count`.

---

## 4. Playbook 3: Product & Line-Item Revenue Analysis

When analyzing which products or services are being sold inside deals:

1. **Find Target Deals**:
   - Call `bitrix_search_items` for the relevant funnel/stage/period and collect the deal `id`s.
2. **Batch-Fetch Product Rows**:
   - Call `bitrix_list_product_rows` passing `dealIds: [...]` (up to 50 owner IDs per call, or `dealId` for a single deal).
   - Aggregate by `productName` / `productId`: total quantity sold, total gross revenue (`price * quantity`), and average discount (`discountSum`).

---

## 5. Playbook 4: CRM Hygiene & Stalled Deals Audit

When asked to find anomalies, stalled deals, or clean up the pipeline:

1. **Stalled Deals (No Activity / Old `updatedTime`)**:
   - Query active stages (`!=STAGE_ID` for `WON`/`LOSE`) with `["updatedTime", "<=", "<ISO-date-N-days-ago>"]` and order by `{"updatedTime": "ASC"}`.
2. **Incomplete Deal Cards**:
   - Identify active deals with `opportunity == 0`, missing `companyId`/`contactId`, or expired `closedate`.
3. **Safe Bulk Remediation**:
   - Present the list of problematic records in a table first.
   - If the user asks to move or update them, prepare changes via `bitrix_prepare_move_item_stage` / `bitrix_prepare_update_item`, present the `operation_id`s, and wait for explicit user approval before calling `bitrix_confirm_operation`.
