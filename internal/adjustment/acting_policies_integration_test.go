//go:build integration

package adjustment

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
)

// Security K1 re-check (mandatory for K2): the "drop the grant function
// from one acting policy" mutant must be killed for EVERY acting policy
// migration 0113 adds. Each probe below isolates ONE policy with the
// layered technique security endorsed (K1-C3, k1-security-recheck.md): on
// a scratch database, inside a transaction that is always rolled back, the
// table's user triggers are disabled and every OTHER acting policy on the
// same table is replaced by its GUC-only form - so the ONLY thing that can
// refuse the probe is the target policy's own
// (SELECT financial_acting_session_valid()) arm. The probe then runs in an
// acting-SHAPED session whose principal's grant for X has been revoked
// (valid GUC shape, invalid grant - exactly the TM-3 residual shape) and
// must be denied; the same probe in a VALID acting session must be allowed
// (non-vacuity).
type actingProbe struct {
	table, policy string
	// run executes the probe and reports whether it was ALLOWED.
	run func(ctx context.Context, tx pgx.Tx, f *probeFixture) (bool, error)
}

type probeFixture struct {
	w                *world
	revoked          staffMember
	executedRequest  Request
	pendingRequest   Request
	someLedgerTx     uuid.UUID
	playerCashAcct   uuid.UUID
	freshAsset       string
	licenceID        uuid.UUID
	freshAccountNoPj uuid.UUID
}

func countAllowed(sql string, args ...func(f *probeFixture) any) func(context.Context, pgx.Tx, *probeFixture) (bool, error) {
	return func(ctx context.Context, tx pgx.Tx, f *probeFixture) (bool, error) {
		vals := make([]any, len(args))
		for i, a := range args {
			vals[i] = a(f)
		}
		var n int
		if err := tx.QueryRow(ctx, sql, vals...).Scan(&n); err != nil {
			return false, err
		}
		return n > 0, nil
	}
}

func tenantArg(f *probeFixture) any { return f.w.Tenant }

// execAllowed runs a write in a savepoint: allowed iff it succeeded and
// affected at least one row; a 42501 RLS violation (or 0 rows) is "denied".
func execAllowed(sql string, args ...func(f *probeFixture) any) func(context.Context, pgx.Tx, *probeFixture) (bool, error) {
	return func(ctx context.Context, tx pgx.Tx, f *probeFixture) (bool, error) {
		vals := make([]any, len(args))
		for i, a := range args {
			vals[i] = a(f)
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT probe`); err != nil {
			return false, err
		}
		tag, err := tx.Exec(ctx, sql, vals...)
		if _, rbErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT probe`); rbErr != nil {
			return false, rbErr
		}
		if err != nil {
			if pgCode(err) == "42501" {
				return false, nil
			}
			return false, err
		}
		return tag.RowsAffected() > 0, nil
	}
}

var actingProbes = []actingProbe{
	{"player_accounts", "acting_read", countAllowed(`SELECT count(*) FROM player_accounts WHERE tenant_id = $1`, tenantArg)},
	{"wallets", "acting_read", countAllowed(`SELECT count(*) FROM wallets WHERE tenant_id = $1`, tenantArg)},
	{"ledger_accounts", "acting_read", countAllowed(`SELECT count(*) FROM ledger_accounts WHERE tenant_id = $1`, tenantArg)},
	{"ledger_accounts", "acting_insert", execAllowed(`INSERT INTO ledger_accounts (tenant_id, account_type, asset_code, status) VALUES ($1, 'manual_adjustment', $2, 'active')`,
		tenantArg, func(f *probeFixture) any { return f.freshAsset })},
	{"ledger_transactions", "acting_read", countAllowed(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenantArg)},
	{"ledger_transactions", "acting_insert", execAllowed(`INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, correlation_id, reason_code)
		VALUES ($1, 'manual_adjustment', 'probe:' || gen_random_uuid(), gen_random_uuid(), 'probe')`, tenantArg)},
	{"ledger_transactions", "acting_lock", countAllowed(`SELECT count(*) FROM (SELECT id FROM ledger_transactions WHERE id = $1 FOR UPDATE) x`,
		func(f *probeFixture) any { return f.someLedgerTx })},
	{"ledger_entries", "acting_read", countAllowed(`SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`, tenantArg)},
	{"ledger_entries", "acting_insert", execAllowed(`INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
		VALUES ($1, $2, $3, $4, 'credit', 1)`, func(f *probeFixture) any { return f.someLedgerTx }, func(f *probeFixture) any { return f.playerCashAcct },
		tenantArg, func(f *probeFixture) any { return f.w.Asset })},
	{"wallet_balance_projection", "acting_read", countAllowed(`SELECT count(*) FROM wallet_balance_projection WHERE tenant_id = $1`, tenantArg)},
	{"wallet_balance_projection", "acting_insert", execAllowed(`INSERT INTO wallet_balance_projection (ledger_account_id, tenant_id, asset_code, account_type, debit_total, credit_total)
		VALUES ($1, $2, $3, 'manual_adjustment', 0, 0)`, func(f *probeFixture) any { return f.freshAccountNoPj }, tenantArg, func(f *probeFixture) any { return f.freshAsset })},
	{"wallet_balance_projection", "acting_update", execAllowed(`UPDATE wallet_balance_projection SET updated_at = updated_at WHERE ledger_account_id = $1`,
		func(f *probeFixture) any { return f.playerCashAcct })},
	{"payment_attempts", "acting_read", countAllowed(`SELECT count(*) FROM payment_attempts WHERE tenant_id = $1`, tenantArg)},
	{"deposit_intents", "acting_read", countAllowed(`SELECT count(*) FROM deposit_intents WHERE tenant_id = $1`, tenantArg)},
	{"licences", "acting_read_own_licence", countAllowed(`SELECT count(*) FROM licences WHERE id = $1`, func(f *probeFixture) any { return f.licenceID })},
	{"asset_authorizations", "acting_read", countAllowed(`SELECT count(*) FROM asset_authorizations WHERE tenant_id = $1`, tenantArg)},
	{"staff_capability_grant_requests", "acting_read", countAllowed(`SELECT count(*) FROM staff_capability_grant_requests WHERE tenant_id = $1`, tenantArg)},
	{"financial_approval_policies", "acting_read", countAllowed(`SELECT count(*) FROM financial_approval_policies WHERE tenant_id = $1 OR tenant_id IS NULL`, tenantArg)},
	{"tenant_financial_policy_profiles", "acting_read", countAllowed(`SELECT count(*) FROM tenant_financial_policy_profiles WHERE tenant_id = $1`, tenantArg)},
	{"ledger_adjustment_requests", "acting_read", countAllowed(`SELECT count(*) FROM ledger_adjustment_requests WHERE tenant_id = $1`, tenantArg)},
	{"ledger_adjustment_requests", "acting_insert", execAllowed(`INSERT INTO ledger_adjustment_requests (tenant_id, wallet_id, player_account_id, brand_id, asset_code, direction, amount,
			reason_code, note_hash, payload_hash, initiated_by, initiated_by_scope, initiated_by_person_id, tenant_status_at_submission, required_at_submission,
			contributing_policy_ids, expires_at)
		SELECT tenant_id, wallet_id, player_account_id, brand_id, asset_code, direction, amount, reason_code, note_hash, payload_hash, initiated_by,
			initiated_by_scope, initiated_by_person_id, tenant_status_at_submission, required_at_submission, contributing_policy_ids, expires_at
		  FROM (VALUES (1)) v, LATERAL (SELECT $1::uuid AS tenant_id, $2::uuid AS wallet_id, $3::uuid AS player_account_id, $4::uuid AS brand_id,
			$5::text AS asset_code, 'credit_player'::text AS direction, 1::numeric AS amount, 'operational_error_correction'::text AS reason_code,
			repeat('a', 64) AS note_hash, repeat('b', 64) AS payload_hash, $6::uuid AS initiated_by, 'platform_acting'::text AS initiated_by_scope,
			$6::uuid AS initiated_by_person_id, 'active'::text AS tenant_status_at_submission, 1 AS required_at_submission,
			'{}'::uuid[] AS contributing_policy_ids, now() + interval '1 hour' AS expires_at) x`,
		tenantArg, func(f *probeFixture) any { return f.w.Wallet }, func(f *probeFixture) any { return f.w.Player }, func(f *probeFixture) any { return f.w.Brand },
		func(f *probeFixture) any { return f.w.Asset }, func(f *probeFixture) any { return f.revoked.ID })},
	{"ledger_adjustment_requests", "acting_update", execAllowed(`UPDATE ledger_adjustment_requests SET closed_at = closed_at WHERE id = $1`,
		func(f *probeFixture) any { return f.pendingRequest.ID })},
	{"ledger_adjustment_approvals", "acting_read", countAllowed(`SELECT count(*) FROM ledger_adjustment_approvals WHERE tenant_id = $1`, tenantArg)},
	{"ledger_adjustment_approvals", "acting_insert", execAllowed(`INSERT INTO ledger_adjustment_approvals (tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope,
			decided_by_person_id, decided_txid, reason_code) VALUES ($1, $2, 'reject', $3, $4, 'platform_acting', $4, 0, 'probe')`,
		tenantArg, func(f *probeFixture) any { return f.pendingRequest.ID }, func(f *probeFixture) any { return f.pendingRequest.PayloadHash },
		func(f *probeFixture) any { return f.revoked.ID })},
}

// guOnly is the GUC-only form an OTHER acting policy is widened to while a
// probe isolates its target.
func gucOnly(table string) string {
	if table == "licences" || table == "financial_approval_policies" {
		return "true"
	}
	return "tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid"
}

func TestActingPolicies_EveryK2ActingPolicyRequiresTheGrantFunction(t *testing.T) {
	pool := scratchPoolThrough(t, "k2actpol_", 113)
	ctx := context.Background()
	w := newWorldOn(t, pool, worldOpts{base: 1, authorizedAsset: true})
	w.fund(3_000)
	f := &probeFixture{w: w}

	// Populate every table the probes read.
	r, err := w.submit(w.Acting, w.credit(100, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil || !out.Executed {
		t.Fatalf("execute: %v", err)
	}
	f.executedRequest = out.Request
	f.someLedgerTx = *out.Request.LedgerTransactionID
	if f.pendingRequest, err = w.submit(w.F1, w.credit(50, ReasonOperationalErrorCorrection)); err != nil {
		t.Fatal(err)
	}
	pid, ref := w.openExposure()
	w.tombstoneFor(pid, ref)
	jur := w.newJurisdiction()
	w.assignLicence(jur)
	ta2 := w.staff(w.Tenant, "tenant_admin")
	w.approvePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelTenant, BaseRequiredApprovals: intPtr(1)}, w.TenantAdmin, ta2)
	pc, err := w.proposePolicy(PolicyChangeInput{ChangeKind: ChangeKindProfileAssignment, TenantID: &w.Tenant, ProfileCode: strPtr("k2-probe-profile")}, w.AdminA)
	if err != nil {
		t.Fatalf("profile assignment: %v", err)
	}
	if err := w.decidePolicy(pc, w.AdminB, DecisionApprove); err != nil {
		t.Fatalf("approve profile assignment: %v", err)
	}
	f.revoked = w.staff(uuid.Nil, "platform_admin")
	w.grantActing(f.revoked, capability.CapabilityLedgerAdjustmentApprove)
	w.revokeGrant(f.revoked.ID, capability.CapabilityLedgerAdjustmentApprove)
	other := newWorldOn(t, pool, worldOpts{})
	f.freshAsset = other.Asset
	if err := pool.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE wallet_id = $1 AND account_type = 'player_cash'`, w.Wallet).Scan(&f.playerCashAcct); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, w.Tenant).Scan(&f.licenceID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO ledger_accounts (tenant_id, account_type, asset_code, status) VALUES ($1, 'manual_adjustment', $2, 'active') RETURNING id`,
			w.Tenant, f.freshAsset+"").Scan(&f.freshAccountNoPj)
	}); err != nil {
		t.Fatalf("fixture reads: %v", err)
	}
	// freshAsset now has an account (for the projection probe); the
	// ledger_accounts insert probe needs an asset with NO account yet.
	third := newWorldOn(t, pool, worldOpts{})
	accountInsertAsset := third.Asset

	var failures []string
	for _, p := range actingProbes {
		for _, valid := range []bool{true, false} {
			principal := f.revoked.ID
			if valid {
				principal = w.Acting.ID
			}
			allowed, err := runIsolatedProbe(ctx, pool.Raw(), p, f, principal, accountInsertAsset)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s.%s (valid=%v): %v", p.table, p.policy, valid, err))
				continue
			}
			if valid && !allowed {
				failures = append(failures, fmt.Sprintf("%s.%s: VALID acting session was denied (probe is vacuous)", p.table, p.policy))
			}
			if !valid && allowed {
				failures = append(failures, fmt.Sprintf("%s.%s: acting session WITHOUT an in-force grant was allowed - the grant function is missing from this policy", p.table, p.policy))
			}
		}
	}
	if len(failures) > 0 {
		t.Fatalf("acting policy probes:\n  %s", strings.Join(failures, "\n  "))
	}
}

// runIsolatedProbe runs p in an always-rolled-back transaction on the
// scratch pool, with p's table isolated as described above.
func runIsolatedProbe(ctx context.Context, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, p actingProbe, f *probeFixture, principal uuid.UUID, accountInsertAsset string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE `+p.table+` DISABLE TRIGGER USER`); err != nil {
		return false, err
	}
	rows, err := tx.Query(ctx, `SELECT policyname, cmd FROM pg_policies WHERE tablename = $1 AND permissive = 'PERMISSIVE' AND policyname LIKE 'acting%' AND policyname <> $2`, p.table, p.policy)
	if err != nil {
		return false, err
	}
	type pol struct{ name, cmd string }
	var others []pol
	for rows.Next() {
		var x pol
		if err := rows.Scan(&x.name, &x.cmd); err != nil {
			rows.Close()
			return false, err
		}
		others = append(others, x)
	}
	rows.Close()
	for _, o := range others {
		expr := gucOnly(p.table)
		var stmt string
		switch o.cmd {
		case "SELECT":
			stmt = fmt.Sprintf(`ALTER POLICY %s ON %s USING (%s)`, o.name, p.table, expr)
		case "INSERT":
			stmt = fmt.Sprintf(`ALTER POLICY %s ON %s WITH CHECK (%s)`, o.name, p.table, expr)
		default:
			stmt = fmt.Sprintf(`ALTER POLICY %s ON %s USING (%s) WITH CHECK (%s)`, o.name, p.table, expr, expr)
		}
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.acting_platform_principal_id', $1, true)`, principal.String()); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.acting_tenant_id', $1, true)`, f.w.Tenant.String()); err != nil {
		return false, err
	}
	saved := f.freshAsset
	if p.table == "ledger_accounts" && p.policy == "acting_insert" {
		f.freshAsset = accountInsertAsset
	}
	defer func() { f.freshAsset = saved }()
	return p.run(ctx, tx, f)
}
