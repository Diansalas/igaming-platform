_Reviewer: `security`. Branch `prh2-d1-poll-amount` @ `ea17d68`. Recorded verbatim by the orchestrator._

Security re-review — PRH-2 D1 (prh2-d1-poll-amount @ ea17d686cfe386f454e05b4be4816c877bb2f93f; fix round 39604a0 and ea17d68 on top of 07aa48d; earlier review reviews/d1-security.md)

VERDICT: ACCEPT. D1-F1 and D1-F2 are CLOSED. All six required mutants are re-killed, a full `-race` pass is green, and nothing is pre-merge. There are two Low items (D1-F3, unchanged and not pre-merge, and one new Low, D1-L1).

Method:
- Disk: 13G free at the start, 12G at the end.
- Fresh private DB sec_d1b_rv_20261003. DB access worked. Dropped WITH (FORCE); verified 0 left.
- git archive of ea17d68 into the scratchpad. Built once; go vet clean. The export, logs and scripts are deleted.
- No role or credential changes, no sudo, no shared cache touched, pipefail on, grepped for FAIL.
- Disclosure: my first mutant batch hit the tool's 600s background limit and was killed partway through M5. I immediately restored the mutated file from its saved copy and cmp-verified sweeper.go, poll_evidence.go and drive.go byte-identical to ea17d68 (no test process was left running). I then re-ran M5 and M6 individually in the foreground; both reverted identically.

------------------------------------------------------------------
1. D1-F1: CLOSED. sweeper.go:580-601 (`case ErrorClassDefiniteDecline`) now uses the attempt's BOUND reference (`refPtr = attempt.ProviderReference`, :587) for both finalizeDeclined and DeclineEvidence.ProviderRef. A differing non-empty echo is audit-only (`payments.poll_decline_reference_mismatch`) through echoAuditMeta.
Probes (a live ambiguous attempt bound to R; the poll returns Declined with echo E):
- E fresh and valid: 0 sweep errors; attempt declined; attempt and intent reference = R; 1 mismatch audit.
- E bound to another intent: 0 sweep errors (no more 23505 loop); declined; attempt and intent reference = R; 1 mismatch audit.
- E invalid ("bad\x01ref"): 0 sweep errors (no more CHECK loop); declined; attempt and intent reference = R; 1 mismatch audit; no raw invalid value in audit_log.

2. D1-F2: CLOSED. sweeper.go:500-513 passes `ProviderReference: bound` and adds echoAuditMeta only for a differing echo.
Probe (succeeded attempt; mismatched poll, 4999, with echo "bad\x01" + 300×"Z"):
- audit provider_reference is the bound reference;
- 0 audit rows contain the raw echo.
The receipt.go change only makes auditTerminalAmountAssetMismatch take an extra map. echoAuditMeta writes only echo_* or echoed_provider_reference keys, so it cannot override provider_reference.

3. D1-F3 (raw, unbounded asset echo in audit metadata): CONFIRMED NOT PRE-MERGE (Low). The classification is unchanged. It is not a money, binding or confidentiality issue: jsonb escapes the value safely and it never reaches a key or a binding. It is audit growth driven by provider data. Fix before real-provider use: record provider_asset_code / echoed_asset_code only if it matches the asset-code format (e.g. at most 16 bytes, [A-Z0-9_-]), otherwise record its length and hash prefix. Sites: poll_evidence.go (mismatch extra; Missing metadata, both live and the new declined D1-M1 path), receipt.go auditTerminalAmountAssetMismatch (`echoed_asset_code`), and C's sync_amount_mismatch park in drive.go.

4. Mutants (the killing tests are this branch's own; each was reverted and cmp-verified identical):
- M1, echo as posting key (sweeper.go:556 boundRef → res.ProviderReference): KILLED by TestPollReference_EmptyOrMatchingEcho_PostsUnderTheBoundReference and TestPollDeclinedAttempt_ContradictionAuditedMatchingPostsT13.
- M2, tombstone lookup uses the echo (poll_evidence.go:134): KILLED by TestPollTombstone_OnBoundReference_DisputesEvenWithAnEmptyEcho.
- M3 = D-REF-3, echoAuditMeta also stores an invalid echo (poll_evidence.go:176): KILLED by TestPollReference_DifferentNonEmptyEcho_ParksNoPosting, TestPollDecline_EchoNeverOverwritesTheBoundReference and TestF3SM_TerminalMismatchAudit_NeverStoresARawEcho.
- M4 = D-F1-1, Decline uses the raw echo (sweeper.go:587): KILLED by TestPollDecline_EchoNeverOverwritesTheBoundReference.
- M5 = D-F2-1, terminal audit stores the raw echo (sweeper.go:512): KILLED by TestF3SM_TerminalMismatchAudit_NeverStoresARawEcho.
- M6 (my own), the F-C4 ledger_transactions binding check disabled (drive.go, `AND false`): KILLED by both TestFC4_* tests.

5. `-race` pass: internal/txscope (TestPCG1, INV-IO-1(c)) ok. internal/payments/... ok in three name-partitioned runs (^Test[A-H] 48s, ^Test[I-O] 167s, ^Test[P-Z] 199s), with no DATA RACE. Every test in the package matches one partition (none starts with a non-letter after "Test"), and payments has no subpackages. The A7 lock-order tests are included.

6. Survivors' classification:
- D-ECHO-2 (ApplySuccess links the echo): AGREE, EQUIVALENT. ApplySuccess writes `provider_reference = COALESCE(provider_reference, $3)`. On the poll-success path the attempt always has a bound non-empty reference (the branch returns an error otherwise, sweeper.go ~542), and a differing echo has already been parked as poll_reference_mismatch. So the argument cannot change the row.
- D-DRAIN-4 (callback-path T4/T9 drain): AGREE, PRE-EXISTING and out of scope. D1's only receipt.go change (564c515..ea17d68) is the auditTerminalAmountAssetMismatch signature; the drain site is untouched. It stays owned by PAY-RECEIPT-T4-DRAIN-TEST-1, and that item should land before real-provider use.

7. C's pending-only redirect rule: HOLDS. playerFacingRedirect is still applied after phase C in InitiateDepositAttempt (deposit_v2.go:337) and driveCreatedAttempt (drive.go:248). D1 adds no player-facing path; the sweeper and poll code return no redirect or token.

------------------------------------------------------------------
NEW (Low, not pre-merge)

D1-L1: sweeper.go:599-601, `} else if res.ProviderReference != "" { refPtr = &res.ProviderReference }`, still binds a raw, unvalidated echo when the attempt has NO bound reference. The Pending branch's ref-less fallback (~451-454) does the same. Both are unreachable from the sweeper today, because processViaQueryStatus only polls by `*attempt.ProviderReference`, and applyStatusEvidence has no other caller. They are latent: a future direct caller would reintroduce D1-F1. Fix (one line each): drop the fallback (refPtr = nil), or bind only after providerref.Validate plus foreignReferenceBinding, mirroring C's T6.

Scope: code- and DB-level review of the D1 fix round; no penetration test. The ADR 0095 §36 text was read only for the survivor classification.

Relevant paths (commit ea17d68):
- internal/payments/sweeper.go (Decline branch 580-601; succeeded-terminal audit 500-513; posting key 556)
- internal/payments/poll_evidence.go (echoAuditMeta ~169-181; tombstone 134; D1-M1 declined-Missing audit ~89-95)
- internal/payments/receipt.go (auditTerminalAmountAssetMismatch extra map)
- internal/payments/drive.go (playerFacingRedirect 248/258; F-C4 ledger check)
- internal/payments/deposit_v2.go (line 337)
- docs/plans/payment-readiness/evidence/prh2-d1-mutation-kill.txt
