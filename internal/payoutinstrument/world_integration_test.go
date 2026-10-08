//go:build integration

package payoutinstrument

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// world is one tenant with two brands on a database where every assertion runs
// as the RUNTIME role (asserted NOT rolsuper, NOT rolbypassrls); fixtures and
// the deliberate "SQL tamper" attacks run through the owner role.
type world struct {
	t        *testing.T
	owner    *db.Pool
	rt       *db.Pool
	tenantID uuid.UUID
	brandID  uuid.UUID
	brand2ID uuid.UUID
	keys     *Keys
	svc      *Service
}

func connect(t *testing.T, env string) *db.Pool {
	t.Helper()
	url := os.Getenv(env)
	if url == "" {
		t.Skipf("%s not set", env)
	}
	p, err := db.Connect(context.Background(), url, 10, 5*time.Second)
	if err != nil {
		t.Fatalf("connect %s: %v", env, err)
	}
	t.Cleanup(p.Close)
	return p
}

func newWorld(t *testing.T, fpKids ...string) *world {
	t.Helper()
	owner := connect(t, "TEST_DATABASE_URL")
	rt := connect(t, "TEST_RUNTIME_DATABASE_URL")
	var super, bypass bool
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("the runtime role must be NOT rolsuper and NOT rolbypassrls (super=%v bypass=%v)", super, bypass)
	}
	w := &world{t: t, owner: owner, rt: rt, tenantID: uuid.New(), brandID: uuid.New(), brand2ID: uuid.New()}
	w.keys = newTestKeys(t, fpKids...)
	svc, err := NewService(w.keys, DefaultKinds(), NewMockVerifier())
	if err != nil {
		t.Fatal(err)
	}
	w.svc = svc
	if err := owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'B13 tenant', 'under_platform_licence')`,
			w.tenantID, "b13-"+w.tenantID.String()[:8])
		return err
	}); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := owner.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, b := range []uuid.UUID{w.brandID, w.brand2ID} {
			if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'B13 brand')`, b, w.tenantID, "b-"+b.String()[:8]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed brands: %v", err)
	}
	return w
}

type player struct {
	ID, PersonID, BrandID uuid.UUID
}

func (w *world) newPlayer(brand uuid.UUID) player {
	w.t.Helper()
	p := player{ID: uuid.New(), PersonID: uuid.New(), BrandID: brand}
	if err := w.owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, p.PersonID)
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	if err := w.owner.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			VALUES ($1, $2, $3, $4, $5, 'x', 'active')`, p.ID, w.tenantID, brand, p.PersonID, p.ID.String()+"@example.test")
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return p
}

// approveKYC seeds an approved KYC verification for the player.
func (w *world) approveKYC(p player) {
	w.t.Helper()
	if err := w.owner.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			VALUES ($1, $2, $3, $4, $5, 'approved', 'mock')`, uuid.New(), w.tenantID, p.BrandID, p.ID, p.PersonID)
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
}

const (
	ibanA = "GB82WEST12345698765432"
	ibanB = "DE89370400440532013000"
)

func ibanDetail(iban string) json.RawMessage { return json.RawMessage(`{"iban":"` + iban + `"}`) }

func (w *world) registerParams(p player, iban string) RegisterParams {
	return RegisterParams{TenantID: w.tenantID, PlayerAccountID: p.ID, Kind: KindBankAccount, Rail: "sepa", AssetCodes: []string{"EUR"}, Detail: ibanDetail(iban)}
}

// register runs Register in a runtime tenant transaction.
func (w *world) register(p player, iban string) (RegisterResult, error) {
	return w.registerWith(w.svc, w.registerParams(p, iban))
}

func (w *world) registerWith(svc *Service, rp RegisterParams) (RegisterResult, error) {
	var res RegisterResult
	err := w.rt.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = svc.Register(ctx, tx, rp)
		return err
	})
	return res, err
}

func (w *world) mustRegister(p player, iban string) Instrument {
	w.t.Helper()
	res, err := w.register(p, iban)
	if err != nil {
		w.t.Fatalf("register: %v", err)
	}
	return res.Instrument
}

// verified registers and verifies (MOCK verifier) an IBAN instrument.
func (w *world) verified(p player, iban string) Instrument {
	w.t.Helper()
	inst := w.mustRegister(p, iban)
	if _, err := w.svc.Verify(context.Background(), w.rt, w.tenantID, inst.ID); err != nil {
		w.t.Fatalf("verify: %v", err)
	}
	return w.load(inst.ID)
}

func (w *world) load(id uuid.UUID) Instrument {
	w.t.Helper()
	var inst Instrument
	if err := w.rt.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inst, err = loadInstrument(ctx, tx, w.tenantID, id, lockNone)
		return err
	}); err != nil {
		w.t.Fatalf("load: %v", err)
	}
	return inst
}

// rtTx runs fn in a runtime tenant transaction and returns its error.
func (w *world) rtTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return w.rt.WithTenant(context.Background(), w.tenantID, fn)
}

// ownerTx runs fn in an OWNER tenant transaction (still subject to FORCE RLS).
func (w *world) ownerTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return w.owner.WithTenant(context.Background(), w.tenantID, fn)
}

// tamper simulates arbitrary SQL by the owner (the attack the Go-side seals
// exist for): inside one owner transaction the table's user triggers are
// disabled and row-level security is un-forced, stmt runs, then both are
// restored before commit. It fails the test if stmt touched no row.
func (w *world) tamper(table string, stmt string, args ...any) {
	w.t.Helper()
	if err := w.tamperErr(table, stmt, args...); err != nil {
		w.t.Fatalf("tamper %s: %v", table, err)
	}
}

func (w *world) tamperErr(table string, stmt string, args ...any) error {
	return w.ownerTx(func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{`ALTER TABLE ` + table + ` DISABLE TRIGGER USER`, `ALTER TABLE ` + table + ` NO FORCE ROW LEVEL SECURITY`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, stmt, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() < 1 {
			return fmt.Errorf("tamper statement affected no row")
		}
		for _, q := range []string{`ALTER TABLE ` + table + ` FORCE ROW LEVEL SECURITY`, `ALTER TABLE ` + table + ` ENABLE TRIGGER USER`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
}

func (w *world) gate(inst Instrument, p player, asset string, adapter any) (GateResult, error) {
	var g GateResult
	err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		g, err = w.svc.EvaluateGate(ctx, tx, GateParams{TenantID: w.tenantID, BrandID: p.BrandID, PlayerAccountID: p.ID, PersonID: p.PersonID,
			InstrumentID: inst.ID, AssetCode: asset, Adapter: adapter, Lock: true, WantDetail: true})
		return err
	})
	return g, err
}

func pgState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func requireCode(t *testing.T, err error, code string, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected SQLSTATE %s, got success", what, code)
	}
	if got := pgState(err); got != code {
		t.Fatalf("%s: expected SQLSTATE %s, got %q: %v", what, code, got, err)
	}
}

func requireGateReason(t *testing.T, err error, reason string, integrity bool) {
	t.Helper()
	g, ok := IsGateRefusal(err)
	if !ok {
		t.Fatalf("expected a gate refusal %q, got %v", reason, err)
	}
	if g.Reason != reason || g.Integrity() != integrity {
		t.Fatalf("gate refusal = %q integrity=%v, want %q integrity=%v", g.Reason, g.Integrity(), reason, integrity)
	}
}

// countRows counts rows of a payout table for the tenant through the runtime role.
func (w *world) count(table, where string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		q := `SELECT count(*) FROM ` + table
		if where != "" {
			q += ` WHERE ` + where
		}
		return tx.QueryRow(ctx, q, args...).Scan(&n)
	}); err != nil {
		w.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func (w *world) seedWallet(p player, asset string) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.ownerTx(func(ctx context.Context, tx pgx.Tx) error {
		wl, err := wallet.GetOrCreate(ctx, tx, w.tenantID, p.BrandID, p.ID, asset)
		id = wl.ID
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return id
}

var _ = fmt.Sprintf
