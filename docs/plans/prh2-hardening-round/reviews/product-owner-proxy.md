# Product-owner-proxy review — PRH-2 plan (2026-09-28)

Reviewer: `product-owner-proxy` (read-only; the reviewer has no write access, so the orchestrator committed this verbatim in substance). Plan reviewed: `docs/plans/prh2-hardening-round/plan.md` at `a3547f5`.

**Verdict: APPROVE WITH CONDITIONS** (F1–F3; none blocks W0).

| ID | Severity | Plan section | Recommendation |
|---|---|---|---|
| F1 | Medium | §7 HD-PRH2-1 | Option (a) ("never" four-eyes) risks reading as licence to disable a CLAUDE.md non-negotiable ("manual balance adjustments require a reason code and four-eyes approval above a configurable threshold"). State the floor in the question itself: the **threshold** is configurable (it can be zero, i.e. "always"); the four-eyes **capability** cannot be removed. Record (b) as the engineering default/floor unless the human explicitly amends CLAUDE.md, and route any "(a)" answer through an explicit CLAUDE.md amendment record, not only an ADR 0098 note. Escalating is correct; the framing must show the floor. |
| F2 | Low | §3 vs §5 | `KS-CAS-DISCRIM-TEST-1` appears only as a parenthetical in the W2 cell; add a §5 line naming the test, owner and reviewer. |
| F3 | Low | §7 CI must-pass names | CI changes are already out of scope per the human; record this as **resolved: no** rather than an open question. |
| F4 | Info | §5-K | K's design (closed-enum capabilities, reuse of existing four-eyes patterns, grant read inside the action's own transaction, never from the JWT, "not a second authorization system") is the minimum that satisfies 0098 §1–3. Not scope creep. |
| F5 | Info | §6 | Each of the six new ADRs maps to a genuinely new cross-cutting decision; C, D, E1, E2, F, H and J are correctly amendments to existing ADRs. |
| F6 | Info | §5-I | Alerting is proportionate to the launch-blocking P1 and CLAUDE.md's audit/observability rules; real paging correctly deferred (HD-PRH2-4). |
| F7 | Info | §5-L | Links-only `docs/HANDOVER.md`, handover folded into every workstream's DoD, no reorganisation; the mock-vs-real matrix and secret-names inventory fill verified gaps. |
| F8 | Info | §2, §9 | Wave order is dependency-driven and serves B2C-first: launch-blocking hardening proceeds while the B2B-flavoured capability questions await the human. |

**Human decisions:** HD-PRH2-3 is a confirmation, not a decision (fine). HD-PRH2-4, -5, -6 are genuinely the human's (commercial, privacy, licensing). HD-PRH2-2 is the most "askable either way": the fail-closed default (approval required) could be an engineering call, but asking is defensible because it touches the Stage 3D trust boundary. HD-PRH2-1: see F1. The CI question: see F3.

**Overclaim:** none; the plan is headed "PLANNING GATE — NOT AUTHORIZED FOR IMPLEMENTATION" and its verification method is stated plainly.
