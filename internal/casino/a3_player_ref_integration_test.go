//go:build integration

// CAS-PLAYER-REF-1 (A3): the provider-neutral opaque player reference.
//
// The vendor is handed a random per-(tenant, provider) reference instead of
// the platform's player account id, at launch, and echoes it on callbacks,
// where it is resolved scoped to the callback's own tenant and provider. A raw
// account id (or any unknown, foreign-tenant or foreign-provider reference)
// does not resolve. The table (migration 0111) is tenant-scoped, FORCE RLS,
// append-only, unique on (tenant, provider, player) and on (tenant, provider,
// ref); get-or-create is idempotent under concurrency.
package casino

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func countPlayerRefRows(t *testing.T, pool *db.Pool, tenantID uuid.UUID, providerID string, playerID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_provider_player_refs WHERE tenant_id = $1 AND provider_id = $2 AND player_account_id = $3`,
			tenantID, providerID, playerID).Scan(&n)
	}); err != nil {
		t.Fatalf("count player ref rows: %v", err)
	}
	return n
}

func TestA3_LaunchHandsTheVendorOnlyTheOpaqueRef(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	var results [2]LaunchGameResult
	for i := range results {
		r, err := orch.LaunchGame(context.Background(), pool, NewMockOutboundResolver(), LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		if err != nil || r.Denied {
			t.Fatalf("launch %d: %+v %v", i, r, err)
		}
		results[i] = r
	}
	reqs := provider.LaunchRequests()
	if len(reqs) != 2 {
		t.Fatalf("expected the vendor to receive 2 launch requests, got %d", len(reqs))
	}
	wantRef := playerRefFor(t, pool, f.tenantID, "mock-casino", f.playerAccountID)
	for i, req := range reqs {
		if req.PlayerRef == uuid.Nil || req.PlayerRef != wantRef {
			t.Fatalf("launch %d: the vendor must receive the issued opaque reference %s, got %s", i, wantRef, req.PlayerRef)
		}
		for name, forbidden := range map[string]uuid.UUID{
			"player account id": f.playerAccountID, "wallet id": f.walletID, "person id": f.personID,
			"tenant id": f.tenantID, "brand id": f.brandID,
		} {
			if req.PlayerRef == forbidden {
				t.Fatalf("launch %d: the vendor's player reference equals the platform %s", i, name)
			}
		}
		if req.PlayerRef.Version() != 4 {
			t.Fatalf("launch %d: the reference must be a random (v4) UUID, got version %d", i, req.PlayerRef.Version())
		}
		if strings.Contains(req.Call.IdempotencyKey, f.playerAccountID.String()) {
			t.Fatalf("launch %d: the call context must not carry the account id", i)
		}
	}
	if reqs[0].PlayerRef != reqs[1].PlayerRef || results[0].SessionID == results[1].SessionID {
		t.Fatal("the reference must be stable per player and provider across launches while sessions differ")
	}
	if strings.Contains(results[0].LaunchURL, f.playerAccountID.String()) {
		t.Fatalf("the launch URL must not carry the account id: %s", results[0].LaunchURL)
	}
	if n := countPlayerRefRows(t, pool, f.tenantID, "mock-casino", f.playerAccountID); n != 1 {
		t.Fatalf("expected exactly one reference row for the player, got %d", n)
	}
}

// The reference the vendor was launched with is the one bootstrap returns and
// the one a callback must echo.
func TestA3_LaunchRefIsTheBootstrapRefAndResolvesBackToThePlayer(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	if session.PlayerRef == uuid.Nil {
		t.Fatal("a freshly minted session must carry the issued player reference")
	}
	in := provider.BootstrapPayload(f.tenantID, token, "req-a3-ref-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	res, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if res.PlayerRef != session.PlayerRef {
		t.Fatalf("bootstrap must return the reference issued at launch (%s), got %s", session.PlayerRef, res.PlayerRef)
	}
	var resolved uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		resolved, err = ResolveProviderPlayerRef(ctx, tx, f.tenantID, "mock-casino", res.PlayerRef)
		return err
	}); err != nil || resolved != f.playerAccountID {
		t.Fatalf("the reference must resolve to the player: %s %v", resolved, err)
	}
}

func TestA3_PlayerRef_GetOrCreateIsIdempotentUnderConcurrency(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	// A provider with no reference yet for this player.
	const providerID = "mock-concurrent"
	const n = 24
	refs := make([]uuid.UUID, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				refs[i], err = GetOrCreateProviderPlayerRef(ctx, tx, f.tenantID, providerID, f.playerAccountID)
				return err
			})
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("get-or-create %d: %v", i, errs[i])
		}
		if refs[i] == uuid.Nil || refs[i] != refs[0] {
			t.Fatalf("every concurrent creator must converge on one reference: %s vs %s", refs[i], refs[0])
		}
	}
	if c := countPlayerRefRows(t, pool, f.tenantID, providerID, f.playerAccountID); c != 1 {
		t.Fatalf("expected exactly one row, got %d", c)
	}
}

func TestA3_PlayerRef_ScopedPerTenantAndProvider(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fB := seedCasinoFixture(t, pool)
	refA := fA.playerRef
	refB := fB.playerRef
	refA2 := playerRefFor(t, pool, fA.tenantID, "mock-other", fA.playerAccountID)
	if refA == refB || refA == refA2 {
		t.Fatal("references must differ across tenants and across providers")
	}
	resolve := func(tenantID uuid.UUID, providerID string, ref uuid.UUID) error {
		return pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ResolveProviderPlayerRef(ctx, tx, tenantID, providerID, ref)
			return err
		})
	}
	if err := resolve(fA.tenantID, "mock-casino", refA); err != nil {
		t.Fatalf("own reference must resolve: %v", err)
	}
	for name, c := range map[string]struct {
		tenant   uuid.UUID
		provider string
		ref      uuid.UUID
	}{
		"another tenant's reference":                 {fA.tenantID, "mock-casino", refB},
		"another provider's reference":               {fA.tenantID, "mock-casino", refA2},
		"the raw account id":                         {fA.tenantID, "mock-casino", fA.playerAccountID},
		"the raw account id of the other tenant":     {fB.tenantID, "mock-casino", fB.playerAccountID},
		"a reference under the other tenant's scope": {fB.tenantID, "mock-casino", refA},
		"an unknown reference":                       {fA.tenantID, "mock-casino", uuid.New()},
		"the nil reference":                          {fA.tenantID, "mock-casino", uuid.Nil},
	} {
		if err := resolve(c.tenant, c.provider, c.ref); !errors.Is(err, ErrPlayerRefUnresolved) {
			t.Fatalf("%s must not resolve (ErrPlayerRefUnresolved), got %v", name, err)
		}
	}
	// Random, not derived: a set of fresh players all get distinct v4 values.
	seen := map[uuid.UUID]bool{}
	for i := 0; i < 20; i++ {
		f := seedCasinoFixture(t, pool)
		if f.playerRef.Version() != 4 || seen[f.playerRef] || f.playerRef == f.playerAccountID {
			t.Fatalf("reference %s must be a unique random v4 value unrelated to the account id", f.playerRef)
		}
		seen[f.playerRef] = true
	}
}

// Callbacks: a raw account id (or any unresolvable reference) is refused for
// bets and wins with the same typed error, and nothing is posted.
func TestA3_Callbacks_RawAccountIDAndForeignRefsDoNotResolve(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	other := seedCasinoFixture(t, e.pool) // another tenant entirely
	rawAccount := e.f.playerAccountID

	cases := map[string]uuid.UUID{
		"raw player account id":     rawAccount,
		"another tenant's ref":      other.playerRef,
		"an unknown reference":      uuid.New(),
		"the session id as a ref":   e.sessionID,
		"the wallet id as a ref":    e.f.walletID,
		"the other tenant's player": other.playerAccountID,
	}
	for name, ref := range cases {
		for _, kind := range []CallbackEventType{CallbackEventBet, CallbackEventWin} {
			txID := "ref-" + strings.ReplaceAll(name, " ", "-") + "-" + string(kind)
			sess := uuid.Nil
			if kind == CallbackEventBet {
				sess = e.sessionID
			}
			payload := e.provider.CallbackPayload(e.f.tenantID, kind, txID, "", "round-ref", "game-1", 100, "EUR", OutcomeSucceeded, "", ref, sess)
			err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := e.orch.receiveCallbackInTx(ctx, tx, e.f.tenantID, "mock-casino", payload)
				return err
			})
			if !errors.Is(err, ErrPlayerRefUnresolved) || !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("%s as the player_ref of a %s must be refused with ErrPlayerRefUnresolved (an ErrInvalidInput), got %v", name, kind, err)
			}
			if c := e.countTx(t, txID); c != 0 {
				t.Fatalf("%s/%s: nothing may post, got %d ledger transactions", name, kind, c)
			}
		}
	}
	e.requireBalance(t, 10000)

	// A missing reference is a distinct, equally fatal input error.
	payload := e.provider.CallbackPayload(e.f.tenantID, CallbackEventBet, "ref-none", "", "round-ref", "game-1", 100, "EUR", OutcomeSucceeded, "", uuid.Nil, e.sessionID)
	err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.orch.receiveCallbackInTx(ctx, tx, e.f.tenantID, "mock-casino", payload)
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a bet with no player_ref must be an input error, got %v", err)
	}

	// The legitimate reference works for the same bet.
	if r, err := e.receive(t, CallbackEventBet, "ref-good", "", "round-ref", 100, e.sessionID); err != nil || r.Outcome != OutcomeSucceeded {
		t.Fatalf("the issued reference must be accepted: %+v %v", r, err)
	}

	// A rollback of a bet does not require the reference, but when it names
	// one on the stake-return path a raw account id is a mismatch (covered in
	// the non-active tenant stake-return suite); a plain rollback still works.
	if r, err := e.receive(t, CallbackEventRollback, "ref-rb", "ref-good", "round-ref", 0, uuid.Nil); err != nil || r.Outcome != OutcomeSucceeded {
		t.Fatalf("rollback: %+v %v", r, err)
	}
}

// The mock vendor never sees the account id: its callbacks carry the ref and
// a body naming the raw account id is refused by the adapter before dispatch.
func TestA3_Callbacks_RawAccountFieldInBodyIsMalformed(t *testing.T) {
	e := a3SetupConsumed(t, 1000)
	body := `{"event_type":"bet","provider_tx_id":"raw-body-1","round_id":"r","amount":100,"asset_code":"EUR","outcome":"succeeded","player_account_id":"` +
		e.f.playerAccountID.String() + `","session_id":"` + e.sessionID.String() + `"}`
	in := e.provider.SignRawBody(e.f.tenantID, []byte(body))
	err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.orch.receiveCallbackInTx(ctx, tx, e.f.tenantID, "mock-casino", in)
		return err
	})
	if !errors.Is(err, ErrCallbackMalformedBody) {
		t.Fatalf("a callback naming a raw player_account_id must be malformed, got %v", err)
	}
	e.requireBalance(t, 1000)
}

// Schema pins for the reference table (migration 0111): FORCE RLS, both
// uniqueness constraints, append-only, tenant isolation of reads and writes.
func TestA3_PlayerRefTable_RLSUniquenessAndAppendOnly(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fB := seedCasinoFixture(t, pool)

	// FORCE RLS (and RLS enabled).
	var enabled, forced bool
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'casino_provider_player_refs'`).Scan(&enabled, &forced)
	}); err != nil || !enabled || !forced {
		t.Fatalf("casino_provider_player_refs must have ENABLE+FORCE row level security: %v %v %v", enabled, forced, err)
	}

	// Tenant B sees none of tenant A's rows, and cannot write one for A.
	var seen int
	if err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_provider_player_refs WHERE tenant_id = $1`, fA.tenantID).Scan(&seen)
	}); err != nil || seen != 0 {
		t.Fatalf("tenant B must read none of tenant A's references: seen=%d err=%v", seen, err)
	}
	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO casino_provider_player_refs (tenant_id, provider_id, player_account_id) VALUES ($1, 'mock-x', $2)`, fA.tenantID, fA.playerAccountID)
		return err
	})
	assertRLSViolation(t, err)

	// Uniqueness: one reference per (tenant, provider, player) ...
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO casino_provider_player_refs (tenant_id, provider_id, player_account_id, player_ref) VALUES ($1, 'mock-casino', $2, gen_random_uuid())`, fA.tenantID, fA.playerAccountID)
		return err
	})
	if !db.IsUniqueViolation(err) {
		t.Fatalf("a second reference for the same (tenant, provider, player) must be a unique violation, got %v", err)
	}
	// ... and one player per (tenant, provider, reference).
	other := seedSecondPlayerFor(t, pool, fA)
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO casino_provider_player_refs (tenant_id, provider_id, player_account_id, player_ref) VALUES ($1, 'mock-casino', $2, $3)`, fA.tenantID, other, fA.playerRef)
		return err
	})
	if !db.IsUniqueViolation(err) {
		t.Fatalf("the same reference for a second player must be a unique violation, got %v", err)
	}

	// Append-only: an UPDATE or DELETE either errors (deny trigger) or, with no
	// UPDATE/DELETE policy, affects no row; either way the reference is intact.
	for _, stmt := range []string{
		`UPDATE casino_provider_player_refs SET player_ref = gen_random_uuid() WHERE tenant_id = $1`,
		`DELETE FROM casino_provider_player_refs WHERE tenant_id = $1`,
	} {
		var affected int64
		err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, stmt, fA.tenantID)
			affected = tag.RowsAffected()
			return err
		})
		if err == nil && affected != 0 {
			t.Fatalf("%q must not change a reference (append-only), affected %d rows", stmt, affected)
		}
	}
	if got := playerRefFor(t, pool, fA.tenantID, "mock-casino", fA.playerAccountID); got != fA.playerRef {
		t.Fatalf("the reference must be unchanged after mutation attempts: %s vs %s", got, fA.playerRef)
	}

	// Minimal runtime grants: the application role may read and insert, never
	// update, delete or truncate (migration 0111 / deploy/init-app-role.sql).
	var roleExists bool
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime')`).Scan(&roleExists)
	}); err != nil {
		t.Fatal(err)
	}
	if roleExists {
		for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false} {
			var has bool
			if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT has_table_privilege('igaming_runtime', 'casino_provider_player_refs', $1)`, priv).Scan(&has)
			}); err != nil {
				t.Fatal(err)
			}
			if has != want {
				t.Fatalf("igaming_runtime %s on casino_provider_player_refs: want %v, got %v", priv, want, has)
			}
		}
	}
}

// seedSecondPlayerFor adds a second player account (no reference) under f's
// tenant and brand and returns its id.
func seedSecondPlayerFor(t *testing.T, pool *db.Pool, f casinoFixture) uuid.UUID {
	t.Helper()
	id, _ := seedSecondPlayerWallet(t, pool, f)
	return id
}
