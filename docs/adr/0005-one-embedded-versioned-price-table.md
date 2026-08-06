# 0005. One embedded price table, versioned, with unconfirmed prices marked

## Status

Accepted.

## Context

Every USD figure this gateway reports, on a dashboard, in the admin API,
or in a denial message, is arithmetic over a per model rate. Those rates
are external facts that change without notice, and the gateway is
expected to still be able to explain a number it produced last month.

Three failure modes were on the table. Prices could disagree between
copies, so a dashboard and a ledger would report different money for the
same request. Prices could change, so an old figure would become
unexplainable. And a model with no price could be treated as free, so an
expensive unpriced model would look like the cheapest thing in the
fleet.

There was also an honesty problem. Confirming every list price against a
published pricing page is slow, and most of the table had not been
confirmed.

## Decision

One table, compiled into the binary.
[internal/pricing/pricing.yaml](../../internal/pricing/pricing.yaml) is
embedded with `go:embed` and is the only copy in the repository. There is
deliberately no second copy at the repository root. An operator who wants
different numbers copies this file out and points `Load` at the copy,
which makes their table a fork rather than an override.

The table carries a `version` and an `updated` date. `version` must be
at least `MinVersion`, which is 1, and validation says why: an absent
version field decodes to 0, and a ledger row stamped 0 could not be
traced back to any price list
([internal/pricing/pricing.go](../../internal/pricing/pricing.go)).

Every settled request writes a ledger row carrying `price_version`
alongside the tenant, model, token counts, and USD
([internal/pricing/ledger.go](../../internal/pricing/ledger.go), written
by `Guard.Settle` in
[internal/budget/guard.go](../../internal/budget/guard.go)). A figure
recorded before a price change stays explainable after it, because the
row names the prices that produced it. Bumping `version` on any price
change is the rule the file's own header states.

An unpriced model returns not found rather than a guessed price.
`Table.Cost` returns `(Cost{}, false)` for a model with no entry, and the
caller records a zero, so an unpriced model appears as zero spend against
real token counts rather than as an absent metric series that reads as no
traffic.

Rates that were not checked against a published page are marked
`# unverified` in the table itself. At `version: 1`, dated `2026-08-05`,
30 of the 38 model entries carry that mark. The README, the file header,
and [docs/cost-accounting.md](../cost-accounting.md) all say the same
thing: the arithmetic is exact, the ledger reconciles with the running
totals, and the absolute numbers should not be trusted for billing until
each rate is confirmed.

Tiered pricing is not modelled. Where a provider tiers by context length
the base tier is used, and cached input rates, batch discounts, and
negotiated rates are out of scope.

## Consequences

A cost figure is reproducible. Given a ledger row, the price version, and
the table at that version, the USD amount can be recomputed by hand. That
is the difference between a number on a dashboard and a number an
operator can check, and it is what `da4eefe` was verifying when two real
completions produced two rows whose costs summed to exactly the total
the admin API reported.

Prices go stale silently. Nothing in the gateway fetches a price or warns
that `updated` is old. A rate that changed upstream keeps being applied
until somebody edits the file and bumps the version. This is a known gap,
not an oversight.

Updating prices requires a rebuild and a redeploy, because the table is
compiled in. That was accepted deliberately: an external file that can be
edited under a running process is a second source of truth for money.
`Load` exists for operators who want that anyway.

The shipped table is mostly unverified, and saying so in three places
does not make the numbers better. A user who ignores the warning gets
plausible looking money that is wrong. The alternative, shipping no
prices, would have made the entire cost feature undemonstrable, so the
trade was made explicitly and labelled everywhere it shows up.

Unknown models cost nothing on the dashboard. Zero is recorded rather
than a series being omitted, so the traffic is visible even though the
money is not, but somebody still has to notice.

## Alternatives considered

**Fetch prices from provider APIs at startup or on a schedule.**
Rejected. Providers do not offer a usable pricing API, the ones that
publish rates publish them as HTML pages, and a gateway whose cost math
depends on a network fetch has a new failure mode where it cannot price
anything. It also makes a cost figure unreproducible, since the rate at
the time is not recorded anywhere.

**A price file loaded from disk with no embedded default.** Rejected. It
makes the binary unable to price anything on its own, which turns a
misplaced file into silent zero cost accounting. `Load` still exists for
operators who want their own numbers, so nothing is lost.

**Store the rate on each ledger row instead of a version stamp.**
Rejected, though it is the closest alternative. It would make a row self
contained. It also makes every row larger, makes an audit of "which rows
were priced wrong" require scanning the rows instead of reading a version
number, and offers no way to record that a whole table was revised. A
version is one integer that names an entire consistent set of rates.

**Guess a price for an unknown model, for example the median of the
table.** Rejected. A guessed price is indistinguishable from a real one
on a dashboard, and the error compounds silently across every request to
that model. Reporting the model as unpriced makes the gap visible.

**Ship only prices that had been confirmed.** Rejected. It would have
left the table with a handful of entries, most real traffic unpriced, and
the cost accounting feature impossible to demonstrate. Shipping the
best effort figures with each unconfirmed one marked keeps the feature
usable while making the uncertainty impossible to miss.
