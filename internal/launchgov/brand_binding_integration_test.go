//go:build integration

// ADR 0118 (BRAND-REHOME-HOLD-1), migration 0136: the player brand binding is immutable. Behaviour against
// the shared test database at head, as igaming_runtime (non-superuser, non-BYPASSRLS, asserted by newWorld)
// unless a test says owner:
//
//   - T1 the adversarial re-home CTE (security C2 shape) in both nesting orders is refused with class BR
//     and changes nothing;
//   - T2 the motivating scenario: a stranded withdrawal hold (no brand-pinned child) cannot be moved into
//     a brand closed after LA025 passed, nor out of its brand; LA025 arithmetic and refusal unchanged;
//     a player with a TERMINAL withdrawal request was already pinned by the child FK (23503 with the 0136
//     triggers disabled, i.e. the 0135 behaviour) and is refused BR with them;
//   - T3 every pinned column of every table, alone, raises its own code (BR001 / BR002 / BR003);
//   - T4 the ADR 0118 s1.1 shapes (wallet re-pointed to another player / asset, KYC re-pointed);
//   - T5 no-op and every legitimate update path still commits;
//   - T7 the table owner is bound while the triggers are enabled.
//
// The tests live here (launchgov) because the world fixtures (closed brands through the governed path,
// stranded holds, LA025 counts) are this package's. The schema catalog (T8), pre-flight (T9), up/down/up
// (T10) and the 0135-vs-0136 hole demonstration are in internal/tenant/migration_0136_integration_test.go;
// the static writer guard (T11) is internal/identity/brand_binding_static_test.go. A skip is NOT evidence.
package launchgov_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// rehomeCTE is the security C2 statement: player first, wallet in the outer statement.
const rehomeCTE = `WITH pa AS (UPDATE player_accounts SET brand_id = $1 WHERE id = $2 RETURNING id)
	UPDATE wallets SET brand_id = $1 WHERE player_account_id = $2`

// rehomeCTEReverse is the reverse nesting: wallet first, player in the outer statement.
const rehomeCTEReverse = `WITH wl AS (UPDATE wallets SET brand_id = $1 WHERE player_account_id = $2 RETURNING id)
	UPDATE player_accounts SET brand_id = $1 WHERE id = $2`

// bindingState is everything 0136 pins, read by the OWNER under the tenant GUC (not a blind read).
type bindingState struct {
	player string // id|tenant|brand|created_at
	wallet string // id|tenant|brand|player|asset|created_at (all wallets of the player, ordered)
	kyc    string // tenant|brand|player of every verification of the player, ordered
}

func (w *world) binding(tenantID, player uuid.UUID) bindingState {
	w.t.Helper()
	var s bindingState
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT concat_ws('|', id, tenant_id, brand_id, created_at) FROM player_accounts WHERE id = $1`, player).Scan(&s.player); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT coalesce(string_agg(concat_ws('|', id, tenant_id, brand_id, player_account_id, asset_code, created_at), ';' ORDER BY id), '')
			FROM wallets WHERE player_account_id = $1`, player).Scan(&s.wallet); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT coalesce(string_agg(concat_ws('|', id, tenant_id, brand_id, player_account_id), ';' ORDER BY id), '')
			FROM kyc_verifications WHERE player_account_id = $1`, player).Scan(&s.kyc)
	}); err != nil {
		w.t.Fatalf("read binding state: %v", err)
	}
	if s.player == "" {
		w.t.Fatalf("binding state read nothing (blind read?): %+v", s)
	}
	return s
}

// rtExec runs one statement as igaming_runtime in a tenant transaction.
func (w *world) rtExecIn(tenantID uuid.UUID, sql string, args ...any) error {
	return w.rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

func execOn(p *db.Pool, tenantID uuid.UUID, sql string, args ...any) error {
	return p.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// wantBR asserts the exact SQLSTATE (or, with code "BR", the class with one of codes), and that the
// message carries no row value (no id of the fixture).
func wantBR(t *testing.T, what string, err error, codes []string, leak ...uuid.UUID) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		t.Fatalf("%s: want SQLSTATE %v, got %v", what, codes, err)
	}
	ok := false
	for _, c := range codes {
		if pe.Code == c {
			ok = true
		}
	}
	if !ok || !strings.HasPrefix(pe.Code, "BR") {
		t.Fatalf("%s: want SQLSTATE %v, got %s (%s)", what, codes, pe.Code, pe.Message)
	}
	for _, id := range leak {
		if strings.Contains(pe.Message+pe.Detail+pe.Hint, id.String()) {
			t.Fatalf("%s: the BR message must carry no row value, got %q / %q", what, pe.Message, pe.Detail)
		}
	}
	if !strings.Contains(pe.Message, "ADR 0118") || !strings.Contains(pe.Message, "column(s)") {
		t.Fatalf("%s: the message must name the ADR and the changed column set: %q", what, pe.Message)
	}
}

// playerOnly registers a player on the brand WITHOUT a wallet (a re-point target that satisfies every
// FK and the (player_account_id, asset_code) UNIQUE, so only the 0136 guard can refuse it).
func (w *world) playerOnly(tenantID, brandID uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		br, err := identity.GetBrandByID(ctx, tx, brandID)
		if err != nil {
			return err
		}
		p, err := identity.RegisterPlayer(ctx, tx, br, "lgov-bb-"+uuid.NewString()+"@fixture.invalid", "!x")
		id = p.ID
		return err
	}); err != nil {
		w.t.Fatalf("seed player: %v", err)
	}
	return id
}

func (w *world) kycVerification(tenantID, brandID, player uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			SELECT $1, $2, $3, id, person_id, 'pending', 'mock' FROM player_accounts WHERE id = $4`, id, tenantID, brandID, player)
		return err
	}); err != nil {
		w.t.Fatalf("seed kyc verification: %v", err)
	}
	return id
}

// T1: the C2 re-home CTE, both orders, into an active brand and into a closed brand of the same tenant.
func TestBrandBinding_T1_RehomeCTE_BothOrders_Refused(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b1, b2, b3 := w.activeBrand(tid), w.activeBrand(tid), w.activeBrand(tid)
	if err := launchfix.TrySetBrandStatus(ctx, tid, b3, "closed"); err != nil {
		t.Fatalf("close b3: %v", err)
	}
	p, wl := w.playerWithWallet(tid, b1)
	w.credit(tid, wl, 5000, ledger.AccountManualAdjustment) // ordinary cash, no brand-pinned child (ADR 0118 s1)
	before := w.binding(tid, p)
	for _, target := range []uuid.UUID{b2, b3} {
		for name, q := range map[string]string{"player_first": rehomeCTE, "wallet_first": rehomeCTEReverse} {
			wantBR(t, "T1 "+name, w.rtExecIn(tid, q, target, p), []string{"BR001", "BR002"}, p, wl, target, b1)
			if got := w.binding(tid, p); got != before {
				t.Fatalf("T1 %s: the refused re-home changed the binding:\n before %+v\n after  %+v", name, before, got)
			}
		}
	}
}

// T2: the motivating scenario (ADR 0118 s1 consequence 1). A stranded hold on b1 (owner tamper plant, no
// withdrawal request names it: no brand-pinned child row), b2 closed after its LA025 evaluated nothing.
func TestBrandBinding_T2_StrandedHold_CannotMoveIntoClosedBrandOrOutOfItsBrand(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b1, b2, b3 := w.activeBrand(tid), w.activeBrand(tid), w.activeBrand(tid)
	p, wl := w.playerWithWallet(tid, b1)
	w.strandHold(tid, wl, 700)
	s1 := launchgov.Subject{TenantID: tid, BrandID: &b1}
	s2 := launchgov.Subject{TenantID: tid, BrandID: &b2}
	if err := launchfix.TrySetBrandStatus(ctx, tid, b2, "closed"); err != nil {
		t.Fatalf("b2 holds nothing: its closure (LA025 evaluated) commits: %v", err)
	}
	if reqs, holds := w.ownerCounts(s1); reqs != 0 || holds != 1 {
		t.Fatalf("precondition (owner view): no request and one non-zero hold account on b1, got %d / %d", reqs, holds)
	}
	before := w.binding(tid, p)
	// Into the closed brand (hold lands after LA025 passed) and out to another active brand (b1's LA025
	// count would drop to zero), both orders.
	for _, target := range []uuid.UUID{b2, b3} {
		for _, q := range []string{rehomeCTE, rehomeCTEReverse} {
			wantBR(t, "T2 re-home of a stranded hold", w.rtExecIn(tid, q, target, p), []string{"BR001", "BR002"}, p, wl)
		}
	}
	if got := w.binding(tid, p); got != before {
		t.Fatalf("T2: binding changed:\n before %+v\n after  %+v", before, got)
	}
	if reqs, holds := w.ownerCounts(s1); reqs != 0 || holds != 1 {
		t.Fatalf("T2: LA025 arithmetic for b1 must be unchanged, got %d / %d", reqs, holds)
	}
	if _, holds := w.ownerCounts(s2); holds != 0 {
		t.Fatalf("T2: the closed brand acquired a hold account: %d", holds)
	}
	wantLA025(t, "closing b1 after the refused re-home", launchfix.TrySetBrandStatus(ctx, tid, b1, "closed"), 1)
	if s := w.status(s1); s != "active" {
		t.Fatalf("b1 must stay active, got %s", s)
	}
}

// T2 second case: a player with a TERMINAL withdrawal request was already pinned by the child's composite
// FK. With the two 0136 triggers disabled for one rolled-back owner transaction (exactly the 0135 schema
// for these tables: 0136 adds only these triggers), the re-home fails 23503; with them, class BR.
func TestBrandBinding_T2_TerminalRequest_23503Before_BRAfter(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	p, wl := w.playerWithWallet(tid, b1)
	w.withdrawal(tid, b1, p, wl, "cancelled")
	before := w.binding(tid, p)

	errRolledBack := errors.New("rolled back by design")
	var pre error
	err := w.owner.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`ALTER TABLE player_accounts DISABLE TRIGGER player_accounts_identity_immutable`,
			`ALTER TABLE wallets DISABLE TRIGGER wallets_identity_immutable`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		_, pre = tx.Exec(ctx, rehomeCTE, b2, p)
		return errRolledBack
	})
	if !errors.Is(err, errRolledBack) {
		t.Fatalf("emulated-0135 transaction: %v", err)
	}
	if pgCode(pre) != "23503" {
		t.Fatalf("before 0136 (triggers disabled) a player with a terminal request is pinned by the child FK: want 23503, got %v", pre)
	}
	for _, q := range []string{rehomeCTE, rehomeCTEReverse} {
		wantBR(t, "T2 terminal request after 0136", w.rtExecIn(tid, q, b2, p), []string{"BR001", "BR002"}, p)
	}
	if got := w.binding(tid, p); got != before {
		t.Fatalf("binding changed:\n before %+v\n after  %+v", before, got)
	}
	// Both triggers are enabled again (the disable was transactional and rolled back).
	var enabled int
	if err := w.owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname IN ('player_accounts_identity_immutable', 'wallets_identity_immutable') AND tgenabled = 'O'`).Scan(&enabled)
	}); err != nil || enabled != 2 {
		t.Fatalf("the 0136 triggers must be enabled after the rolled-back emulation: %d err=%v", enabled, err)
	}
}

// T3: each pinned column of each table, alone. Exact SQLSTATE: in particular player_accounts.brand_id
// alone fails on its own trigger (BR001) before the FK would, and wallets.brand_id alone is BR002 (not the
// 23503 the composite FK would raise: this kills a mutant that drops brand_id from the function body).
func TestBrandBinding_T3_EveryPinnedColumnAlone(t *testing.T) {
	w := newWorld(t)
	tid, other := w.activeTenant(), w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	p, wl := w.playerWithWallet(tid, b1)
	p2 := w.playerOnly(tid, b1) // no wallet: a re-point to p2 would satisfy every FK and UNIQUE
	kv := w.kycVerification(tid, b1, p)
	before := w.binding(tid, p)
	cases := []struct {
		table, set, code string
		id               uuid.UUID
	}{
		{"player_accounts", "id = gen_random_uuid()", "BR001", p},
		{"player_accounts", "tenant_id = '" + other.String() + "'", "BR001", p},
		{"player_accounts", "brand_id = '" + b2.String() + "'", "BR001", p},
		{"player_accounts", "created_at = created_at - interval '1 day'", "BR001", p},
		{"wallets", "id = gen_random_uuid()", "BR002", wl},
		{"wallets", "tenant_id = '" + other.String() + "'", "BR002", wl},
		{"wallets", "brand_id = '" + b2.String() + "'", "BR002", wl},
		{"wallets", "player_account_id = '" + p2.String() + "'", "BR002", wl},
		{"wallets", "asset_code = 'USD'", "BR002", wl},
		{"wallets", "created_at = created_at - interval '1 day'", "BR002", wl},
		{"kyc_verifications", "tenant_id = '" + other.String() + "'", "BR003", kv},
		{"kyc_verifications", "brand_id = '" + b2.String() + "'", "BR003", kv},
		{"kyc_verifications", "player_account_id = '" + p2.String() + "'", "BR003", kv},
	}
	for _, c := range cases {
		err := w.rtExecIn(tid, `UPDATE `+c.table+` SET `+c.set+` WHERE id = $1`, c.id)
		wantBR(t, c.table+" SET "+c.set, err, []string{c.code}, c.id, p, p2, b2, other)
		col := strings.Fields(c.set)[0]
		var pe *pgconn.PgError
		if errors.As(err, &pe) && !strings.Contains(pe.Message, "column(s) "+col) {
			t.Fatalf("%s: the message must name exactly the changed column %q: %q", c.table, col, pe.Message)
		}
	}
	if got := w.binding(tid, p); got != before {
		t.Fatalf("T3: binding changed:\n before %+v\n after  %+v", before, got)
	}
}

// T4: the ADR 0118 s1.1 shapes. A wallet re-pointed to another player of the same tenant and brand (every
// FK satisfied), a wallet's asset changed, and a KYC verification re-pointed to another player of another
// brand (player + brand together, both FKs satisfied).
func TestBrandBinding_T4_RepointShapes(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	p, wl := w.playerWithWallet(tid, b1)
	w.credit(tid, wl, 1200, ledger.AccountManualAdjustment)
	victim := w.playerOnly(tid, b1) // no EUR wallet: the re-point satisfies every FK and UNIQUE (ADR 0118 s1.1)
	other := w.playerOnly(tid, b2)
	kv := w.kycVerification(tid, b1, p)
	before, beforeVictim := w.binding(tid, p), w.binding(tid, victim)
	wantBR(t, "wallet re-pointed to a same-brand player", w.rtExecIn(tid, `UPDATE wallets SET player_account_id = $1 WHERE id = $2`, victim, wl), []string{"BR002"}, wl, victim)
	wantBR(t, "wallet asset changed", w.rtExecIn(tid, `UPDATE wallets SET asset_code = 'GBP' WHERE id = $1`, wl), []string{"BR002"}, wl)
	wantBR(t, "kyc verification re-pointed (player + brand together)", w.rtExecIn(tid,
		`UPDATE kyc_verifications SET player_account_id = $1, brand_id = $2 WHERE id = $3`, other, b2, kv), []string{"BR003"}, kv, other, b2)
	if got := w.binding(tid, p); got != before {
		t.Fatalf("T4: binding changed:\n before %+v\n after  %+v", before, got)
	}
	if got := w.binding(tid, victim); got != beforeVictim {
		t.Fatalf("T4: the victim's binding changed:\n before %+v\n after  %+v", beforeVictim, got)
	}
}

// T5: no-op writes of pinned columns and every legitimate update path commit (the WHEN clause keeps them
// off the guard function entirely).
func TestBrandBinding_T5_NoOpAndLegitimateUpdatesPass(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b1 := w.activeBrand(tid)
	p, wl := w.playerWithWallet(tid, b1)
	kv := w.kycVerification(tid, b1, p)
	staff := w.tenantStaff(tid, auth.RoleTenantAdmin)
	before := w.binding(tid, p)
	for _, q := range []string{
		`UPDATE player_accounts SET id = id, tenant_id = tenant_id, brand_id = brand_id, created_at = created_at WHERE id = $1`,
		`UPDATE wallets SET id = id, tenant_id = tenant_id, brand_id = brand_id, player_account_id = player_account_id, asset_code = asset_code, created_at = created_at WHERE player_account_id = $1`,
		`UPDATE kyc_verifications SET tenant_id = tenant_id, brand_id = brand_id, player_account_id = player_account_id WHERE player_account_id = $1`,
		// The SET list of identity.SetPlayerAccountDeclaredResidence (the service function needs an active
		// evidence-collection configuration; its own suite runs it end to end).
		`UPDATE player_accounts SET declared_residence_country = 'MT', declared_residence_captured_at = now(), updated_at = now() WHERE id = $1`,
		`UPDATE wallets SET status = 'frozen' WHERE player_account_id = $1`,
		`UPDATE wallets SET status = 'active' WHERE player_account_id = $1`,
	} {
		if err := w.rtExecIn(tid, q, p); err != nil {
			t.Fatalf("legitimate / no-op update refused: %s: %v", q, err)
		}
	}
	if err := w.rt.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
		if err := identity.SetPlayerAccountStatus(ctx, tx, p, identity.PlayerStatusSuspended); err != nil {
			return err
		}
		if _, err := identity.ReinstatePlayerAccount(ctx, tx, p); err != nil {
			return err
		}
		if err := identity.SetPlayerAccountPasswordHash(ctx, tx, p, "!rotated"); err != nil {
			return err
		}
		if err := identity.SetPlayerAccountEmailVerified(ctx, tx, p); err != nil {
			return err
		}
		_, err := identity.SetPlayerAccountStatusIfCurrent(ctx, tx, p, identity.PlayerStatusActive, identity.PlayerStatusActive)
		return err
	}); err != nil {
		t.Fatalf("identity service writers: %v", err)
	}
	if err := w.rtExecIn(tid, `UPDATE kyc_verifications SET status = 'approved', reason = 'ok', provider_reference = 'ref-1',
			verified_residence_country = 'MT', verified_residence_source = 'reviewer_determination',
			verified_residence_set_by = $2, verified_residence_set_at = now(), updated_at = now() WHERE id = $1`, kv, staff.StaffID); err != nil {
		t.Fatalf("kyc status / reason / provider_reference / verified_residence_*: %v", err)
	}
	if got := w.binding(tid, p); got != before {
		t.Fatalf("T5: binding changed:\n before %+v\n after  %+v", before, got)
	}
	var status, wstatus string
	if err := w.owner.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status FROM player_accounts WHERE id = $1`, p).Scan(&status); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT status FROM wallets WHERE id = $1`, wl).Scan(&wstatus)
	}); err != nil || status != "active" || wstatus != "active" {
		t.Fatalf("the legitimate writes must have landed: player %q wallet %q err=%v", status, wstatus, err)
	}
}

// T7: the trigger binds the table OWNER too while it is enabled (DISABLE TRIGGER by the owner is the ADR
// 0110 s9.4 residual, asserted only by the catalog test).
func TestBrandBinding_T7_OwnerIsBound(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	p, wl := w.playerWithWallet(tid, b1)
	p2 := w.playerOnly(tid, b1)
	kv := w.kycVerification(tid, b1, p)
	before := w.binding(tid, p)
	var who bool
	if err := w.owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user::text = (SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = 'wallets')::text`).Scan(&who)
	}); err != nil || !who {
		t.Fatalf("the owner pool must be the wallets table owner: %v err=%v", who, err)
	}
	for _, q := range []string{rehomeCTE, rehomeCTEReverse} {
		wantBR(t, "owner re-home", execOn(w.owner, tid, q, b2, p), []string{"BR001", "BR002"}, p)
	}
	wantBR(t, "owner wallet re-point", execOn(w.owner, tid, `UPDATE wallets SET player_account_id = $1 WHERE id = $2`, p2, wl), []string{"BR002"}, wl)
	wantBR(t, "owner kyc re-point", execOn(w.owner, tid, `UPDATE kyc_verifications SET player_account_id = $1 WHERE id = $2`, p2, kv), []string{"BR003"}, kv)
	if got := w.binding(tid, p); got != before {
		t.Fatalf("T7: binding changed:\n before %+v\n after  %+v", before, got)
	}
}
