# Fiat PSP provider-integration package

Status: **DOCUMENT / SPECIFICATION PACKAGE ONLY.** No adapter, no PSP call, no credential, no account, no agreement.
Prepared 2026-10-16 by `payments` on branch `psp-integration-package` (base: `claude/focused-wright-jw88w9`).
Provider-integration phase item 1 of 2 (fiat PSP; casino is the other first-priority package, owned elsewhere).

Labels (CLAUDE.md): everything in this directory is `NOT IMPLEMENTED` as software. The Nuvei evaluation is a
CANDIDATE evaluation (owner directive 2026-10-11: Nuvei is not mandatory) and is `BLOCKED` on public-documentation
access (see below). Nothing here is a statement that any PSP is licensed, contracted or approved.

## Read this first: documentation access limitation

The Nuvei public developer documentation (`https://docs.nuvei.com/`) could **not** be read in the session that wrote
this package: the session's egress proxy answered `403` to `CONNECT docs.nuvei.com:443` (policy denial, recorded by the
proxy status endpoint) and the web search/fetch tools were not available. The rule "do not route around a policy
denial" was followed. Consequently **no Nuvei-specific behaviour is asserted anywhere in this package as fact**; every
Nuvei cell is `UNVERIFIED (needs provider documentation/sandbox)` and no Nuvei URL is cited as a source. The platform
side (what any PSP must supply) is fully grounded in the repository and is verified against code. Re-running the
documentation review with working access is gate G1 in `integration-gates.md`; it is a read-only task that needs no
credential and no account.

## Index

| File | Content |
|---|---|
| `nuvei-compatibility-assessment-2026-10-17.md` | **Current Nuvei assessment** (search-summary evidence of the official docs; conditional viability; new gaps G-09/G-10 and finding N-1; decisions needed). |
| `nuvei-evaluation.md` | Candidate evaluation: per-topic platform requirement vs Nuvei (all UNVERIFIED), fit/gap table, viability statement, alternatives, what to obtain from the vendor, UNVERIFIED registry (N-U01..). |
| `provider-neutral-mapping.md` | Platform requirement -> what the PSP must supply -> how the neutral interface carries it -> gap, with concrete code paths. Includes the platform-side abstraction gaps found (G-01..). |
| `conformance-plan.md` | Conformance specification a real adapter must pass: existing suites it maps to, new cases needed (PSP-C-nn), the A11 binding rows, SR11-1 / LF-A11-1 / 2 / 6 prerequisites. |
| `integration-gates.md` | Gate sequence G0..G9 with entry criteria, owner, evidence; the exact owner/vendor inputs. No real money until explicit production authorization. |
| `open-questions.md` | The minimum decisions/inputs, each with a recommendation. |

## Principles applied

- Providers adapt to the provider-neutral interfaces (`internal/payments/types.go`, `contract.go`,
  `internal/reconciliation/statement/statement.go`, `internal/payoutinstrument`); the core is not redesigned around
  the first provider. Where a real PSP is likely to expose a missing abstraction, it is documented as a gap with scope
  (provider-specific vs platform-wide), the review it needs (security / ledger-finance) and, only if necessary, a core
  change proposal. No core change is made by this package.
- Hosted fields / redirect only; the platform never stores or handles a raw PAN (CLAUDE.md; `internal/payoutinstrument`
  refuses any Luhn-valid 12-19 digit string).
- Sandbox only until explicit production authorization; synthetic tenant; no real money.
