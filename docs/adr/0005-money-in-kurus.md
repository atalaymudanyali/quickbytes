# 0005. Money as whole kuruş

**Status:** Accepted · recorded 2026-10-22 (end of phase 1)

## Context

Computers store decimal numbers like `125.50` in binary, and most decimals can't be
stored exactly in binary, the same way 1/3 can't be written exactly in decimal. So
`0.1 + 0.2` becomes `0.30000000000000004`. With prices, totals, discounts and refunds,
those tiny errors add up, and a total can end up one kuruş off.

## Decision

**Every amount is a whole number of kuruş** (1 TL = 100 kuruş), everywhere: database,
Go code and JSON. `32000` means 320.00 TL. A cashier counting coins works the same
way: there's no half coin, so there's no rounding.

- Column: `price_kurus integer NOT NULL CHECK (price_kurus > 0 AND price_kurus <= 10000000)`.
  The upper bound is 100,000 TL, which catches a typo of a few extra zeros.
- Field names say the unit (`price_kurus`), so nobody has to guess.
- Only the display layer (the iPhone app) turns kuruş into "320,00 TL".

## Alternatives

| Option | Why not |
|---|---|
| Floating-point TL (`float64`, `125.5`) | Rounding errors, as above. |
| PostgreSQL `numeric` / a decimal library | Exact, but every layer (Go, JSON, Swift) needs a decimal type, and JSON numbers often turn back into floats on the way. |
| Amounts as strings (`"125.50"`) | Exact, but every sum needs parsing, and nothing stops `"12,550"` vs `"125.50"` mix-ups. |

## Consequences

- Sums and comparisons are plain integer maths: exact, fast and easy to test.
- Splitting an amount (for example a discount shared across items) needs an explicit
  rounding rule for the leftover kuruş. That gets decided when orders arrive in
  phase 2.
- One currency is assumed. Multiple currencies would need a currency field next to
  every amount.
