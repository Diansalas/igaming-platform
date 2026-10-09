//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
)

// R31 (ADR 0095 section 48 decision 3, ADR 0111 section 22): instrument eligibility stays a rule of INITIATION. A NEW
// withdrawal request naming an unverified, suspended, revoked or verification-expired instrument is the same generic 409
// PAYOUT_INSTRUMENT_NOT_USABLE: no row, no hold, and the idempotency key is not consumed. (The settlement side of the
// decision - an already-authorised payout still settles after such a change - is pinned in internal/payments.)
func TestRequestWithdrawalHandler_R31_UnusableInstrumentStates_Generic409(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newFinancialTestServer(t, pool, issuer, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100_000)
	mustApproveKYCForWithdrawal(t, pool, tenant.ID, brand.ID, player.ID)
	svc := pitest.Shared()
	ctx := context.Background()

	rows := func() int {
		return countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, tenant.ID)
	}
	ledger := func() int {
		return countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID)
	}
	block := func(id uuid.UUID, suspend bool) {
		t.Helper()
		if err := pool.WithTenant(ctx, tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			if suspend {
				_, err = svc.Suspend(ctx, tx, payoutinstrument.BlockParams{TenantID: tenant.ID, InstrumentID: id,
					Actor: payoutinstrument.Actor{Type: payoutinstrument.ActorStaff, ID: uuid.NewString()}, ReasonCode: "aml_review"})
			} else {
				_, err = svc.ApplyProviderBlock(ctx, tx, tenant.ID, id, payoutinstrument.MockVerifierID, payoutinstrument.EventRevoke, "provider_revoked")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	post := func(id uuid.UUID) *http.Response {
		return postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken,
			map[string]any{"asset_code": "EUR", "amount": 500, "idempotency_key": "r31-same-key", "payout_instrument_id": id.String()})
	}
	refused := func(name string, id uuid.UUID) {
		t.Helper()
		r0, l0 := rows(), ledger()
		resp := post(id)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("%s: status %d, want 409", name, resp.StatusCode)
		}
		if got := apiCode(t, resp); got != apierror.CodePayoutInstrumentNotUsable {
			t.Fatalf("%s: code %s, want %s (one generic refusal for every unusable state)", name, got, apierror.CodePayoutInstrumentNotUsable)
		}
		if rows() != r0 || ledger() != l0 {
			t.Fatalf("%s: a refusal must write no row and no hold", name)
		}
	}

	// unverified: registered, never verified
	var pending uuid.UUID
	if err := pool.WithTenant(ctx, tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := svc.Register(ctx, tx, payoutinstrument.RegisterParams{TenantID: tenant.ID, PlayerAccountID: player.ID,
			Kind: payoutinstrument.KindSyntheticTest, Rail: "bank_transfer", AssetCodes: []string{"EUR"}, Detail: []byte(`{"label":"pendingone"}`)})
		pending = r.Instrument.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	refused("unverified", pending)

	suspended := pitest.Bind(t, pool, tenant.ID, player.ID, "EUR")
	block(suspended, true)
	refused("suspended", suspended)

	revoked := pitest.Bind(t, pool, tenant.ID, player.ID, "EUR")
	block(revoked, false)
	refused("revoked", revoked)

	// verification expired: the tenant's jurisdiction gets a 2s verification max age; a freshly bound instrument's
	// verification really expires.
	jid, lid := uuid.New(), uuid.New()
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'R31 test jurisdiction')`, jid, "R31-"+jid.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1,$2,'platform',$3)`, lid, jid, "L-"+lid.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lid, tenant.ID)
		return err
	}); err != nil {
		t.Fatalf("licence tenant: %v", err)
	}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payout_instrument_verification_max_age (jurisdiction_id, max_age) VALUES ($1, interval '2 seconds')`, jid)
		return err
	}); err != nil {
		t.Fatalf("max age: %v", err)
	}
	expired := pitest.Bind(t, pool, tenant.ID, player.ID, "EUR")
	time.Sleep(2500 * time.Millisecond)
	refused("verification expired", expired)

	// None of the refusals consumed the key: the SAME key succeeds with a usable instrument (the max age is 2s, so bind
	// right before use).
	good := pitest.Bind(t, pool, tenant.ID, player.ID, "EUR")
	if resp := post(good); resp.StatusCode != http.StatusCreated {
		t.Fatalf("the idempotency key must not have been consumed by the refusals: status %d", resp.StatusCode)
	}
}
