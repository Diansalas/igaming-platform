# Architect ruling — K1 R-12 expiry (2026-09-28)

**Reviewer:** `architect`. The orchestrator recorded this ruling.

**Scope:** code review F-4, on K1 head `61faff8`. Read-only.

## Decision

1. **R-12 becomes "no overlapping validity ranges".**
   - No two unrevoked grants for one `(tenant, grantee, capability)` may have overlapping half-open `[valid_from, valid_until)` ranges; a NULL end means unbounded.
   - Expired grants do not block. Queued back-to-back renewals are allowed.
   - **The binding control is DB-level:** the grant INSERT trigger takes a per-key `pg_advisory_xact_lock` and then refuses an overlap (`CG012`). The request and approval triggers pre-check for a legible error.
   - The partial unique index on `revoked_at IS NULL` is **dropped**.
   - `EXCLUDE USING gist` is deferred, because it needs the `btree_gist` extension. It joins the existing PHASE-D-ARCH/SEC-P3-2 (0075/0076) extension decision, and no `CREATE EXTENSION` is to be run.
   - **Option (b), auto-supersede by revoke: REJECTED.** It would record natural expiry as a governance decision, misattribute the actor and mutate history. Expiry stays derived, and no row is written at expiry.
2. **R-14 (new): no backdated or already-expired grant.**
   - At request: `valid_from` defaults to `now()` and must satisfy `now()-5min ≤ valid_from ≤ request expires_at`; `valid_until`, if set, must be `> now()`.
   - At approval: a request whose window has already ended is refused.
   - At INSERT: `valid_from := GREATEST(requested, now())`, and `CHECK (valid_from >= granted_at)`.
3. **The 5-minute value** is a technical clock-skew tolerance: the same value as `PaymentCoverageMaxClockSkew`, and hardcoded like the 24-hour request TTL. Because of the INSERT clamp it grants no authority, so it is not a policy value.
4. **K2/K3** lock the grant **in force at `now()`**, never "the unrevoked grant".

Exact ADR 0099 text is given for R-12, the new R-14, §7.4, §8.2, §10.3–§10.5, §13 (INV-CAP-13/14), §14 (A-20..A-23 plus 4 mutants), §16 and §19. The K1 implementer applies it on the K1 branch.

## Implementer conditions

- **I-1:** make the 0112 edits (the index drop, the lock and overlap check, the clamp and CHECK, R-14 at request and approval).
- **I-2:** in Go, stop defaulting `valid_from` to `time.Now()`, map the new refusals to 409, and fix the stale comment.
- **I-3:** add tests A-20 to A-23 plus the four mutants (drop the overlap check, drop the clamp, drop the approval window check, restore the unique index).
- **I-4:** no `CREATE EXTENSION`.
- **I-5:** a K2/K3 review item: lock the grant in force at `now()`.

## For security

Confirm:
- the 5-minute hardcoded tolerance;
- that the trigger plus advisory lock is an acceptable binding control in place of a unique index.
