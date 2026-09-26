# 16 - Code review: CODE-HYGIENE-10.3-1 (commit a09f88c, merged a3928f8)

Reviewer: `code-reviewer` (independent of the implementer). Date: 2026-09-26.
Scope: `git show a09f88c` (10 files). I read the code itself, not the commit message.

## Verdict: NOT READY (one Medium finding; small rework)

Items 1, 2 and 6 are correct. Items 3 and 5 are adequately justified. Item 4
does **not** do what the commit message, the code comments and the
task-registry row say it does (F-1). The fix is small. Nothing in this
commit affects money movement or tenant isolation.

Checks run (read-only; the main tree was being edited concurrently for F-POOL-1):
- `go test -race -count=1 ./internal/providercred/ ./internal/db/`: ok
- `go test -race -count=1 -tags=integration ./internal/db/` (subset), and
  `-v -run TestWithTenantAndWithTenantSnapshot_SetIdenticalTenantSessionState`: PASS.

I did not run `internal/jurisdiction` or `internal/reconciliation` integration
tests (they need an exclusive DB / full migrate-down).

---

## Findings (most severe first)

### F-1 (Medium; claim accuracy / item 4 not achieved): the 0075 test still breaks at migration 0099

`internal/jurisdiction/migration_0075_integration_test.go`: the three
`MigrateDown(dir, 24)` calls now use `migrationsFromVersion(t, dir, 75)`. But
the two assertions that follow them are still hard-coded:
- `wantDown` (line ~418): exactly 0098..0075, 24 elements.
- `wantDirty` (line ~599): exactly 0098..0076, 23 elements.

**Failure scenario:** someone lands `0099_x.up.sql`/`.down.sql`.
`migrationsFromVersion` returns 25, so `MigrateDown` rolls back
`[99, 98, ..., 75]`. Then `len(rolledBack) != len(wantDown)` (25 vs 24) calls
`t.Fatalf` in `TestMigration0075_DownMigrationCleanThenFailsOnDirtyDatabase`.
That is the same "fails loudly at migration 0099" symptom the registry row
(item 5) asked to fix. The only difference is that it now fails one assertion
later. Only the third call site (`..._RestoresPreMigrationRLSPosture`, which
has no order assertion) is actually fixed.

The new comments say the count "never goes stale as new migrations land
above 0098", and the registry row now marks the item **Done**. Both claims
are false for 2 of the 3 call sites, so this is a no-fake-completion problem
as well as a code problem.

Ordering detection itself is intact. The hard-coded `wantDown`/`wantDirty`
still catch misordering, which is exactly why they go stale.

**Fix (pick one):**
- (a) Recommended: use the actual 0092 precedent (`stagedMigrations0092`).
  Stage a temp migrations dir that holds back every version > 0098. The
  existing constants, `wantDown`/`wantDirty` and the per-migration guard
  reasoning in the comments (for example "0097/0098 refuse only once casino
  evidence exists") then stay valid indefinitely. This also means a future
  0099 whose down migration has its own guard cannot break this test's
  scenario.
- (b) Derive `wantDown`/`wantDirty` from the directory: versions >= 75 (or
  >= 76) sorted descending. This still catches misordering, because
  `MigrateDown`'s actual order is compared to an independently sorted list.
  But it silently assumes every future down migration is reversible in this
  scenario.

The commit message calls the current helper "mirroring
migration_0092_integration_test.go's migrationFileVersion pattern". It
mirrors only the filename parser, not the hold-back that makes the 0092
tests stale-proof.

### F-2 (Low; security-domain, surface to `security`): derived tokens lost their redacting type

`internal/providercred/outbound.go`: `derivedEntry.token` changed from
`secretstore.Secret` to a raw `[]byte` so it can be zeroed. `Secret`
redacts on `String`/`GoString`/`Format`/`LogValue`/`MarshalJSON`, and that
defence-by-type is now gone for derived tokens held in the cache.

Today the practical exposure is small:
- `derivedEntry` is unexported.
- The map/list hold pointers, so `%+v` on the cache prints addresses, not
  bytes.
- `Get` still returns a raw `[]byte`, as before.

**Failure scenario:** a future debug helper or a test failure message
formats a `*derivedEntry` or `derivedEntry` (for example
`t.Logf("%+v", *el.Value.(*derivedEntry))`). That prints the token bytes,
where it previously printed `[REDACTED-SECRET]`.

Cleaner alternative: keep `secretstore.Secret` and add a package-owned
`(*Secret).Wipe()` (zero in place) to `secretstore`. This keeps both the
redaction and the zeroing. That touches `secretstore`, which is near the
F-POOL-1 work, so coordinate. Retention/redaction policy is `security`'s
call. I am not adjudicating it, only flagging it.

### F-3 (Low; test gaps behind "each mutation-killed" claims)

`internal/providercred/outbound_cache_test.go`:
- **Zeroing on eviction is only tested through a direct call to
  `removeElementLocked`.** It is not tested through the bound or rotation
  paths the doc claims. A mutation that makes the rotation loop in `Put` do
  `delete(d.entries, k)` instead of `d.removeElementLocked(el)` survives
  every test:
  - The rotation test only checks `len(cache.entries)`.
  - The bound test uses distinct handles.

  The result would be un-zeroed stale tokens plus orphaned `order` list
  elements that still count toward `maxSize`, so the effective bound shrinks
  and live entries get evicted early. Add assertions on `cache.order.Len()`
  and on zeroed bytes for an entry evicted by rotation and one evicted by the
  bound, captured before the `Put`.
- **No concurrent test exists**, so the `-race` run proves nothing about the
  locking. By reading the code, the locking is correct: every access to
  `entries`/`order` happens under `d.mu`, including the expiry removal in
  `Get`. A small parallel Put/Get test would make the `-race` claim
  meaningful. This is non-blocking.

### F-4 (Low; accuracy): the snapshot begin-error text changed, and the registry says it didn't

`WithTenantSnapshot`'s begin failure used to be
`db: begin repeatable-read tx: %w`. It is now `db: begin tx: %w`, which is
indistinguishable from `WithTenant`'s. The registry row says "same error
text per caller". No code matches on the string (I grepped and found nothing
outside `.claude/worktrees`), so nothing breaks. But the "byte-identical"
claim is not literally true, and operators lose the isolation-level hint in
logs.

Fix either way: pass a label into `withTenantTx`, or correct the wording.
Separately, the doc comment in `tenant_snapshot.go` ("the tenant set_config
below") now points at code that lives in `tenant_rls.go`.

### F-5 (Informational): the parity test comment overclaims

The `TestWithTenantAndWithTenantSnapshot_SetIdenticalTenantSessionState` doc
says that if "another GUC, a statement_timeout, a role switch" were added to
one path only, "this test would catch the divergence". It would not: the
test only reads `app.tenant_id`, RLS visibility on one table, and
`transaction_isolation`.

What actually prevents drift is the shared helper, not the test. The test
does meaningfully prove:
- the GUC value;
- that RLS is enforced on both paths (it ran, and cross-tenant rows were not
  visible, so the test role does not bypass RLS);
- the two isolation levels.

Reword the comment, or compare `current_setting` for the known GUC set
(`app.player_account_id`, `app.principal_id`, `app.platform_admin_principal_id`
are unset) if the stronger claim is wanted.

---

## Verified correct (no finding)

- **Item 1 behaviour.**
  - `WithTenant`: the old code was `p.pool.Begin(ctx)`. pgxpool's `Begin` is
    `BeginTx(ctx, pgx.TxOptions{})`, so the new call is equivalent.
  - The nil-tenant check stays in each public wrapper with its own message.
  - Same `set_config('app.tenant_id', $1, true)`: parameter-bound and
    transaction-local.
  - Same error wrapping for set/commit.
  - Same deferred `Rollback`, which also runs on panic because the defer is
    inside `withTenantTx`, and is a no-op after commit.
  - `fn` errors are returned unwrapped as before.
  - `WithTenantSnapshot` still passes `IsoLevel: pgx.RepeatableRead`, and the
    integration test asserts `repeatable read` vs `read committed`.
- **Item 2 correctness.**
  - The cache is keyed by (tenant, handle, fingerprint). Rotation eviction
    matches on tenant AND handle with a different fingerprint, so it never
    crosses tenants or handles.
  - `Put` copies its input and `Get` returns a fresh copy, so zeroing an
    evicted entry cannot corrupt a token a caller holds. There is no
    use-after-evict.
  - Re-`Put` of the same key zeroes the old owned copy and moves the entry
    to the back.
  - The bound loop trims from the front (FIFO).
  - An out-of-order concurrent `Put` of an older fingerprint after a newer
    one evicts the newer entry. That only causes a cache miss and
    re-derivation. The stale entry cannot be served, because a live handle
    read will not return the old fingerprint.
  - The O(n) scan per `Put` (n <= 4096) is acceptable for a rare derivation
    path.
  - There is still no production caller (grep), consistent with the
    PROV-OUTBOUND-CRED-1 precondition.
- **Item 6.** Migration 0097 declares `reason_class ... CHECK (...)` inline
  and unnamed. It is the only unnamed CHECK on `reason_class`, and the other
  two table CHECKs are named explicitly, so Postgres's
  `casino_callback_rejections_reason_class_check` is deterministic (45 chars,
  under NAMEDATALEN). An exact name match fails loudly with no rows. A future
  DROP + ADD under the same name (the 0097 `mismatch_kind` pattern) keeps
  working.
- **Item 3 (aliases) and item 5 (`NewWithSDKFake`).** The justifications are
  accurate. The aliases are used in `statement_mock.go`'s own signatures. A
  `_test.go`-only symbol cannot be imported by `cmd/platform-api`'s untagged
  test, and a build tag would push that test behind `-tags=integration`. The
  existing AST guard and fail-closed nature stand.

## Required before marking CODE-HYGIENE-10.3-1 item 4 done

1. Fix F-1 using option (a) or (b), and correct the comments and registry row.
2. Recommended in the same pass: add the F-3 assertions and fix the F-4
   wording. F-2 goes to `security` for disposition.
