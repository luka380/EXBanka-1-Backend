# API Route Changes — 2026-06-04 (TODO_final + options-tax, SP1–SP6)

This release (`VERSION` 1.0.0 → 1.6.0) is **fully backward-compatible**: it adds new routes, new optional query params, and new *additive* response fields. **No existing route, method, status code, auth requirement, request field, or response field was removed, renamed, or re-typed** (per the API Versioning Compatibility Requirement). Full details live in `REST_API_v3.md`; this file is the at-a-glance changelog.

## New routes

| Method | Path | Auth | Sub-project |
|---|---|---|---|
| `GET` | `/api/v3/admin/audit/business-actions` | `admin.audit.view` | SP2 — business audit log |
| `GET` | `/api/v3/me/watchlists` | AnyAuth | SP6 — named watchlists |
| `POST` | `/api/v3/me/watchlists` | AnyAuth | SP6 |
| `DELETE` | `/api/v3/me/watchlists/:watchlist_id` | AnyAuth | SP6 |
| `GET` | `/api/v3/me/watchlists/:watchlist_id/items` | AnyAuth | SP6 |
| `POST` | `/api/v3/me/watchlists/:watchlist_id/items` | AnyAuth | SP6 |
| `DELETE` | `/api/v3/me/watchlists/:watchlist_id/items/:listing_id` | AnyAuth | SP6 |

**SP2 — `GET /api/v3/admin/audit/business-actions`** — who changed an employee limit, reset a usedLimit, approved/rejected an order, changed permissions, or triggered manual tax collection. Query: `action` (`limit.set`\|`limit.used_reset`\|`order.approve`\|`order.decline`\|`permissions.set`\|`tax.collect`), `target_type` (`employee`\|`order`\|`role`\|`tax`), `actor_id`, `since`/`until` (`YYYY-MM-DD`), `page`/`page_size`. Returns `{entries:[{id, action, actor_id, target_type, target_id, detail, timestamp}], total, page, page_size}`.

**SP6 — named watchlists** — a user may keep several named lists (e.g. "tech", "forex pairs"). `POST /me/watchlists {name}` (1–64 chars, idempotent on name). `GET /me/watchlists` → `{watchlists:[{id, name, item_count, created_at}]}` (always includes the lazily-created default). Per-list item ops mirror the legacy single-list ops but are scoped to `:watchlist_id`. A list is owner-scoped (403 if not yours); the same listing may live in multiple lists.

## Changed routes (additive only — existing clients unaffected)

| Method | Path | What was added |
|---|---|---|
| `GET` | `/api/v3/investment-funds` | **New query params** `sort_by` (`name`\|`value`\|`profit`\|`annualized_return`\|`volatility`\|`reward_to_variability`\|`max_drawdown`) + `sort_order` (`asc`\|`desc`). **New response fields** per fund: `annualized_return_pct, volatility_pct, reward_to_variability, max_drawdown_pct, metrics_available, dividend_mode`. (SP3, SP4) |
| `GET` | `/api/v3/investment-funds/:id` | **New response fields**: the four metrics above + `metrics_available`, `dividend_mode`, plus `history` (this fund's daily NAV series `[{date, total_value_rsd}]`) and `average_history` (system-average series, each fund indexed to 100). (SP3, SP4) |
| `POST` | `/api/v3/investment-funds` | **New optional body field** `dividend_mode` (`payout` default \| `reinvest`). (SP4) |
| `PUT` | `/api/v3/investment-funds/:id` | **New optional body field** `dividend_mode`. (SP4) |
| `GET/POST/DELETE` | `/api/v3/me/watchlist[/:listing_id]` | **Behaviour clarified, not changed:** these legacy single-list routes now operate on the owner's lazily-created default **"My Watchlist"**. Same request/response shapes; existing clients keep working. (SP6) |

## No route changes (behaviour-only)

- **SP1 (options/premium tax)** — no REST changes; internal tax recording on the existing OTC accept/exercise sagas and the monthly `POST /api/v3/tax/collect`. New behaviour: seller premium taxed at accept; buyer taxed at exercise on `(market−strike)×qty − premium` (cost basis steps to market); buyer `−premium` loss at expiry; bank-owned (actuary-on-behalf) gains exempt (Profit Banke).
- **SP5 (notifications)** — no REST changes; new in-app/email notification *types* (`LIMIT_CHANGED`, `OTC_CONTRACT_EXPIRING_SOON`) surfaced through the existing `GET /api/v3/me/notifications`.

## New config / env vars

| Var | Default | Service |
|---|---|---|
| `FUND_SNAPSHOT_CRON_UTC` | `23:50` | stock-service (SP3) |
| `FUND_METRICS_MIN_MONTHLY_RETURNS` | `2` | stock-service (SP3) |
| `OTC_EXPIRY_WARNING_DAYS` | `3` | stock-service (SP5-E) |

## Serialization note

`GET /investment-funds` and `GET /me/watchlists` now **hand-shape** their items so every field is always present — in particular `metrics_available` (`false`) and `item_count` (`0`) no longer drop out when they hold default values (the raw proto-JSON omits `false`/`0`). Field **names and types are unchanged** (numeric ids stay JSON numbers); only the previously-omitted default fields are now always included. The detail endpoints already behaved this way.
