# Product-owner-proxy review — ADRs 0102–0104 (2026-09-28)

Read-only. Recorded by the orchestrator.

| ADR | Verdict |
|---|---|
| 0102 durable alerting | **ACCEPT** |
| 0103 casino bootstrap | **ACCEPT** |
| 0104 tenant-visible audit | **ACCEPT WITH CONDITIONS**. §5.3: G1 ships the **resolver with compiled-in defaults only**. The `audit_presentation_policies` table and its write API are deferred as registered follow-up AUDIT-PRESENTATION-POLICY-1. |

## Findings

**ADR 0104**
- **§5.3 presentation policy.** Resolver-only satisfies HD-PRH2-5: "supported through configuration" means the architecture must not block a different presentation, not that a writable table must exist before any jurisdiction needs one. There is a single jurisdiction today (Anjouan) and no B2B tenant, so the table would have no consumer. Deferral is purely additive; the record itself always keeps IP, metadata and actor.
- **§5.4 display name.** The orchestrator's option (a), a nullable `staff_users.display_name`, is minimal and reversible. No objection.

**ADR 0102**
- **Five tables.** They map one-to-one onto HD-PRH2-4's list: severity vocabulary, durable state and scope, dedup/idempotency (append-only occurrences, so tenants get no UPDATE on platform rows), routing (seeds nothing), and delivery, retry and escalation. Nothing is speculative.
- **§6.3 and §7.6.** Correctly left to security. Confirm both are closed before I-core merges.
- **Real channels and recipient resolver.** Correctly deferred as PROVIDER DEPENDENT.

**ADR 0103**
- **Provider-neutral.** Yes: generic route fields and per-tenant webhook signing.
- **MOCK discipline.** Only the vendor side is mocked, and it drives the real route over HTTP, so the MOCK cannot become a production workaround.
- **Player-ref tension.** The raw `PlayerAccountID` is still sent in `LaunchRequest`. Confirm CAS-PLAYER-REF-1 is registered; it is (`0939c5a`).
- **Open items.** Out-of-scope items are registered, not dropped.
