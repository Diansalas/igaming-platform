//go:build integration

// Shared fixtures for the K2 (ADR 0100) integration tests. Every world is
// fully isolated from every other test sharing the database: its own
// tenant, brand, players, staff, grants AND its own freshly created asset
// code, with its platform-baseline policy row scoped to that asset
// (asset_code = the world's asset). No test in this package ever authors a
// NULL-asset platform row, so "no in-force platform baseline -> disabled"
// (HD-PRH2-3, B-9) stays true for every other asset in the shared DB. All
// policy values are synthetic, test-only (HD-PRH2-3).
package adjustment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 20, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := pgCode(err); got != code {
		t.Fatalf("expected SQLSTATE %s, got %q (%v)", code, got, err)
	}
}

// staffMember is one staff principal with its own distinct Person.
type staffMember struct {
	ID       uuid.UUID
	PersonID uuid.UUID
	TenantID uuid.UUID // uuid.Nil for a platform principal
	Role     string
}

type world struct {
	t      *testing.T
	pool   *db.Pool
	svc    *Service
	Tenant uuid.UUID
	Brand  uuid.UUID
	Asset  string

	// Platform principals: two policy authors (A/B), a K1 grant approver,
	// and one acting principal (G-P2 grants for Tenant).
	AdminA, AdminB, GrantApprover, Acting staffMember
	// A second acting principal (for acting-approves-acting cases).
	Acting2 staffMember
	// Tenant staff.
	TenantAdmin      staffMember
	F1, F2, F3       staffMember // finance, with initiate+approve grants
	FinanceNoGrant   staffMember
	Player           uuid.UUID
	PlayerPerson     uuid.UUID
	Wallet           uuid.UUID
	grantIDs         map[string]uuid.UUID // key staffID|capability
	baselinePolicyID uuid.UUID
}

var worldCounter atomic.Int64

func (w *world) staff(tenantID uuid.UUID, role string) staffMember {
	w.t.Helper()
	return mkStaff(w.t, w.pool, tenantID, role, uuid.New())
}

func mkStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID, role string, person uuid.UUID) staffMember {
	t.Helper()
	ctx := context.Background()
	s := staffMember{ID: uuid.New(), PersonID: person, TenantID: tenantID, Role: role}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1) ON CONFLICT DO NOTHING`, person)
		return err
	}); err != nil {
		t.Fatalf("insert person: %v", err)
	}
	insert := func(ctx context.Context, tx pgx.Tx) error {
		var tid any
		if tenantID != uuid.Nil {
			tid = tenantID
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, $2, $3, 'x', $4, 'active', $5)`, s.ID, tid, "k2-"+s.ID.String()+"@test.invalid", role, person)
		return err
	}
	var err error
	if tenantID == uuid.Nil {
		err = pool.WithoutTenant(ctx, insert)
	} else {
		err = pool.WithTenant(ctx, tenantID, insert)
	}
	if err != nil {
		t.Fatalf("insert staff %s: %v", role, err)
	}
	return s
}

func assetActor(id uuid.UUID) assetregistry.ActorContext {
	return assetregistry.ActorContext{ActorID: id, ReasonCode: "test", RequestID: uuid.NewString()}
}

func fileAndApproveAsset(t *testing.T, pool *db.Pool, op assetregistry.ChangeOperation, code string, requester, approver uuid.UUID) {
	t.Helper()
	params := assetregistry.FileChangeRequestParams{Operation: op, AssetCode: code, Actor: assetActor(requester)}
	if op == assetregistry.ChangeCreate {
		params.AssetType = assetregistry.AssetTypeFiat
		params.DecimalExponent = 2
	}
	var reqID uuid.UUID
	if err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := assetregistry.FileChangeRequest(ctx, tx, params)
		reqID = req.ID
		return err
	}); err != nil {
		t.Fatalf("file %s: %v", op, err)
	}
	if err := pool.WithPlatformAdmin(context.Background(), approver, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.DecideChangeRequest(ctx, tx, assetregistry.DecideChangeRequestParams{RequestID: reqID, Approve: true, Actor: assetActor(approver)})
		return err
	}); err != nil {
		t.Fatalf("approve %s: %v", op, err)
	}
}

// worldOpts shapes newWorld.
type worldOpts struct {
	// authorizedAsset: activate + platform-authorize the asset and add the
	// tenant's all-products asset_authorizations row (not suspended). When
	// false the asset exists but is suspended (platform_authorized=false).
	authorizedAsset bool
	// base is the synthetic baseline requirement of the asset-scoped
	// platform row. 0 means "no baseline" (disabled).
	base int
}

func newWorld(t *testing.T, opts worldOpts) *world {
	t.Helper()
	return newWorldOn(t, testPool(t), opts)
}

func newWorldOn(t *testing.T, pool *db.Pool, opts worldOpts) *world {
	t.Helper()
	w := &world{t: t, pool: pool, svc: NewService(pool), grantIDs: map[string]uuid.UUID{}}
	ctx := context.Background()
	n := worldCounter.Add(1)

	w.AdminA = w.staff(uuid.Nil, "platform_admin")
	w.AdminB = w.staff(uuid.Nil, "platform_admin")
	w.GrantApprover = w.staff(uuid.Nil, "platform_admin")
	w.Acting = w.staff(uuid.Nil, "platform_admin")
	w.Acting2 = w.staff(uuid.Nil, "platform_admin")

	w.Tenant = uuid.New()
	w.Brand = uuid.New()
	if err := pool.WithPlatformAdmin(ctx, w.AdminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, $2, $3, 'under_platform_licence')`,
			w.Tenant, "K2 world", fmt.Sprintf("k2-%d-%s", n, strings.ReplaceAll(w.Tenant.String(), "-", "")[:12]))
		return err
	}); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := pool.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, name, slug) VALUES ($1, $2, 'K2 brand', $3)`,
			w.Brand, w.Tenant, "k2b-"+strings.ReplaceAll(w.Brand.String(), "-", "")[:16])
		return err
	}); err != nil {
		t.Fatalf("brand: %v", err)
	}

	w.Asset = "K2" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	fileAndApproveAsset(t, pool, assetregistry.ChangeCreate, w.Asset, w.AdminA.ID, w.AdminB.ID)
	if err := pool.WithPlatformAdmin(ctx, w.AdminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.CreateAsset(ctx, tx, assetregistry.CreateAssetParams{
			Code: w.Asset, AssetType: assetregistry.AssetTypeFiat, DecimalExponent: 2, DisplayName: "K2 test asset", Actor: assetActor(w.AdminA.ID),
		})
		return err
	}); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	w.TenantAdmin = w.staff(w.Tenant, "tenant_admin")
	if opts.authorizedAsset {
		w.authorizeAsset()
	}

	w.F1 = w.staff(w.Tenant, "finance")
	w.F2 = w.staff(w.Tenant, "finance")
	w.F3 = w.staff(w.Tenant, "finance")
	w.FinanceNoGrant = w.staff(w.Tenant, "finance")
	for _, f := range []staffMember{w.F1, w.F2, w.F3} {
		w.grantTenant(f, capability.CapabilityLedgerAdjustmentInitiate)
		w.grantTenant(f, capability.CapabilityLedgerAdjustmentApprove)
	}
	w.grantActing(w.Acting, capability.CapabilityLedgerAdjustmentInitiate)
	w.grantActing(w.Acting, capability.CapabilityLedgerAdjustmentApprove)
	w.grantActing(w.Acting2, capability.CapabilityLedgerAdjustmentApprove)

	w.Player, w.PlayerPerson, w.Wallet = w.newPlayer()

	if opts.base > 0 {
		w.baselinePolicyID = w.approvePolicy(PolicyChangeInput{
			ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelPlatform,
			AssetCode: strPtr(w.Asset), BaseRequiredApprovals: intPtr(opts.base),
		}, w.AdminA, w.AdminB)
	}
	return w
}

func (w *world) authorizeAsset() {
	t := w.t
	ctx := context.Background()
	fileAndApproveAsset(t, w.pool, assetregistry.ChangeActivate, w.Asset, w.AdminA.ID, w.AdminB.ID)
	if err := w.pool.WithPlatformAdmin(ctx, w.AdminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.SetActive(ctx, tx, w.Asset, true, assetActor(w.AdminA.ID))
		return err
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	fileAndApproveAsset(t, w.pool, assetregistry.ChangePlatformAuthorize, w.Asset, w.AdminA.ID, w.AdminB.ID)
	if err := w.pool.WithPlatformAdmin(ctx, w.AdminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.SetPlatformAuthorized(ctx, tx, w.Asset, true, assetActor(w.AdminA.ID))
		return err
	}); err != nil {
		t.Fatalf("platform-authorize: %v", err)
	}
	if err := w.pool.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.AuthorizeScope(ctx, tx, assetregistry.AuthorizeScopeParams{
			TenantID: w.Tenant, ScopeKind: assetregistry.ScopeTenant, AssetCode: w.Asset, Eligible: true, Actor: assetActor(w.TenantAdmin.ID),
		})
		return err
	}); err != nil {
		t.Fatalf("tenant-authorize: %v", err)
	}
}

func (w *world) newPlayer() (player, person, wallet uuid.UUID) {
	w.t.Helper()
	ctx := context.Background()
	player, person, wallet = uuid.New(), uuid.New(), uuid.New()
	if err := w.pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person)
		return err
	}); err != nil {
		w.t.Fatalf("person: %v", err)
	}
	if err := w.pool.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			VALUES ($1, $2, $3, $4, $5, 'x', 'active')`, player, w.Tenant, w.Brand, person, player.String()+"@k2.invalid"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, $5)`,
			wallet, w.Tenant, w.Brand, player, w.Asset)
		return err
	}); err != nil {
		w.t.Fatalf("player/wallet: %v", err)
	}
	return player, person, wallet
}

func grantKey(staff uuid.UUID, c capability.Capability) string {
	return staff.String() + "|" + string(c)
}

// grantTenant issues a G-T grant: tenant_admin requests, an independent
// platform principal co-approves (ADR 0099 §4).
func (w *world) grantTenant(f staffMember, c capability.Capability) uuid.UUID {
	w.t.Helper()
	ctx := context.Background()
	var reqID uuid.UUID
	if err := w.pool.WithPrincipalScope(ctx, w.Tenant, w.TenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := capability.CreateRequest(ctx, tx, w.Tenant, capability.NewRequestInput{GranteeStaffID: f.ID, Capability: c, ReasonCode: "k2-fixture"})
		reqID = r.ID
		return err
	}); err != nil {
		w.t.Fatalf("grant request %s: %v", c, err)
	}
	var gid uuid.UUID
	if err := w.pool.WithPlatformAdmin(ctx, w.GrantApprover.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, g, err := capability.DecideAndGrant(ctx, tx, w.Tenant, reqID, "approve", "k2-fixture")
		if err == nil {
			gid = g.ID
		}
		return err
	}); err != nil {
		w.t.Fatalf("grant approve %s: %v", c, err)
	}
	w.grantIDs[grantKey(f.ID, c)] = gid
	return gid
}

// grantActing issues a G-P2 grant (a platform principal acting in Tenant).
func (w *world) grantActing(p staffMember, c capability.Capability) uuid.UUID {
	w.t.Helper()
	ctx := context.Background()
	until := time.Now().Add(2 * time.Hour)
	var reqID uuid.UUID
	if err := w.pool.WithPlatformAdmin(ctx, w.AdminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := capability.CreateRequest(ctx, tx, w.Tenant, capability.NewRequestInput{GranteeStaffID: p.ID, Capability: c, ValidUntil: &until, ReasonCode: "k2-fixture"})
		reqID = r.ID
		return err
	}); err != nil {
		w.t.Fatalf("G-P2 request %s: %v", c, err)
	}
	var gid uuid.UUID
	if err := w.pool.WithPlatformAdmin(ctx, w.GrantApprover.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, g, err := capability.DecideAndGrant(ctx, tx, w.Tenant, reqID, "approve", "k2-fixture")
		if err == nil {
			gid = g.ID
		}
		return err
	}); err != nil {
		w.t.Fatalf("G-P2 approve %s: %v", c, err)
	}
	w.grantIDs[grantKey(p.ID, c)] = gid
	return gid
}

func (w *world) revokeGrant(staff uuid.UUID, c capability.Capability) {
	w.t.Helper()
	gid, ok := w.grantIDs[grantKey(staff, c)]
	if !ok {
		w.t.Fatalf("no grant recorded for %s %s", staff, c)
	}
	if err := w.pool.WithPrincipalScope(context.Background(), w.Tenant, w.TenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
		return capability.RevokeGrant(ctx, tx, w.Tenant, gid, "k2-test-revoke")
	}); err != nil {
		w.t.Fatalf("revoke: %v", err)
	}
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// approvePolicy proposes and approves a policy change; returns the policy id.
func (w *world) approvePolicy(in PolicyChangeInput, requester, approver staffMember) uuid.UUID {
	w.t.Helper()
	c, err := w.proposePolicy(in, requester)
	if err != nil {
		w.t.Fatalf("propose policy: %v", err)
	}
	if err := w.decidePolicy(c, approver, DecisionApprove); err != nil {
		w.t.Fatalf("approve policy: %v", err)
	}
	var id uuid.UUID
	if err := w.pool.WithPlatformAdmin(context.Background(), w.AdminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM financial_approval_policies WHERE change_id = $1`, c.ID).Scan(&id)
	}); err != nil {
		w.t.Fatalf("read policy row: %v", err)
	}
	return id
}

func (w *world) policySession(actor staffMember, fn func(ctx context.Context, tx pgx.Tx, call PolicyCall) error) error {
	ctx := context.Background()
	call := PolicyCall{ActorID: actor.ID, TenantID: actor.TenantID}
	if actor.TenantID == uuid.Nil {
		return w.pool.WithPlatformAdmin(ctx, actor.ID, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, tx, call) })
	}
	return w.pool.WithPrincipalScope(ctx, actor.TenantID, actor.ID, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, tx, call) })
}

func (w *world) proposePolicy(in PolicyChangeInput, requester staffMember) (PolicyChange, error) {
	var c PolicyChange
	err := w.policySession(requester, func(ctx context.Context, tx pgx.Tx, call PolicyCall) error {
		var err error
		c, err = ProposePolicyChangeInTx(ctx, tx, call, in)
		return err
	})
	return c, err
}

func (w *world) decidePolicy(c PolicyChange, approver staffMember, d Decision) error {
	return w.policySession(approver, func(ctx context.Context, tx pgx.Tx, call PolicyCall) error {
		_, err := DecidePolicyChangeInTx(ctx, tx, call, c.ID, d, c.ContentHash, "k2-test")
		return err
	})
}

// ctxFor returns a context carrying actor's verified-token identity, as the
// HTTP auth middleware would build it.
func ctxFor(actor staffMember) context.Context {
	return tenant.WithContext(context.Background(), tenant.Context{TenantID: actor.TenantID, Subject: actor.ID.String(), Role: actor.Role})
}

func (w *world) target() Target {
	tg, err := NewTarget(tenant.Context{TenantID: uuid.Nil}, w.Tenant)
	if err != nil {
		w.t.Fatal(err)
	}
	return tg
}

// submit submits through the Service (the real session dispatch).
func (w *world) submit(actor staffMember, in SubmitInput) (Request, error) {
	return w.svc.Submit(ctxFor(actor), w.target(), in, Meta{RequestID: "k2-test"})
}

func (w *world) decide(actor staffMember, r Request, d Decision) (Outcome, error) {
	return w.svc.Decide(ctxFor(actor), w.target(), r.ID, DecisionInput{Decision: d, PayloadHash: r.PayloadHash, ReasonCode: "k2-test"}, Meta{RequestID: "k2-test"})
}

func (w *world) credit(amount int64, reason ReasonCode) SubmitInput {
	return SubmitInput{WalletID: w.Wallet, AssetCode: w.Asset, Direction: DirectionCreditPlayer, Amount: amount, ReasonCode: reason,
		EvidenceRefHash: evidenceFor(reason), Note: "k2 test adjustment"}
}

func (w *world) debit(amount int64, reason ReasonCode) SubmitInput {
	in := w.credit(amount, reason)
	in.Direction = DirectionDebitPlayer
	return in
}

func evidenceFor(reason ReasonCode) *string {
	if reason == ReasonCompensatingEntry || reason == ReasonExternalInstruction {
		h := strings.Repeat("ab", 32)
		return &h
	}
	return nil
}

// fund posts a casino_win for the player (house_gaming -> player_cash) so a
// debit has funds, and returns its ledger transaction id (usable as a
// causation: a player_cash leg on this wallet).
func (w *world) fund(amount int64) uuid.UUID {
	w.t.Helper()
	return w.postCasino(ledger.TxCasinoWin, amount)
}

func (w *world) postCasino(tt ledger.TransactionType, amount int64) uuid.UUID {
	w.t.Helper()
	var txID uuid.UUID
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		wallet := w.Wallet
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.Tenant,
			ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: w.Asset},
			ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: w.Asset})
		if err != nil {
			return err
		}
		pid, ptx := "k2-mock-casino", uuid.NewString()
		playerDir, houseDir := ledger.Credit, ledger.Debit
		if tt == ledger.TxCasinoBet {
			playerDir, houseDir = ledger.Debit, ledger.Credit
		}
		res, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.Tenant, TransactionType: tt, IdempotencyKey: "k2-fixture:" + ptx,
			ProviderID: &pid, ProviderTxID: &ptx, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: ids[1], Direction: houseDir, Amount: amount},
				{LedgerAccountID: ids[0], Direction: playerDir, Amount: amount},
			},
		})
		txID = res.TransactionID
		return err
	}); err != nil {
		w.t.Fatalf("post %s: %v", tt, err)
	}
	return txID
}

// playerCash returns the projected player_cash balance of the world's wallet.
func (w *world) playerCash() int64 {
	w.t.Helper()
	var bal int64
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		wallet := w.Wallet
		id, err := ledger.GetOrCreateAccount(ctx, tx, w.Tenant, &wallet, ledger.AccountPlayerCash, w.Asset)
		if err != nil {
			return err
		}
		b, err := ledger.GetProjectedBalance(ctx, tx, id)
		bal = b.Signed()
		return err
	}); err != nil {
		w.t.Fatalf("balance: %v", err)
	}
	return bal
}

// assertInvariants is LF test 14: every K2 test ends with SUM(debits) ==
// SUM(credits) per (transaction, asset) and per tenant, and every
// projection row of the tenant equal to its recomputation from entries.
func (w *world) assertInvariants() {
	w.t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var unbalancedTx int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM (
			SELECT ledger_transaction_id, asset_code FROM ledger_entries WHERE tenant_id = $1
			GROUP BY 1, 2 HAVING sum(CASE WHEN direction = 'debit' THEN amount ELSE -amount END) <> 0) x`, w.Tenant).Scan(&unbalancedTx); err != nil {
			return err
		}
		if unbalancedTx != 0 {
			return fmt.Errorf("SUM(D) != SUM(C) for %d transaction(s)", unbalancedTx)
		}
		var debits, credits string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount) FILTER (WHERE direction='debit'),0)::text,
			COALESCE(sum(amount) FILTER (WHERE direction='credit'),0)::text FROM ledger_entries WHERE tenant_id = $1`, w.Tenant).Scan(&debits, &credits); err != nil {
			return err
		}
		if debits != credits {
			return fmt.Errorf("tenant SUM(D)=%s != SUM(C)=%s", debits, credits)
		}
		var drift int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM wallet_balance_projection p
			LEFT JOIN (SELECT ledger_account_id,
			                  sum(amount) FILTER (WHERE direction='debit') AS d,
			                  sum(amount) FILTER (WHERE direction='credit') AS c
			             FROM ledger_entries WHERE tenant_id = $1 GROUP BY 1) e ON e.ledger_account_id = p.ledger_account_id
			WHERE p.tenant_id = $1 AND (p.debit_total <> COALESCE(e.d,0) OR p.credit_total <> COALESCE(e.c,0))`, w.Tenant).Scan(&drift); err != nil {
			return err
		}
		if drift != 0 {
			return fmt.Errorf("projection != recomputed for %d account(s)", drift)
		}
		return nil
	}); err != nil {
		w.t.Fatalf("financial invariants: %v", err)
	}
}

// requestState reads a request's state through a tenant session.
func (w *world) request(id uuid.UUID) Request {
	w.t.Helper()
	var r Request
	if err := w.pool.WithPrincipalScope(context.Background(), w.Tenant, w.F1.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = GetInTx(ctx, tx, w.Tenant, id)
		return err
	}); err != nil {
		w.t.Fatalf("read request: %v", err)
	}
	return r
}

// auditActions returns the audit actions recorded for a request id, in order.
func (w *world) auditActions(requestID uuid.UUID) []string {
	w.t.Helper()
	var out []string
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action FROM audit_log WHERE tenant_id = $1 AND target_id = $2 AND target_type = 'ledger_adjustment_request' ORDER BY created_at, id`,
			w.Tenant, requestID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	}); err != nil {
		w.t.Fatalf("audit read: %v", err)
	}
	return out
}
