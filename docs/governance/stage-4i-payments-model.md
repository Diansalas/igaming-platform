# Stage 4I — Payments Phase: the C-3(d) determination

**Status of this document: a RULING on `payments`' own contract
(`AdapterCapability.SupportedCountries`) and a scope note recording what
is deliberately NOT built in this phase.** This is `payments`' contribution
to Stage 4I's chain (`architect` reconnaissance → `identity-compliance` →
`risk` → `security` → `backend` → `casino` → `bonus-engine` →
**`payments`** → `sportsbook` → `qa` → `architect` final → independent
security/compliance final), per the directive. `docs/governance/stage-4i-
canonical-model.md` §4.5 explicitly deferred payments' own design phase to
this document: "`payments` has not had its design phase. This document does
not specify the payments contract and no implementer should infer one."

No code behavior was changed to produce this ruling. The only changes made
alongside it are two non-functional doc-comment clarifications in
`internal/payments/types.go` and `internal/payments/orchestrator.go`,
recorded in §4 below.

---

## 1. The question this phase was asked to settle

Reconnaissance **C-3(d)** and Risk's Phase 3 report (`stage-4i-risk-
model.md` §1.4) both characterize `AdapterCapability.SupportedCountries`'
"empty ⇒ not country-restricted" semantic as part of a family of
"absent-jurisdiction-value" contracts alongside AssetAuthorization
(absent ⇒ unconditional deny), Risk (absent ⇒ conditional deny), and
Casino's per-game blocklist (absent ⇒ skip, the K-3 defect fixed earlier
this stage). Risk's own table (§1.4) labels (d) "**a defect and a
code-space confusion**."

This phase's job was to determine, from the actual code and its own doc
comments — not from the label alone — whether `SupportedCountries`' empty-
means-permissive semantic is:

- **(A)** a genuine jurisdiction-gating defect requiring remediation, in
  the same family as K-3, or
- **(B)** a legitimate, differently-designed provider-capability concept
  that the "permissive" framing miscategorizes.

## 2. Determination: **(B)**. Not a jurisdiction-gating defect. No functional remediation needed.

### 2.1 What `SupportedCountries` actually is, read from the code

`AdapterCapability.SupportedCountries` (`internal/payments/types.go:188-
219` after this phase's clarification) is one of four adapter-declared,
static capability lists on the same struct: `SupportedFiatCurrencies`,
`SupportedCryptoAssets`, `SupportedPaymentMethods`, `SupportedCountries`.
ADR 0022 §2 - the frozen design this package implements - names it
explicitly: `supported_countries []TEXT -- ISO country codes; empty/
omitted means "not country-restricted", never "all countries" by silent
default`. This is a Stage 3B decision, made before any jurisdiction work
existed, describing a payment rail's own technical market/coverage
footprint (the same kind of fact as "this local payment method only
settles in Brazil," or "this card network has no declared country
restriction") - not a claim about which markets the *platform* is legally
licensed to serve.

### 2.2 Where it is actually consumed - confirmed by code, not assumed

`SupportedCountries` has exactly one production consumer:
`WriteCapability`'s narrow-only invariant (`internal/payments/
capability.go:304-314`), which enforces that a tenant-configured
`ProviderCapability` row may only *narrow* an adapter's declared country
set, never widen it to "unrestricted" or beyond the declared set. This is
a data-integrity check between two capability layers (adapter-declared vs.
operator-configured, ADR 0022 §2's "two layers, one shape") - it makes no
routing decision and gates no operation.

`RouteProvider` (`internal/payments/orchestrator.go`), the only place a
live deposit/withdrawal routing decision is made, **never reads
`SupportedCountries` at all**. Routing dimension 2 (country/jurisdiction)
remains an explicit `TODO(jurisdiction)` (`orchestrator.go:82-104`,
re-confirmed unchanged at HEAD). There is therefore no live gate today
whose absent-value behavior could be "fail-open" or "permissive" in the
way K-3's blocklist check was: K-3 was already *wired into* `LaunchGame`
(guarded by a now-removed `params.JurisdictionCode != nil` check) before
its fix; `SupportedCountries` has never been wired into any routing
decision at all. There is no control to arm, because the control does not
exist yet.

### 2.3 The asymmetry with its sibling fields is original design, not an oversight

`RouteProvider`'s dimension checks for currency/asset and payment method
use `containsString` - **membership required**, so an empty
`SupportedFiatCurrencies`/`SupportedCryptoAssets`/`SupportedPaymentMethods`
list means "supports none" (correctly modeling, e.g., a crypto-only
provider's genuinely-empty fiat list - ADR 0022 §2's own comment: "empty
for a crypto-only provider"). `SupportedCountries` was deliberately given
the opposite default in the same ADR, in the same section, for a
different reason: currency/method support is essential, enumerable
information every adapter must declare to be routable at all, whereas
country restriction is the exceptional case most real PSPs never bother
declaring at all. Treating silence as "no known restriction" avoids
forcing every adapter to enumerate the ISO-3166 list just to be routable.
This is a reasoned, sibling-inconsistent-on-purpose design choice recorded
in the frozen ADR before this stage began - not an inverted regulatory
absent-value contract discovered by accident.

### 2.4 Why this is a different concept from AssetAuthorization/Risk/Casino's gating semantics

The canonical model's concept-distinction discipline (player location vs.
residence vs. nationality vs. tenant licensing jurisdiction vs. brand
operating jurisdiction vs. transaction jurisdiction vs. product
jurisdiction) exists precisely to stop two different questions from being
answered by one field. AssetAuthorization/Risk/Casino's absent-value
contracts all answer the same underlying question: **"is this operation
permitted under a jurisdiction this platform is licensed/configured to
apply rules for?"** - a regulatory permission question, keyed in
`jurisdictions.code`/`.id` space, gated on a value `internal/jurisdiction`
resolves from server-side context. `SupportedCountries` answers a
different question entirely: **"does this specific payment rail
technically process transactions touching this country at all?"** - a
vendor capability question, keyed in ISO-3166 space, with no dependency on
`internal/jurisdiction` and no path to one today. Treating the second as a
degenerate case of the first - which is what comparing their "empty"
defaults directly does - is itself the kind of concept-conflation the
directive warns against, not a resolution of one.

It does not map cleanly onto "product jurisdiction" (concept 7) either:
product jurisdiction is about what the *platform's own licence* permits
(e.g., a licence covering casino but not sportsbook in jurisdiction X);
`SupportedCountries` is about what a *specific third-party vendor's rail*
technically reaches, independent of the platform's own licence. It is
closer to a pure vendor-integration fact with no jurisdiction analogue in
the canonical model's list - a legitimate "none of the above."

### 2.5 The genuine, narrower finding worth keeping from C-3(d)

Risk's report bundled two claims into C-3(d); only one survives scrutiny
as a real hazard:

- **"Empty means permissive" is a defect** - **not adopted**. Per §2.1-2.4
  above, it is a correctly-designed, currently-inert capability default,
  not a jurisdiction gate.
- **Code-space confusion (ISO country vs. `jurisdictions.code`)** -
  **adopted as a genuine, real hazard**, confirmed independently by
  migration `0035`'s own comment on `casino_games.jurisdiction_blocklist`,
  which cites `supported_countries` as *the schema precedent* for using an
  unconstrained `TEXT[]` "as data, not a referential constraint." That
  precedent is sound for each field *within its own code space*, but a
  future implementer skimming both fields could easily assume they share
  one code space because they share a schema pattern. They do not, and no
  mapping between them exists (recon §1.2 point 3). This is a real,
  worth-documenting risk for whoever eventually builds routing dimension 2
  or a country→jurisdiction mapping - addressed in §4 below, not by a
  behavior change now.

## 3. Scope confirmations

- **`withdrawal_policies.jurisdiction_code`'s `CHECK (jurisdiction_code IS
  NULL)` (migration 0033) is untouched.** No migration was written or
  modified in this phase. `internal/withdrawal/policy.go` was read only;
  its jurisdiction dimension remains exactly as recon's K-6 and the
  canonical model §4.5 describe it - accepted, disabled, every caller
  passes `nil`. This phase did not lift, weaken, or work around that
  constraint, and nothing in §4's doc-comment changes is reachable from
  any code path that writes to that column.
- **Routing dimension 2 (`TODO(jurisdiction)`) is not implemented in this
  phase.** No country→jurisdiction mapping, no new routing filter, no
  `internal/jurisdiction` consumption was added to `internal/payments`.
  The canonical model §4.5 requires any such mapping to be "an explicit,
  stored, audited mapping... an authorization surface... requiring
  `security` review when proposed" - none of that exists yet, and this
  phase does not propose one.
- **`internal/jurisdiction`'s internals were not touched** - only referred
  to by name (`Resolve`) in a doc comment, as the mechanism a *future*
  dimension-2 gate would use.
- **No Human Decision Register item is resolved or unblocked** by this
  phase. HDR-J-1/J-3/J-6 remain exactly as open as before; this
  determination concerns a provider-capability default, not player
  jurisdiction resolvability.

## 4. What was actually changed

Two non-functional doc-comment clarifications, no behavior change:

1. `internal/payments/types.go` - `SupportedCountries`' doc comment now
   states explicitly that it is a payment-rail capability fact in
   ISO-3166 space, records this phase's determination and citation, and
   binds two rules on any future implementer of routing dimension 2 or a
   country→jurisdiction mapping: (1) never substitute a
   `SupportedCountries` value for a `jurisdictions.code` value without an
   explicit, audited, `security`-reviewed mapping; (2) never reuse this
   field's "empty = unrestricted" capability default as the absent-value
   *contract* for a future jurisdiction/regulatory gate - that gate
   resolves jurisdiction via `internal/jurisdiction.Resolve` and satisfies
   the platform-wide invariant independently.
2. `internal/payments/orchestrator.go` - `RoutingRequest`'s
   `TODO(jurisdiction)` comment gained a pointer to this document and the
   same reminder, at the one call site a future implementer would edit
   first.

No test changes were needed - no behavior changed. Full validation floor
(`go build ./...`, `go vet ./...`, `gofmt -l .`, unit + `-tags=integration`
with `-race`, full repo suite) was re-run and is clean (see the commit
implementing this document for the exact results).

## 5. Summary table for `architect`'s final synthesis

| Item | Disposition |
|---|---|
| C-3(d) "empty means permissive" | **Not a defect.** Legitimate, ADR-0022-original provider-capability default; no live gate consumes it; no remediation |
| C-3(d) code-space confusion (ISO country vs. `jurisdictions.code`) | **Real, narrower hazard**, addressed by doc-comment hardening in `types.go`/`orchestrator.go`, not a behavior change |
| Routing dimension 2 (`TODO(jurisdiction)`) | Still not implemented; still requires a future, security-reviewed, explicit country→jurisdiction mapping if ever built |
| `withdrawal_policies.jurisdiction_code` CHECK (migration 0033) | Untouched, confirmed |
| Other payments-domain jurisdiction/country/region/market fields | None found beyond `SupportedCountries` (repo-wide grep of `internal/payments` and `internal/withdrawal` for country/region/market/geo/jurisdiction, confirmed against `internal/withdrawal/policy.go`'s already-known, already-disabled `jurisdictionCode` parameter - no new finding) |
| HDR-J-1/J-3/J-6 | Not touched, not unblocked |
