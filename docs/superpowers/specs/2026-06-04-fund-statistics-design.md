# Fund Statistics + History — design (SP3 / TODO_final item B, Celina 4)

**Date:** 2026-06-04
**Status:** Approved approach (per SP decomposition) — autonomous build. All in `stock-service`.

## 1. Requirement (Celina 4 §Statistika fondova)

On the fund discovery page, add sortable columns: **annualized return** (godišnji prinos), **reward-to-variability ratio** (Sharpe-style, higher is better), **max drawdown** (largest peak-to-trough), **volatility** (std-dev of monthly returns). Fund detail adds the metrics + a **historical fund-value chart** + a **comparison chart vs the average of all funds**. Metrics are shown only once there is **enough history** (a system-defined minimum number of snapshots).

## 2. What exists

- Fund NAV is computed live: `FundService.fundValueRSD` = cash (RSD account) + Σ(holding.qty × listing.price→RSD). No history is stored.
- Detail (`GetFund`) already returns investor count, contributed, NAV, profit, profit %. Discovery (`ListFunds`) returns base fund fields.
- Proven daily-cron pattern: `ListingCronService.StartDailyCron` (23:55 UTC, `cronreg` BeginRun/EndRun/TriggerChan).
- Proven daily-snapshot table pattern: `ListingDailyPriceInfo` + `ListingDailyPriceRepository` (composite unique `(listing_id, date)`, upsert-by-date, history query).

## 3. Design

### 3.1 Snapshot table + daily cron (the missing foundation)

- **Model** `FundValueSnapshot` (mirror `ListingDailyPriceInfo`): `ID`, `FundID` (uniq w/ Date), `Date` (date), `TotalValueRSD`, `LiquidRSDBal`, `HoldingsValueRSD`, `InvestorCount`. Composite unique `(fund_id, date)`.
- **Repository** `FundValueSnapshotRepository`: `UpsertByFundAndDate(snap)`; `ListByFundSince(fundID, since)` → ascending by date; `ListAllSince(since)` → all funds' snapshots ascending (for the average series).
- **Cron** `FundSnapshotCron` (mirror `ListingCronService`): daily at a configurable UTC time (default `23:50`). `RunOnce`: list active funds; for each, compute NAV via the existing `fundValueRSD` path + investor count/liquid/holdings (reuse `FundService.Statistics`), upsert one snapshot dated *today* (idempotent — re-run overwrites today's row). Registered in `cronreg` as `fund-snapshot-cron`; manual-triggerable. Startup catch-up pass writes today's snapshot immediately so a fresh deploy has ≥1 point.

### 3.2 Metrics (pure functions — `fund_metrics.go`)

Input: a fund's snapshots ascending `V_0..V_n` (NAV in RSD, with dates). Output `FundMetrics{AnnualizedReturnPct, VolatilityPct, RewardToVariability, MaxDrawdownPct}` + `available bool`.

- **Monthly resample:** reduce daily snapshots to the **last snapshot of each calendar month** → monthly NAV series `M_0..M_k`. Monthly returns `r_i = M_i/M_{i-1} − 1` (i=1..k).
- **Volatility** = sample standard deviation of `{r_i}`, expressed as a percentage.
- **Reward-to-variability** = `mean({r_i}) / stddev({r_i})` (risk-free = 0; unitless; higher better). Zero stddev → not available.
- **Annualized return** = `(V_n / V_0)^(365 / daysBetween(date_0, date_n)) − 1`, as a percentage (uses the full daily span, robust to partial months). Requires `date_n > date_0` and `V_0 > 0`.
- **Max drawdown** = `max over t of (peak_so_far − V_t) / peak_so_far` on the **daily** series (finer-grained), as a percentage.
- **Availability gate:** `available = len(monthlyReturns) >= MinMonthlyReturnsForMetrics` (config, default **2** → needs ≥3 monthly points) AND `V_0 > 0`. When not available, all metric fields are `"0"` / omitted and `metrics_available=false`. Config: `FUND_METRICS_MIN_MONTHLY_RETURNS` (default 2).

Computed **on demand** (no metrics table): the series is one small row-per-day table per fund; fund count is small. Detail computes for one fund; discovery computes per listed fund (see 3.4).

### 3.3 Detail page (`GetFund`)

Extend `FundDetailResponse` proto:
- `string annualized_return_pct, volatility_pct, reward_to_variability, max_drawdown_pct;`
- `bool metrics_available;`
- `repeated FundValueSnapshotItem history;` — this fund's `(date, total_value_rsd)` series.
- `repeated FundValueSnapshotItem average_history;` — the **system average** series: for each date, the mean across all funds of their NAV **indexed to 100 at each fund's first snapshot** (so funds of different sizes compare on % terms). Built from `ListAllSince`.

`GetFund` loads the fund's snapshots, computes metrics + history, and the average series, and fills the response.

### 3.4 Discovery (`ListFunds`) + sorting

- Extend `FundResponse` with `annualized_return_pct, volatility_pct, reward_to_variability, max_drawdown_pct, metrics_available`.
- Extend `ListFundsRequest` with `sort_by` (`name`|`value`|`profit`|`annualized_return`|`volatility`|`reward_to_variability`|`max_drawdown`) and `sort_order` (`asc`|`desc`, default `desc` for metrics).
- Service: when a **metric sort** is requested, load all active funds, compute each one's metrics from its snapshots, sort in memory (funds with `metrics_available=false` sort last), then paginate. For non-metric sorts, keep current behaviour and compute metrics only for the returned page (display). Fund count is small (a bank's set of funds), so the all-funds compute is acceptable; documented.

### 3.5 Gateway + docs

- Gateway `ListFunds`/`GetFund` JSON gains the new fields + `history`/`average_history` arrays; `?sort_by=&sort_order=` query params plumbed into the gRPC request (validated via `oneOf`). Swagger + `docs/api/REST_API_v3.md` updated.
- `Specification.md`: new entity, new cron, extended proto/route, new config var.
- `VERSION`: MINOR bump.

## 4. Concurrency / safety

- Snapshot upsert uses `ON CONFLICT (fund_id, date) DO UPDATE` (mirror the listing daily-price upsert) — idempotent same-day re-runs.
- Cron honors `ctx.Done()` and `cronreg` pause; per-fund failures log + continue (one bad fund doesn't abort the batch).
- Metrics are read-only pure functions; no shared state.

## 5. Testing

- **Unit (metrics):** known monthly series → asserted annualized return, volatility, reward-to-variability, max drawdown (hand-computed); `available=false` below the gate; zero-stddev → reward-to-variability not available; single-point series → all unavailable, no panic/div-by-zero.
- **Unit (repo):** upsert-by-date idempotency; `ListByFundSince` ordering; `ListAllSince` multi-fund.
- **Unit (cron):** `RunOnce` writes one snapshot per active fund with NAV = `fundValueRSD`; re-run overwrites same date (no dup).
- **Unit (service):** `ListFunds` metric-sort orders correctly with mixed available/unavailable; `GetFund` fills metrics + history + average series.
- **Integration:** seed a fund with ≥3 monthly snapshots (insert directly), `GET /investment-funds/{id}` returns `metrics_available=true` with non-empty `history`; `GET /investment-funds?sort_by=annualized_return` returns 200 and is ordered.

## 6. Out of scope / YAGNI

- No real-time intraday valuation; daily granularity only.
- No persisted metrics table (computed on demand; revisit only if fund count grows large).
- Risk-free rate fixed at 0 for reward-to-variability (no treasury-rate feed).
