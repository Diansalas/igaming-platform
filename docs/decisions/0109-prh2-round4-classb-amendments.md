# ADR 0109: PRH-2 round 4 Class-B amendments (payout reference binding, MA020 exposure semantics)

Status: ACCEPTED as amendment record (2026-10-06). Amends ADR 0095 §36.4 and ADR 0100 §5.2
(and ADR 0101 §26). It does NOT claim real-PSP support: everything here is MOCK-verified.

## 1. Payout reference binding (B10, PAY-PAYOUT-REFBIND-1) - amends ADR 0095 §36.4

ADR 0095 §36.4 states that tombstones are excluded from reference-conflict detection. That
remains true for **deposits**. For **payouts** a tombstone on `(tenant, provider, provider_tx_id)`
is now a CONFLICT: `withdrawal.Complete` posts under that provider key, so a tombstone would make
every redelivery fail on the unique index and roll back with the hold stuck. A payout whose
reported reference matches any attempt, `withdrawal_requests` row or ledger key (including a
deposit tombstone) is parked `provider_reference_conflict` (`bound_to=ledger_tombstone` etc.),
the hold is kept, nothing posts, and M2 force-resolution is refused (Go `M2ResolvableDispute` and
DB `payment_m2_admits`). **Consequence / open:** a legitimate payout parked this way has NO
operator resolution path (tracked: PAY-PAYOUT-CONTRADICTION-HOLD-1 widened to this reason;
CT-BLOCKED set in ADR 0107; B12 alert; operator runbook before live payouts).

## 2. MA020 exposure semantics (B4, MA020-SYNC-MISMATCH-1, migration 0119) - amends ADR 0100 §5.2

(Recorded on branch `prh2-r4-classb-b4`; effective when that branch merges.)
(a) R-MA-2: a **reference-less** park no longer blocks manual credits (0113 blocked reference-less
`multiple_success_for_intent` forever). Security argument: only the PSP controls the reported
reference; a bound park cannot become reference-less (COALESCE bind). Residual window: between the
park and the first statement line naming it, the only signal is the park-time P1 alert (delivery =
ALERT-DELIVERY-1); a four-eyes goodwill credit could hand-pay a capture later refunded/allocated.
(b) R-MA-3: clearing is no longer tombstone-only; an RC-3-eligible persisted `deposit_reversal`
statement line also clears (tenant session only; the acting session stays tombstone-only because
statement tables are invisible to it). The function therefore reads statement tables, contradicting
the 0100 line "no statement source needed".
(c) F-VIS: in both K2 sessions the Y evidence is invisible (RLS), so SQL fails closed: every
`poll_reference_mismatch` park blocks that player's manual credits even after X and Y clear, until
MA020-K2-VISIBILITY-1 lands (security's policy text and coupling rules are in the task registry).
(d) Before a real PSP: C3 (reversal line status/amount/asset not checked, fix Go and SQL together) and
C4 (statement-table INSERT limited to the system session shape) - MA020-RECLINE-CHECK-1 and
STMT-TABLE-INSERT-RLS-1.
