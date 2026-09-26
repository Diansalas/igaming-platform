> Gate 10.3-W2/W3 — `code-reviewer` working paper (recorded 2026-09-26 against `becc5c2`; condensed from the reviewer's report). Dispositions: `05-gate-log.md`.

# Gate 10.3-W2/W3 independent code review

**Verdict: NOT READY — small fixes.** No money-writing bug (streams insert only run/mismatch/audit rows; the rejection
record changes no money). Merge integration, permission union, tenant isolation verified. Unit tests + vet only.

| # | Sev | Finding | Owner / fix |
|---|---|---|---|
| 1 | High (gate blocker) | Security W2A-SEC-1 (registry precondition) and W2A-SEC-2 (matched key_id log) still open at `becc5c2` | close-out pass (in progress) |
| 2 | Medium | C4 exemption applies when either rollback OR original touches bonus accounts → a corrupt cash-bet rollback crediting `player_bonus` passes undetected (`casino_consistency.go:569-571`) | ledger-finance: exempt on the original only + sub-test |
| 3 | Medium | C2 never requires a bet to credit / a win to debit `house_gaming` (only the negatives); statement totals can't catch it with the MOCK (`casino_consistency.go:337-343, 393-411`) | ledger-finance: positive check or recorded deviation |
| 4 | Medium | C6 fires on all 11 rejection classes vs paper 02 §2.4's 5; E9 different-reference is a P1 in C6 but a match in casino_statement | ledger-finance ruling on class set |
| 5 | Medium | labels overstated/stale: ADR 0093 awssm `IMPLEMENTED` (target PARTIALLY), wiring called STAGING REQUIRED (it is NOT IMPLEMENTED), refusal claim false, startup string "NOT IMPLEMENTED in this build (W3b)", registry rows 3882-3888 still "pending", 0097 comment stale | docs close-out |
| 6 | Low–Med | W3b tests: no ctx-honouring hang/timeout test through `Fetcher.Fetch`; no never-dials-real-AWS guard; `New` success path untested | close-out pass |
| 7 | Low | W3a: unpaired tombstones not flagged and many-to-one tombstone pairing — sound but undisclosed | ledger-finance docs |
| 8 | Low | `auth.RequireAnyPermission` has no unit test ("all-of" rewrite / empty list would pass); C4 boundary untested | small follow-up |
| 9 | Low | chain-tip pin tests hand-maintained (0092 prefix list, hard-coded counts) will break at 0099; two stale comments | small follow-up |
| 10 | Low | dead/test-only code (`ListRunsForStream`, `Router.Backends`, casino statement aliases, `DerivedTokenCache` unbounded/speculative, `RunSweepTenants` test-only); `WithTenantSnapshot` duplicates `WithTenant` | small follow-up |

Reported deviations judged: W2b C3 order check omitted — sound; C4 cash-only intent sound but too broad (#2);
`WithTenantSnapshot` sound (§2.15 allows RR; fail-closed under RC; working kill control); win = `house_gaming` debit sound;
unpaired tombstones sound but undisclosed (#7); PROV-OUTBOUND-CRED-1 PARTIAL label accurate, precondition unregistered (#1).
W2b/W3a test plans complete; W3b gaps in #6; govulncheck unpinned.
