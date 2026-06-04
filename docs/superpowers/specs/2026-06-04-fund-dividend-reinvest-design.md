# Fund Dividend Auto-Reinvestment — design + plan (SP4 / TODO_final item C, Celina 4)

**Date:** 2026-06-04 · All in `stock-service` + gateway passthrough.

## Requirement
Funds currently credit received stock dividends to their RSD account (payout). Add a per-fund **`dividend_mode`** (`payout` | `reinvest`). In **reinvest** mode the received dividend automatically **buys more of the dividend-paying stock** for the fund (DRIP) instead of sitting as cash.

## Existing hook (from exploration)
`DividendService.Payout` (`dividend_service.go`) iterates fund holdings of the paid security, computes `gross = AmountPerShareRSD × fh.Quantity`, and `CreditAccount`s the fund's RSD account. A fund places a buy via `OrderService.CreateOrder(CreateOrderRequest{OnBehalfOfFundID, AccountID=fund.RSDAccountID, ActingEmployeeID=fund.ManagerEmployeeID, ListingID, Direction:"buy", OrderType:"market", Quantity})`, which reserves from the fund's RSD account and credits `fund_holdings` on fill. Dividend payout is a manual RPC (`DeclareDividend` → `PayoutDividend`), no cron.

## Design (reuse the tested order path — minimal new money movement)

Reinvest = **credit the fund's RSD account as today, then place a fund market-buy** for `qty = floor(gross / currentPriceRSD)` shares of the same security. The buy reserves the cost back out of the just-credited cash and the fill lands in `fund_holdings`; any sub-share remainder stays as fund cash. This reuses the existing, tested fund-order path — no bespoke debit/credit logic.

- **Model:** `DividendMode` enum (`payout`|`reinvest`, mirror `FundType`), field `DividendMode DividendMode gorm:"type:varchar(16);not null;default:'payout';index"` on `InvestmentFund`.
- **Service wiring:** `DividendService.WithReinvest(orderPlacer fundReinvestOrderPlacer, listings listingPriceLookup)`. Narrow interfaces:
  - `fundReinvestOrderPlacer`: `CreateOrder(ctx, CreateOrderRequest) (*model.Order, error)` (satisfied by `*OrderService`).
  - `listingPriceLookup`: `GetBySecurityIDAndType(securityID uint64, securityType string) (*model.Listing, error)` (satisfied by `*ListingRepository`).
- **Payout branch:** after the existing `CreditAccount` succeeds, if `fund.DividendMode == reinvest` and reinvest deps are wired: look up the listing for `(payment.SecurityID, "stock")`; convert `gross` and price to a share `qty = floor(gross / priceRSD)`; if `qty >= 1`, `CreateOrder` the market buy on behalf of the fund. **Best-effort:** any failure (no listing, zero price, qty 0, order error) logs a WARN and leaves the cash credited (graceful degrade to cash) — never aborts the payout loop. The `FundDividendPayment` snapshot is still recorded (gross is what the fund received; how it's deployed is a separate concern).
- **Create/Update:** `CreateFundInput`/`UpdateFundInput` accept an optional `DividendMode`; `CreateFund` defaults to `payout`. Proto `CreateFundRequest`/`UpdateFundRequest` gain `string dividend_mode`; `FundResponse` gains `string dividend_mode` (so the UI shows it).
- **Gateway:** `createFundRequest`/`updateFundRequest` accept `dividend_mode` (validated `oneOf("payout","reinvest")`), forwarded to the gRPC request; `FundResponse.dividend_mode` flows through automatically.

## Concurrency / safety
- The reinvest buy goes through `OrderService.CreateOrder`, which already reserves funds atomically and fills via the saga-backed engine. The dividend `CreditAccount` and the order reservation are independently idempotent (dividend by `creditKey`; order by its own keys). A reinvest-order failure cannot lose money — the dividend cash remains in the fund account.
- Idempotency: the existing per-(payment, fund-holding) idempotency guard prevents re-crediting on a re-run; the reinvest buy is attempted only on the first (non-idempotent-skipped) pass, so a Payout re-run does not double-buy.

## Testing
- **Unit (dividend service):** reinvest-mode fund → a market buy `CreateOrder` is placed for `floor(gross/price)` shares of the security on behalf of the fund (mock orderPlacer records the call); payout-mode fund → no order placed (current behaviour). Price-unavailable / qty<1 → no order, no error, cash credited.
- **Unit (service create/update):** `DividendMode` set/validated; invalid value rejected at the gateway.
- **Integration:** create a fund with `dividend_mode=reinvest`; `GET` detail shows `dividend_mode:"reinvest"`; `PUT` toggles to `payout`. (Full dividend→buy E2E needs a fund holding + declared dividend; covered by unit tests.)

## Out of scope
- Reinvesting into something other than the dividend-paying security (manager-chosen target) — DRIP only.
- Fractional shares — sub-share remainder stays as fund cash.

## VERSION: MINOR bump.
