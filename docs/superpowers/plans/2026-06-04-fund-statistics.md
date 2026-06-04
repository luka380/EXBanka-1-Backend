# Fund Statistics + History Implementation Plan (SP3)

> REQUIRED SUB-SKILL: superpowers:executing-plans. Reference spec: `docs/superpowers/specs/2026-06-04-fund-statistics-design.md`.

**Goal:** Per-fund annualized return, volatility, reward-to-variability, max drawdown + daily NAV history + vs-average comparison, sortable on discovery.

**Architecture:** New `fund_value_snapshots` table + daily cron (NAV per active fund); pure metrics functions over the series; proto/service/gateway plumbing. All in `stock-service`.

---

## Task 1: Metrics math (TDD core) — `internal/service/fund_metrics.go`
- [ ] Write `fund_metrics_test.go` first with hand-computed expectations (see spec §3.2): annualized return, volatility (stddev of monthly returns), reward-to-variability (mean/stddev), max drawdown (daily peak-to-trough); availability gate; div-by-zero/single-point safety.
- [ ] Implement `type SnapshotPoint struct { Date time.Time; ValueRSD decimal.Decimal }`, `type FundMetrics struct {...}`, `ComputeFundMetrics(points []SnapshotPoint, minMonthlyReturns int) (FundMetrics, bool)` with monthly resample (last point per calendar month), float64 math internally, decimal in/out. Run tests → pass. Commit.

## Task 2: Snapshot model + repository — `model/fund_value_snapshot.go`, `repository/fund_value_snapshot_repository.go`
- [ ] Model `FundValueSnapshot{ID, FundID(uniq+Date), Date, TotalValueRSD, LiquidRSDBal, HoldingsValueRSD, InvestorCount}` (mirror `ListingDailyPriceInfo`).
- [ ] Repo: `UpsertByFundAndDate` (ON CONFLICT (fund_id,date) DO UPDATE), `ListByFundSince(fundID, since)` asc, `ListAllSince(since)` asc. Test upsert idempotency + ordering. Commit.

## Task 3: Daily snapshot cron — `internal/service/fund_snapshot_cron.go`
- [ ] Mirror `ListingCronService`: `NewFundSnapshotCron(fundRepo, snapshotRepo, fundSvc, cronUTC, registry)`, `RunOnce(ctx)` (active funds → `fundSvc.Statistics` → upsert snapshot dated today), `StartDailyCron(ctx)` (default 23:50 UTC, BeginRun/EndRun/TriggerChan, startup catch-up). Per-fund failures log+continue.
- [ ] Wire in `cmd/main.go` (construct after fundService + snapshotRepo; `AutoMigrate(&model.FundValueSnapshot{})`; `.StartDailyCron(ctx)`).
- [ ] Test `RunOnce` writes one snapshot per fund; re-run overwrites same date. Commit.

## Task 4: Proto — extend FundResponse + FundDetailResponse + ListFundsRequest
- [ ] `contract/proto/stock/stock.proto`: `FundResponse` += `annualized_return_pct, volatility_pct, reward_to_variability, max_drawdown_pct, metrics_available`. `FundDetailResponse` += same 4 + `metrics_available` + `repeated FundValueSnapshotItem history` + `repeated FundValueSnapshotItem average_history`. New `FundValueSnapshotItem{ date, total_value_rsd }`. `ListFundsRequest` += `sort_by`, `sort_order`. `make proto`; build contract. Commit.

## Task 5: Service + handler wiring
- [ ] `FundService`: `WithSnapshots(snapshotRepo, minMonthlyReturns)`; helper `fundMetrics(fundID)` and `fundHistory(fundID)` from snapshots; `averageHistory()` from `ListAllSince` (index each fund to 100 at its first point, average per date). `List` gains metric-sort handling (spec §3.4).
- [ ] `investment_fund_handler.go`: `GetFund` fills metrics + history + average_history; `ListFunds` passes sort + fills per-fund metric fields; `toFundResponse` extended.
- [ ] Unit: metric-sort ordering (mixed available), GetFund fills fields. Build + test + lint. Commit.

## Task 6: Gateway + docs + version
- [ ] Gateway `investment_fund_handler.go`: surface new fields + `history`/`average_history`; plumb `?sort_by=&sort_order=` (validate via `oneOf`). Swagger regen.
- [ ] `docs/api/REST_API_v3.md` + `Specification.md` (entity, cron, proto, config `FUND_METRICS_MIN_MONTHLY_RETURNS`). VERSION MINOR bump + version.go. Lint. Commit.

## Task 7: Integration test — `test-app/workflows/wf_fund_stats_test.go`
- [ ] Seed a fund + ≥3 monthly snapshots directly; `GET /investment-funds/{id}` → `metrics_available=true`, non-empty `history`; `GET /investment-funds?sort_by=annualized_return` → 200, ordered. Commit.

## Self-review
- Coverage: snapshots+cron (T2,T3), metrics (T1), detail metrics+history+average (T4,T5), discovery sort (T4,T5), min-count gate (T1), gateway+docs (T6). All spec sections mapped.
- Types: `SnapshotPoint`/`FundMetrics` (T1) reused by service (T5); proto `FundValueSnapshotItem` (T4) = history items (T5) = gateway JSON (T6).
