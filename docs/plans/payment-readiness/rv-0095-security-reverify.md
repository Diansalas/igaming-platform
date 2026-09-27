# RV-0095 — Security re-verification of ADR 0095 (revision 2)

Reviewer: `security`, 2026-09-27, at `HEAD 13b44b5`. Target:
`docs/decisions/0095-provider-io-transaction-boundary-and-payment-contract.md` (revision 2,
ACCEPTED (design), NOT IMPLEMENTED).

**Method.** I checked each S95-C1..C13 condition (§22.4) against the ADR's **design text**:
§2 (INV-IO-14/15), §3.2, §4.3, §4.4, §5.2, §5.4, §6.1–§6.5, §7, §9.1, §10.2–§10.5, §11, §12,
§13.1–§13.3 and §16.2/§16.4. I did not take the §27.2 "Satisfied" column as evidence. I also
checked the text against the live code and schema it relies on:
- `internal/db/tenant_rls.go`: what `WithPlatformAdmin` does and does not set;
- `migrations/0025_create_deposit_intents.up.sql`: the `tenant_staff_scope` USING/WITH CHECK shape;
- ADR 0093 A1: the "request-binding shape" the ADR says it reuses.

**Out of scope.** No code exists yet. This was not a pen test. This review does not approve
PRH-I1, I2 or I5. Each still needs its own `security` code review, and the outbound tripwire
stays in place until then.

## Verdict

**Not every launch-blocking condition is closed in the text.**

- **Closed at design level:** S95-C1 (the High one), C6, C8 and C13's tenant-scoped surface.
- **Open: C5 and C7.**
  - The kill-switch guard does not stop a single actor from re-scoping an engaged row, which has
    the same effect as releasing it (N1).
  - The platform-scope protection depends on scope columns the application writes. It also
    depends on a platform write path that cannot work under the RLS policies §13.2 specifies
    (N2, N3).
- **Open (narrowly): C13.** The platform-principal engage and release routes that C7 needs are
  not in §10.5 (N3).

All three are text fixes, confined to §10.2, §10.4, §10.5, §13.2 and §16.2 item 20. **They must
land before PRH-I1 implements migration 0102 or the kill-switch routes.** They do not block the
parts of PRH-I1 covered by C1, C6 or C8 (§3–§9, §11, migration 0101).

## Per-condition status

| # | Sev / launch | Status | Verified against | Notes |
|---|---|---|---|---|
| **C1** | High, launch-blocking | **CLOSED** (design) | INV-IO-14; §6.1 step 4(a)(b); §4.4 precondition 1; §4.3 T15 CAS `provider_id = $verified_provider`; §6.4 rows 2 and 5; §7.1 deferred re-apply; §5.4 reversal original lookup; §12.3 first paragraph; §6.5; §16.2 item 20; MX15 | P always comes from the route plus the verifying credential, never from the payload (§6.1 preamble). A merchant-reference hit with a different or NULL `provider_id` becomes an `anomaly` receipt with no state change, and T15 is not taken. **Race check:** the resolution is read-only before locking. Step 7 then re-reads under the parent and attempt locks and re-applies the preconditions. `provider_id` is NULL→value-once (the §13.1 trigger), and T2 can set it only while it is NULL. So a resolved binding cannot change between resolution and CAS. Sync and `QueryStatus` evidence comes from a call made to `attempt.provider_id` with that provider's credential, so it is bound by construction. |
| C2 | Medium | **CLOSED** (design) | §6.1 step 5; §6.2 cap and CHECK rows; §6.4 last row; §7.3; §13 intro; §13.1 partial index | — |
| C3 | Medium | **CLOSED** (design) | §4.2; §6.4 row 2; §7.1; §13.1 `resolution`, retention comment; §10.1 `RetryWindow`; MX18 | See L3 (`received_at` is a DEFAULT, not trigger-forced). |
| C4 | Low | **CLOSED** | §6.2; §16.2 item 20 | — |
| **C5** | Medium, launch-blocking | **OPEN** | §10.2 points 1–3; §13.2 guard comment; §16.2 item 20; MX17 | Points (1)–(3) of the condition are written in the text. The guard is still bypassable by a single actor through columns it does not freeze (**N1**). The "same transaction" binding also has no mechanism, and it does not follow the A1 shape the ADR cites (**N4**). |
| **C6** | Medium, launch-blocking | **CLOSED** (design) | §10.3 first bullet; INV-IO-15; §4.3 T1+T2/T1p/T2/T12; §5.1 flow; §5.2 flow step 5; §16.2 items 12 and 20; MX2 | The predicate sits inside the CAS statement, correlated on `payment_attempts.tenant_id`, under the same RLS context. A read error aborts. There is no cache. For the two INSERT forms (T1+T2, T1p) the correlation written in §10.3 is not literally expressible. They are still fail-closed, because the attempt INSERT needs the `tenant_staff_scope` WITH CHECK under the same GUCs the switch policy uses. That coupling is not stated anywhere (L1). |
| **C7** | Medium (caller-listed as launch-relevant) | **OPEN** | §10.2 `engaged_by_scope`; §10.4 last bullets; §13.2; §16.2 item 20 | The DB check trusts app-written `*_by_scope` columns, and `engaged_by_scope` is mutable (**N2**). The platform principal cannot write the rows under the specified policies, and no platform route exists (**N3**). |
| **C8** | Medium, launch-blocking | **CLOSED** (design) | (a) §3.2 step 8, §11 "No leakage"; (b) §3.2 step 4, §9.1; (c) §11 "No credential held by an adapter", §9.1 renderers; §16.2 items 15 and 20 | All three parts are in the binding text, and they also cover casino, KYC and statement `Fetch` (§3.2 last paragraph; §9.5). §16.2 item 15 still describes the **old** name- and type-based reflection test and omits the static test. Item 20 and §11 carry the correct one (L2). |
| C9 | Low | **CLOSED** | §3.2 step 3; §7.2 step 6; §11 "Tenant and provider isolation" and "Observability" | — |
| C10 | Low | **CLOSED** | §9.3; §13.1 CHECKs; §16.2 item 20; §20 VENDOR-INTAKE-REF-PII-1 | The allow-list is enforced by the application. The DB enforces only the 64-byte bound. That is acceptable. |
| C11 | Low | **CLOSED** | §9.5; §12.1 step 1; §13.3 CHECKs | — |
| C12 | Low | **CLOSED** | §10.1 registration refusals; §8 `NotProcessed` row | — |
| **C13** | Medium, launch-blocking | **OPEN (narrow)** | §10.5; §4.8 M3; §13.1 trigger; §16.2 item 20 | The tenant-scoped surface is fully specified: staff API only, a route-table test, no tenant id from the path or body, RLS with 404 for another tenant, audit with IP/UA/reason, T17 never inline, and the M3 CAS in both SQL and trigger. The platform-principal engage and release routes that C7 needs are missing (N3). |

## New findings

### N1 — Medium, launch-blocking (part of C5): an engaged switch can be re-scoped by a single actor

**Where:** §10.2 and the §13.2 `payment_kill_switches_guard` comment.

The guard constrains only three things: DELETE, `engaged` true→false, and `version`
monotonicity. `tenant_id`, `provider_scope`, `operation_scope` and `engaged_by_scope` are not
frozen.

**Failure scenario:**
1. Tenant security engages `(T, 'psp-a', '*')` after a credential compromise.
2. One staff principal, through the engage path or any other UPDATE path, runs
   `UPDATE … SET provider_scope = 'psp-a-x', version = version + 1`.
3. `engaged` stays true, so the guard passes.
4. §10.3's `k.provider_scope IN ('*', $provider)` no longer matches `psp-a`, so T2, T1p and T12
   resume against the compromised provider.

A narrowing of `operation_scope` from `'*'` to `'deposit'` has the same effect on payouts.

This is exactly the single-actor release C5 exists to prevent. The guard just doesn't cover it.

**Fix (text):** the guard makes `id`, `tenant_id`, `provider_scope` and `operation_scope`
immutable. A scope change is a new row. §16.2 item 20 and MX17 add "a direct UPDATE of a scope
column on an engaged row is refused".

### N2 — Medium, launch-blocking (part of C7): platform-scope protection trusts app-written, mutable columns

**Where:** §13.2 and §10.4.

**(a) The scope columns are application-asserted.** `engaged_by_scope`, `requested_by_scope`
and `approved_by_scope` are written by the application. `requested_by_scope` and
`approved_by_scope` have no CHECK at all. The guard's "platform ⇒ both platform" test compares
these asserted values. So the DB backstop is no stronger than the handler's own check. A tenant
connection can write `'platform'`.

**(b) `engaged_by_scope` is mutable.** A tenant principal can engage, or re-engage (which bumps
`version`), an already platform-engaged row. If the engage handler writes
`engaged_by_scope = <actor scope>`, the platform flag becomes `'tenant'`. Tenant four-eyes can
then release a platform containment.

**Fix (text):**
- `engaged_by_scope` may not go `platform→tenant` while `engaged`.
- A `'platform'` value in any scope column is accepted only when the tx carries
  `app.platform_admin_principal_id`, and that principal resolves to a `staff_users` row with
  `tenant_id IS NULL`. This is the migration 0044 rule `WithPlatformAdmin` already documents.
- `requested_by`, `approved_by` and `changed_by` are forced from the session principal GUC, not
  from client values (the ADR 0093 A1/B5 pattern). The `approved_by <> requested_by` CHECK then
  binds real identities.
- Add CHECK enums on `requested_by_scope` and `approved_by_scope`.
- Add §16.2 item 20 tests:
  - a tenant re-engage of a platform-engaged row does not downgrade it;
  - a tenant connection cannot write `'platform'`.

### N3 — Medium, launch-blocking (C7 + C13): the platform engage/release path is unbuildable as specified and has no route spec

**Where:** §10.2 last paragraph, §10.4 and §10.5.

§10.2 says a platform principal engages each tenant's `'*'` row "through the existing
`WithPlatformAdmin` pattern". But `WithPlatformAdmin` (`internal/db/tenant_rls.go:112`)
**never sets `app.tenant_id`**, and §13.2 defines only tenant policies on both tables. A
platform tx therefore sees and writes zero rows. The platform release that C7 requires cannot be
performed at all.

§10.5 also lists no platform route, no permission, and no rule for how the **target tenant** is
chosen. A platform route necessarily takes the tenant from the path or body, which contradicts
§10.5's blanket rule.

**Implementers will improvise:**
- either set `app.tenant_id` from a request body inside a "platform" handler, which is
  indistinguishable in the DB from tenant staff and makes N2 exploitable;
- or add an unreviewed policy.

**Fix (text):**
- Specify a platform policy pair on both 0102 tables, gated on
  `app.platform_admin_principal_id` set, `app.tenant_id` and `app.player_account_id` unset, and a
  platform `staff_users` row. It covers SELECT, INSERT and UPDATE only, with no DELETE.
- Add platform-admin routes to §10.5:
  - on the platform admin surface, not the tenant staff API;
  - a distinct permission (e.g. `platform_payments_kill_switch:engage|release`);
  - the target tenant taken from the path, and valid **only** for platform principals;
  - audit that carries both the actor and the target tenant.
- Add tests:
  - a tenant principal gets 403/404 on those routes;
  - a platform principal cannot act on the tenant API.

### N4 — Medium (C5 precision): the "same transaction" release binding has no mechanism

**Where:** §13.2.

The guard says "in the same transaction, a release request for this row moved open→approved".
The schema gives the trigger no way to prove either "same transaction" or "this request". There
is no `release_request_id` on the switch row, and no `decided_txid`.

ADR 0093 A1, which the ADR cites, binds through an explicit, immutable FK on the state row
(`activation_request_id`, B3/B4), not through a search. A search-based trigger could accept a
request approved in an earlier tx, or a different request for the same switch.

**Fix (text):**
- Add `release_request_id UUID NULL` on `payment_kill_switches`, with a composite FK
  `(tenant_id, release_request_id)`.
- On true→false, NEW must reference a request with `kill_switch_id = id`, `status='approved'`,
  `expected_version = OLD.version`, a distinct approver and an unexpired `expires_at`, and whose
  `decided_at`/xmin is this transaction (`decided_at` forced by trigger).
- One request can release at most once: a UNIQUE on `release_request_id`.
- Force `expires_at ≤ created_at + 24 h` by trigger rather than trusting the client (currently
  `RECOMMENDATION` text only).

### L1 — Low (C6 note): the INSERT-form fail-closed argument is implicit

The §10.3 predicate text (`k.tenant_id = payment_attempts.tenant_id`) fits the UPDATE forms (T2,
T12). The T1+T2 and T1p INSERT … SELECT forms must correlate on the inserted `tenant_id` value.
In those forms, fail-closed rests on the `payment_attempts` WITH CHECK sharing the GUC predicate
(tenant set, player unset) with the switch policy.

If a player-scoped INSERT policy is ever added to `payment_attempts`, a player-context
INSERT would see zero switches (the switch policy requires the player GUC unset) and claim
anyway.

**Fix:** state this coupling in §10.3 or §13. Add a test: running T1+T2 under a player-scoped
context fails, and makes no call.

### L2 — Low (C8 note): §16.2 item 15 describes the superseded reflection test

Item 15 still describes the name- and type-based check, which is the weaker test C8(c)
replaced. It also omits the static package-var/import test and a `Domain`-mismatch case. Item
15 should also test the gate check and the adapter check **each alone**: a non-checking fake
adapter for the gate, and a bypassed gate for the adapter. Otherwise "neither alone is
load-bearing" is not proven.

**Fix:** align item 15 with §11.

### L3 — Low (C3 note): `received_at` is only `DEFAULT now()`

Explicit application values are accepted.

**Fix:** force `received_at` (and the one-shot `resolved_at`) by trigger, so the C3
"both from the DB clock" guarantee is structural.

### L4 — Low: cross-tenant FK on `payment_kill_switch_release_requests.kill_switch_id`

FK checks ignore RLS. A tenant A request row can therefore reference tenant B's switch id. It
cannot release B's switch, because B's row is invisible to A's UPDATE. It is, however, an
existence oracle and a data-integrity gap.

**Fix:** use a composite FK `(tenant_id, kill_switch_id)` → `(tenant_id, id)`.

## Launch relevance

- N1, N2 and N3 block the first non-MOCK payments adapter, together with the existing
  launch-blocking set.
- N4 must be closed before migration 0102 merges.
- L1–L4 are implementing-review items for PRH-I1.

None of this is a claim that anything is secure or launch-ready. Production launch
authorization remains the human's decision.

---

## Revision 3 addendum

Reviewer: `security`, 2026-09-27, against ADR 0095 revision 3 (commit `3c88e13`). I checked the
design text of §10.2, §10.2.1, §10.2.2, §10.3, §10.4, §10.5, §13.1 (receipt columns), §13.2,
§16.2 items 15 and 20, §16.4 MX17–MX21 and §27.8. As before, I did not take the §27.8 "Where
satisfied" column as evidence. No code exists, so this is a design-level review only. PRH-I1,
I2 and I5 still each need their own `security` code review, and the outbound tripwire stays in
place.

### Status of the rev-2 findings

| Finding | Status | Evidence in rev 3 |
|---|---|---|
| N1 (re-scope an engaged row) | **CLOSED** | §10.2.2 point 2 makes `id`, `tenant_id`, `provider_scope` and `operation_scope` immutable; §13.2 guard comment; item 20 direct-SQL test; MX19. |
| N2 (app-written, mutable scope columns) | **CLOSED** | §10.2.1: actor and scope are forced from session GUCs and verified against `staff_users` (tenant: `tenant_id = app.tenant_id`; platform: `tenant_id IS NULL`, with tenant and player GUCs unset). Any other shape raises. CHECK enums are on every scope column. §10.2.2 point 5: never `platform→tenant`, and no tenant UPDATE of a platform-engaged row. Item 20 tests; MX20. |
| N3 (platform path unbuildable, no routes) | **CLOSED** | §10.2.1 adds a platform RLS family (platform GUC set, tenant and player GUCs unset; SELECT/INSERT/UPDATE; no DELETE or `FOR ALL`). This family is unreachable from the tenant-context claim path. §10.5 adds platform routes with distinct `platform_payments_kill_switch:*` permissions, the path-tenant rule restricted to platform principals, cross-surface 403s, and audit carrying both actor and target tenant. Item 20 tests. |
| N4 (release binding had no mechanism) | **CLOSED, except for N5 below** | §10.2.2 point 6: `release_request_id` with composite FK and `UNIQUE`; `kill_switch_id = id`; `expected_version = OLD.version`; not expired; `decided_txid = txid_current()`. Approve and release happen in one tx. Item 20; MX21. |
| L1 | **CLOSED** | §10.3 states the coupling as binding: no other `payment_attempts` write policy without `security` re-review. Item 20 tests player and platform contexts. |
| L2 | **CLOSED** | Item 15 now includes the recursive test, the static test, a `Domain` mismatch, and the gate-alone and adapter-alone cases. |
| L3 | **CLOSED** | §13.1: `received_at` and `resolved_at` are forced by trigger. Item 20 tests it. |
| L4 | **CLOSED** | §13.2 composite FK `(tenant_id, kill_switch_id)` → `(tenant_id, id)`. Item 20 tests it. |

### New findings

#### N5 — Medium, launch-blocking (C5 precision): release-request column write moments are unspecified, which permits a single-actor release

**Where:** §10.2.1 first paragraph, §10.2.2 "Release-request guard trigger", and the §13.2
session-actor comment ("overwrites every `*_by` / `*_by_scope` column with it").

The text forces `requested_by*` and `approved_by*` "from the session" on INSERT/UPDATE. It does
**not** say which column is forced at which transition. The request guard also does not freeze
the request's other columns: it constrains only `status`, `created_at`, `expires_at`,
`decided_at` and `decided_txid`.

That leaves two readings, and both are defects:

- **Literal reading.** `requested_by` is overwritten with the approver on the approve UPDATE.
  `approved_by = requested_by` then always holds, the CHECK refuses, and no release can ever
  succeed. This fails closed, but the positive release tests in item 20 will fail, and that
  pushes the implementer to "fix" it ad hoc.
- **Likely "fix" reading.** `requested_by*` is forced only at INSERT and `approved_by*` only at
  approval, but `requested_by*` is not frozen afterwards. Then, in a single session:
  1. Actor A creates a request (`requested_by = A`).
  2. In the approve transaction, A sends
     `UPDATE … SET status='approved', requested_by = <any other staff uuid>`.
  3. `approved_by` is forced to A. The CHECK `approved_by IS DISTINCT FROM requested_by` passes.
  4. The §10.2.2 point 6 guard sees a "distinct" approver, and A alone releases the switch.

  The same approach works on a platform-engaged row: A can set `requested_by_scope`
  indirectly by choosing any platform staff UUID.

Also unfrozen: `kill_switch_id`, `expected_version`, `reason_code` and `tenant_id` can be edited
on an `open` request after the requester created it. The approver then approves something the
requester never asked for. Terminal (`approved`, `cancelled`, `expired`) rows are not stated to
be immutable either.

**Fix (text, §10.2.2 request guard, §13.2 comment, item 20, one new mutation):**
- `id`, `tenant_id`, `kill_switch_id`, `expected_version`, `reason_code`, `requested_by`,
  `requested_by_scope`, `created_at` and `expires_at` are set at INSERT (the actor and time
  columns are forced) and are immutable afterwards.
- `approved_by`, `approved_by_scope`, `decided_at` and `decided_txid` are forced at
  `open→approved` only, and are NULL and unwritable otherwise.
- Every column of a row in a terminal status is immutable.
- Item 20 test: an approve UPDATE that also sets `requested_by`, `requested_by_scope` or
  `expected_version` is refused, or those values are ignored; either way a single actor cannot
  release.
- New mutation MX25: allow a `requested_by` UPDATE. It must be caught by item 20.

#### L5 — Low: tenant sessions can obstruct platform release, and INSERT can pre-consume a `release_request_id`

Both issues fail in the safe direction: the switch stays engaged. They are reliability gaps,
not bypasses.

**(a) A tenant can block the platform's release request.** §10.5 refuses release requests on
platform-engaged rows only in the application. At the DB level, a tenant session may still
INSERT an `open` request for a platform-engaged switch. The partial unique index
(`kill_switch_id WHERE status='open'`) then blocks the platform's own request until someone
cancels the tenant's request.

**Fix:** the request guard refuses a tenant-scope INSERT when the switch has
`engaged_by_scope = 'platform'`.

**(b) INSERT can pre-consume a `release_request_id`.** §10.2.2 point 7 governs
`release_request_id` on transitions but not on INSERT. A tenant can INSERT a disengaged switch
row with `release_request_id = <another switch's open request>`. `UNIQUE (release_request_id)`
then makes that switch's legitimate release fail.

**Fix:** force `release_request_id := NULL` on every INSERT.

### Per-condition status (revision 3)

| Condition | Status | Remaining |
|---|---|---|
| **S95-C5** (launch-blocking) | **OPEN (narrow, text-only)** | N5. Everything else in C5 (N1, N4, DELETE, monotonic version, same-tx binding) is closed in the text. |
| **S95-C7** | **CLOSED** (design) | None. Platform scope is derived from the session and verified against `staff_users`, cannot be downgraded, and the platform path is buildable. The N5 fix also protects the platform-engaged case. |
| **S95-C13** (launch-blocking) | **CLOSED** (design) | None. Both surfaces are specified with permissions, the target-tenant rule, cross-surface refusals, audit and OpenAPI conformance. |
| C1, C2, C3, C4, C6, C8, C9, C10, C11, C12 | Unchanged: **CLOSED** (design) | L1–L3 notes are now also written in. |

### Overall launch-blocking status of the ADR 0095 design

**Still launch-blocking, on N5 alone.** It is a one-paragraph text fix in §10.2.2, §13.2 and
§16.2 item 20, plus MX25.

- **Must land before:** migration 0102 or the kill-switch routes are implemented. It does not
  block the parts of PRH-I1 that do not touch 0102: §3–§9, §11, and migration 0101.
- **After it lands:** every S95 condition is closed at design level. Security can confirm that
  by checking only the N5 text; a full re-review is not needed.
- **L5:** an implementing-review item for PRH-I1. Fold it into the same edit if convenient.

The first non-MOCK payments adapter stays blocked until all of the following hold:
- N5 is written in;
- PRH-I1, I2 and I5 pass `security` code review against this design;
- the item 20 tests and the MX17–MX21 (+MX25) mutations are shown passing and killed.

This addendum does not state that anything is secure or launch-ready. Production launch
authorization remains the human's decision.
