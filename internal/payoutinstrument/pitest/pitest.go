//go:build integration

// Package pitest is the shared B13 test support for packages whose fixtures create withdrawals
// (B13-B, ADR 0111 section 18). Since migration 0126 a withdrawal row can no longer be inserted
// without a verified payout instrument binding, in MOCK as well (owner decision 8), so every
// fixture binds a MOCK (synthetic_test, verified by the MOCK verifier) instrument.
//
// It is compiled ONLY under the integration build tag: the random per-process key material below
// must never exist in a production binary (the payoutinstrument key rule: no in-binary random-key
// fallback outside the integration build tag). It deliberately does not import internal/withdrawal
// (that package's own tests import this one).
package pitest

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
)

var (
	once   sync.Once
	shared *payoutinstrument.Service
	serr   error
)

func key() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// Shared returns the process-wide payout-instrument Service (random keys generated once, the
// default kinds, the MOCK verifier). Fixtures that register instruments and the orchestrator /
// withdrawal paths under test MUST use the same instance: the seals are keyed.
func Shared() *payoutinstrument.Service {
	once.Do(func() {
		keys, err := payoutinstrument.NewKeys("m1", map[string][]byte{"m1": key()}, "f1", map[string][]byte{"f1": key()})
		if err != nil {
			serr = err
			return
		}
		shared, serr = payoutinstrument.NewService(keys, payoutinstrument.DefaultKinds(), payoutinstrument.NewMockVerifier())
	})
	if serr != nil {
		panic("pitest: " + serr.Error())
	}
	return shared
}

// Bind registers and verifies (MOCK verifier) a fresh synthetic_test bank_transfer instrument for
// the player, listing the given assets (default EUR), and returns its id. A fresh random label per
// call, so the same player may hold several.
func Bind(t testing.TB, pool *db.Pool, tenantID, playerAccountID uuid.UUID, assets ...string) uuid.UUID {
	t.Helper()
	if len(assets) == 0 {
		assets = []string{"EUR"}
	}
	svc := Shared()
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	// Digit-free alphabet (LF L-1): a hex label can contain a Luhn-valid 12-19 digit run, which the card-number
	// detector (correctly) refuses, making the fixture flaky.
	label := make([]byte, 0, len(raw)+1)
	label = append(label, 't')
	for _, c := range raw {
		label = append(label, 'a'+c%26)
	}
	detail, _ := json.Marshal(map[string]string{"label": string(label)})
	ctx := context.Background()
	var inst payoutinstrument.Instrument
	if err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		res, err := svc.Register(ctx, tx, payoutinstrument.RegisterParams{
			TenantID: tenantID, PlayerAccountID: playerAccountID, Kind: payoutinstrument.KindSyntheticTest,
			Rail: "bank_transfer", AssetCodes: assets, Detail: detail,
		})
		inst = res.Instrument
		return err
	}); err != nil {
		t.Fatalf("pitest: register payout instrument: %v", err)
	}
	if _, err := svc.Verify(ctx, pool, tenantID, inst.ID); err != nil {
		t.Fatalf("pitest: verify payout instrument: %v", err)
	}
	return inst.ID
}

// WithoutBindingGuard runs fn with the withdrawal_requests binding-guard trigger (migration 0126)
// disabled INSIDE tx and re-enabled before returning, so a fixture can plant the LEGACY (NULL/NULL)
// row shape that existed before B13-B (for tests of receipt / reconciliation / migration behaviour
// that are not about the binding), or a row that attacks a different invariant (RLS, a brand FK)
// which a BEFORE INSERT trigger would otherwise pre-empt. The DDL is transactional but a COMMIT
// would make a disabled trigger permanent, hence the re-enable; on a fn error the caller's tx
// rolls back. Needs the table owner (the tests' TEST_DATABASE_URL role).
//
// On a scratch database migrated to a version BEFORE 0123 the trigger does not exist (a few migration
// tests pin an old schema): fn then runs as is.
func WithoutBindingGuard(ctx context.Context, tx pgx.Tx, fn func() error) error {
	var present bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'withdrawal_requests_payout_binding_guard'
		AND tgrelid = 'withdrawal_requests'::regclass)`).Scan(&present); err != nil {
		return err
	}
	if !present {
		return fn()
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_payout_binding_guard`); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `ALTER TABLE withdrawal_requests ENABLE TRIGGER withdrawal_requests_payout_binding_guard`)
	return err
}
