//go:build integration

// PRH-I2 (ADR 0095 §15.1): the casino launch two-phase split. These tests
// target properties that did not exist before the split - no transaction
// held across a provider call, the per-call CallContext's own fields, and
// the new fail-closed branches (nil pool, nil outbound resolver, a
// mismatched resolved credential) - which the broader Stage 4A suite
// (orchestrator_integration_test.go and friends) does not exercise because
// they always wire a well-behaved pool and resolver.
package casino

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// testPoolSized mirrors testPool (orchestrator_integration_test.go) but
// with an explicit, tiny connection-pool bound - used only to PROVE a
// connection is genuinely free (tryAcquireConnection), not merely idle by
// happenstance.
func testPoolSized(t *testing.T, maxConns int32) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, maxConns, 5_000_000_000)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// spyLaunchProvider wraps a *MockCasinoProvider and lets a test observe/
// override exactly the two methods LaunchGame calls OUTSIDE any
// transaction (HealthStatus, Launch) - every other CasinoProvider method
// is the embedded mock's own untouched behavior.
type spyLaunchProvider struct {
	*MockCasinoProvider
	onLaunch       func(ctx context.Context, req LaunchRequest) (LaunchResult, error)
	onHealthStatus func(ctx context.Context) (ProviderHealth, error)
}

func (s *spyLaunchProvider) Launch(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
	if s.onLaunch != nil {
		return s.onLaunch(ctx, req)
	}
	return s.MockCasinoProvider.Launch(ctx, req)
}

func (s *spyLaunchProvider) HealthStatus(ctx context.Context) (ProviderHealth, error) {
	if s.onHealthStatus != nil {
		return s.onHealthStatus(ctx)
	}
	return s.MockCasinoProvider.HealthStatus(ctx)
}

// TestLaunchGame_NoConnectionHeldAcrossHealthAndLaunchCalls is the two-
// phase shape's own load-bearing property (ADR 0095 §15.1): phase A's
// transaction must have committed - releasing its pooled connection -
// before HealthStatus or Launch ever runs. Proven here with a pool sized
// to exactly ONE connection: if phase A's connection were still held while
// the spy provider's Launch runs, this test's own concurrent WithTenant
// call from inside Launch would time out waiting for a second connection
// that does not exist. It also asserts txscope.Held(ctx) is false inside
// Launch - the same defence-in-depth signal providercred.OutboundResolver
// itself checks.
func TestLaunchGame_NoConnectionHeldAcrossHealthAndLaunchCalls(t *testing.T) {
	singleConnPool := testPoolSized(t, 1)
	f := seedCasinoFixture(t, singleConnPool)
	game := seedGame(t, singleConnPool, "mock-casino", "EUR")
	enableGameForTenant(t, singleConnPool, f, game.ID)

	base := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, singleConnPool, f, base, 100)

	var sawHeldDuringHealth, sawHeldDuringLaunch bool
	var acquireErrHealth, acquireErrLaunch error
	provider := &spyLaunchProvider{MockCasinoProvider: base}
	provider.onHealthStatus = func(ctx context.Context) (ProviderHealth, error) {
		sawHeldDuringHealth = txscope.Held(ctx)
		acquireErrHealth = tryAcquireConnection(singleConnPool, f.tenantID)
		return base.HealthStatus(ctx)
	}
	provider.onLaunch = func(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
		sawHeldDuringLaunch = txscope.Held(ctx)
		acquireErrLaunch = tryAcquireConnection(singleConnPool, f.tenantID)
		return base.Launch(ctx, req)
	}

	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(base))
	result, err := orch.LaunchGame(context.Background(), singleConnPool, NewMockOutboundResolver(), LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
	})
	if err != nil {
		t.Fatalf("LaunchGame: %v", err)
	}
	if result.SessionID == uuid.Nil {
		t.Fatal("expected a session id")
	}
	if sawHeldDuringHealth {
		t.Fatal("txscope reported a transaction held during HealthStatus - phase A must have committed first")
	}
	if sawHeldDuringLaunch {
		t.Fatal("txscope reported a transaction held during Launch - phase A must have committed first")
	}
	if acquireErrHealth != nil {
		t.Fatalf("could not acquire the pool's only connection during HealthStatus (phase A's connection was still held): %v", acquireErrHealth)
	}
	if acquireErrLaunch != nil {
		t.Fatalf("could not acquire the pool's only connection during Launch (phase A's connection was still held): %v", acquireErrLaunch)
	}
}

// tryAcquireConnection proves a pooled connection is genuinely free by
// actually taking it (a short WithTenant round trip), not by inspecting
// pool statistics - on a pool sized to one connection this can only
// succeed if nothing else currently holds it.
func tryAcquireConnection(pool *db.Pool, tenantID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1`)
		return err
	})
}

// TestLaunchGame_NilPoolFailsClosedWithoutPanic: PRH-I2's own fail-closed
// convention (mirroring every other nil-resolver branch in this package) -
// no transaction runner configured must be a clean, typed error, never a
// nil-pointer panic reaching the caller.
func TestLaunchGame_NilPoolFailsClosedWithoutPanic(t *testing.T) {
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": NewMockCasinoProvider("mock-casino", "EUR")}, nil)
	_, err := orch.LaunchGame(context.Background(), nil, NewMockOutboundResolver(), LaunchGameParams{
		TenantID: uuid.New(), BrandID: uuid.New(), PlayerAccountID: uuid.New(), WalletID: uuid.New(),
		GameID: uuid.New(), AssetCode: "EUR", Mode: ModeReal,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for a nil pool, got %v", err)
	}
}

// TestLaunchGame_NilOutboundResolverFailsClosedAndRevokesSession: phase A
// still commits the session (the eligibility/capability checks ran and
// passed), but phase B refuses to call Launch with no credential resolver
// configured - so phase C must revoke the session exactly like any other
// launch failure, never leave it usable.
func TestLaunchGame_NilOutboundResolverFailsClosedAndRevokesSession(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	result, err := orch.LaunchGame(context.Background(), pool, nil, LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for a nil outbound resolver, got %v", err)
	}
	if result.SessionID != uuid.Nil {
		t.Fatalf("expected a zero LaunchGameResult on failure, got %+v", result)
	}
	assertSoleSessionRevoked(t, pool, f)
	if !auditActionExists(t, pool, f.tenantID, "casino.launch_failed") {
		t.Fatal("expected a casino.launch_failed audit record")
	}
}

// TestLaunchGame_CredentialBindingMismatchFailsClosedAndRevokesSession is
// the defence-in-depth binding check (ADR 0095 §9.1/S95-C8(b)): a resolver
// that hands back a credential for the WRONG tenant must never reach
// Launch - LaunchGame catches it itself rather than trusting the resolver
// alone.
func TestLaunchGame_CredentialBindingMismatchFailsClosedAndRevokesSession(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	wrongTenantResolver := mismatchedOutboundResolver{}
	result, err := orch.LaunchGame(context.Background(), pool, wrongTenantResolver, LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for a mismatched credential, got %v", err)
	}
	if result.SessionID != uuid.Nil {
		t.Fatalf("expected a zero LaunchGameResult on failure, got %+v", result)
	}
	assertSoleSessionRevoked(t, pool, f)
}

// TestLaunchGame_AuditsLaunchRequestedInPhaseAAndLaunchedInPhaseC pins the
// two separate audit records ADR 0095 §15.1 names: "casino.launch_requested"
// commits with phase A (before any provider call), and "casino.launched"
// commits with phase C (after Launch succeeds) - two rows, not one, and
// neither substituting for the other.
func TestLaunchGame_AuditsLaunchRequestedInPhaseAAndLaunchedInPhaseC(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	_, err := orch.LaunchGame(context.Background(), pool, NewMockOutboundResolver(), LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
	})
	if err != nil {
		t.Fatalf("LaunchGame: %v", err)
	}
	if !auditActionExists(t, pool, f.tenantID, "casino.launch_requested") {
		t.Fatal("expected a casino.launch_requested audit record from phase A")
	}
	if !auditActionExists(t, pool, f.tenantID, "casino.launched") {
		t.Fatal("expected a casino.launched audit record from phase C")
	}
}

// TestLaunchGame_ProviderDeclinedRevokesSession: phase A commits the
// session; phase B's Launch call returns a well-formed but non-Succeeded
// outcome (OutcomeDeclined, MockCasinoProvider's own decline signal, per
// its ProviderGameID convention) - never a transport error - and phase C
// must revoke it exactly like a transport failure, never treat a declined
// launch as success.
func TestLaunchGame_ProviderDeclinedRevokesSession(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)

	var game Game
	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		game, err = UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: "mock-casino", ProviderGameID: MockGameIDDeclineLaunch,
			Name: "Decline Game", GameType: "slot", SupportedAssets: []string{"EUR"}, Status: GameStatusActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed decline-launch game: %v", err)
	}
	enableGameForTenant(t, pool, f, game.ID)

	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))
	result, err := orch.LaunchGame(context.Background(), pool, NewMockOutboundResolver(), LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
	})
	if err == nil {
		t.Fatal("expected an error for a provider-declined launch")
	}
	if result.SessionID != uuid.Nil || result.LaunchURL != "" {
		t.Fatalf("expected a zero LaunchGameResult on decline, got %+v", result)
	}
	assertSoleSessionRevoked(t, pool, f)
	if !auditActionExists(t, pool, f.tenantID, "casino.launch_failed") {
		t.Fatal("expected a casino.launch_failed audit record")
	}
}

type mismatchedOutboundResolver struct{}

func (mismatchedOutboundResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, _ uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	// Always resolves for a DIFFERENT tenant than the one asked for.
	return providercred.NewMockOutboundCredential(uuid.New(), "casino", providerID), nil
}

// TestLaunchGame_CallContextFieldsPassedToProvider pins the exact shape
// ADR 0095 §15.1/§9.1 specifies: TenantID/ProviderID match the launch,
// Credential.Domain is "casino", and IdempotencyKey is "cas:" + the
// session's own id - the deterministic external reference this ADR names.
func TestLaunchGame_CallContextFieldsPassedToProvider(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)

	base := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, base, 100)

	var captured LaunchRequest
	provider := &spyLaunchProvider{MockCasinoProvider: base}
	provider.onLaunch = func(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
		captured = req
		return base.Launch(ctx, req)
	}

	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(base))
	result, err := orch.LaunchGame(context.Background(), pool, NewMockOutboundResolver(), LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
	})
	if err != nil {
		t.Fatalf("LaunchGame: %v", err)
	}
	if captured.Call.TenantID != f.tenantID {
		t.Fatalf("expected Call.TenantID = %s, got %s", f.tenantID, captured.Call.TenantID)
	}
	if captured.Call.ProviderID != "mock-casino" {
		t.Fatalf("expected Call.ProviderID = mock-casino, got %s", captured.Call.ProviderID)
	}
	if captured.Call.Credential.Domain != "casino" {
		t.Fatalf("expected Call.Credential.Domain = casino, got %s", captured.Call.Credential.Domain)
	}
	if captured.Call.Credential.TenantID != f.tenantID {
		t.Fatalf("expected Call.Credential.TenantID = %s, got %s", f.tenantID, captured.Call.Credential.TenantID)
	}
	want := "cas:" + result.SessionID.String()
	if captured.Call.IdempotencyKey != want {
		t.Fatalf("expected Call.IdempotencyKey = %q, got %q", want, captured.Call.IdempotencyKey)
	}
	if captured.Call.Deadline.IsZero() {
		t.Fatal("expected a non-zero Call.Deadline")
	}
}

// TestLaunchGame_CtxCancelledDuringLaunch_StillRevokesAndAudits is
// security review RV-PRH-I2 C1 / code review R1's required test: a client
// disconnect mid-Launch cancels the request ctx BEFORE Launch itself
// returns its (transport) error. Phase C must still run - on its own
// ctx-independent, bounded timeout - and revoke the session plus write the
// casino.launch_failed audit, exactly as if the ctx had never been
// cancelled. A subsequent bet against that session is then rejected, with
// the ledger left exactly balanced.
func TestLaunchGame_CtxCancelledDuringLaunch_StillRevokesAndAudits(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)

	base := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, base, 100)

	ctx, cancel := context.WithCancel(context.Background())
	provider := &spyLaunchProvider{MockCasinoProvider: base}
	provider.onLaunch = func(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
		// Simulate the client's connection dropping WHILE Launch is in
		// flight, exactly like a real HTTP handler's r.Context() being
		// cancelled mid-request - the cancellation happens strictly BEFORE
		// Launch returns its own transport error.
		cancel()
		return LaunchResult{}, errors.New("simulated transport failure after client disconnect")
	}

	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(base))
	_, err := orch.LaunchGame(ctx, pool, NewMockOutboundResolver(), LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
	})
	if err == nil {
		t.Fatal("expected an error for a provider transport failure")
	}
	if ctx.Err() == nil {
		t.Fatal("test setup: expected the request ctx to actually be cancelled by the time LaunchGame returned")
	}

	// Despite the cancelled request ctx, phase C must have run to
	// completion: the session is revoked, not left 'active'.
	assertSoleSessionRevoked(t, pool, f)
	if !auditActionExists(t, pool, f.tenantID, "casino.launch_failed") {
		t.Fatal("expected a casino.launch_failed audit record even though the request ctx was cancelled")
	}

	// The revoked session can never be used to move money.
	var sessionID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM casino_launch_sessions WHERE tenant_id = $1 AND player_account_id = $2`,
			f.tenantID, f.playerAccountID).Scan(&sessionID)
	}); err != nil {
		t.Fatalf("read session id: %v", err)
	}
	debitsBefore, creditsBefore := sumDebitsCredits(t, pool, f.tenantID)
	payload := base.CallbackPayload(f.tenantID, CallbackEventBet, "bet-ctx-cancel", "", "round-ctx-cancel", "game-1", 100, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if !errors.Is(err, ErrLaunchSessionRequired) {
		t.Fatalf("expected ErrLaunchSessionRequired for a bet against the revoked session, got %v", err)
	}
	debitsAfter, creditsAfter := sumDebitsCredits(t, pool, f.tenantID)
	if debitsAfter != creditsAfter || debitsAfter != debitsBefore || creditsAfter != creditsBefore {
		t.Fatalf("expected no ledger effect: before=(%d,%d) after=(%d,%d)", debitsBefore, creditsBefore, debitsAfter, creditsAfter)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected the balance untouched, got %d", balance)
	}
}

// TestLaunchGame_CtxCancelledAfterVendorAccept_StillAuditsLaunched is ADR
// 0095 §16.2 item 18's "crash after vendor accept" case. A genuine process
// crash cannot be simulated in-process; a cancelled request ctx right after
// Launch returns success is this codebase's practical equivalent (the same
// proxy the R1 fix's own ctx-cancel test above uses for "crash after
// phase A"/mid-Launch). Phase C's success path must still commit the
// casino.launched audit on its own ctx-independent timeout, and the
// session must stay usable (a bet against it succeeds) - the vendor really
// did accept the launch, so this is NOT a failure to revoke.
func TestLaunchGame_CtxCancelledAfterVendorAccept_StillAuditsLaunched(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)

	base := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, base, 100)

	ctx, cancel := context.WithCancel(context.Background())
	provider := &spyLaunchProvider{MockCasinoProvider: base}
	provider.onLaunch = func(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
		result, err := base.Launch(ctx, req)
		// The vendor accepted BEFORE the client's own connection drops -
		// exactly "crash after vendor accept": the response is already
		// decided, only the platform's OWN follow-up (the success audit)
		// still has to run.
		cancel()
		return result, err
	}

	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(base))
	result, err := orch.LaunchGame(ctx, pool, NewMockOutboundResolver(), LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
	})
	if err != nil {
		t.Fatalf("expected the launch to succeed despite the ctx being cancelled right after Launch returned, got %v", err)
	}
	if result.SessionID == uuid.Nil {
		t.Fatal("expected a session id")
	}
	if !auditActionExists(t, pool, f.tenantID, "casino.launched") {
		t.Fatal("expected a casino.launched audit record even though the request ctx was cancelled right after Launch returned")
	}

	// The session is genuinely usable - the vendor really did accept it.
	payload := base.CallbackPayload(f.tenantID, CallbackEventBet, "bet-after-vendor-accept", "", "round-after-vendor-accept", "game-1", 500, "EUR", OutcomeSucceeded, "", f.playerAccountID, result.SessionID)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("expected the bet to post against the genuinely-accepted session, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 4500 {
		t.Fatalf("expected cash balance 4500 (5000 - 500 bet), got %d", balance)
	}
}

// TestLaunchGame_CircuitOpenRevokesAndAudits closes code review F1's
// circuit-open gap: the existing TestLaunchGame_UnhealthyProviderCircuitOpenRejected
// asserts only the error; this asserts the session is actually revoked and
// audited too (a mutant deleting the revoke on this specific branch alone
// would otherwise survive).
func TestLaunchGame_CircuitOpenRevokesAndAudits(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	provider.SetHealth(ProviderHealth{CircuitState: CircuitOpen})
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	_, err := orch.LaunchGame(context.Background(), pool, NewMockOutboundResolver(), LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for an open circuit, got %v", err)
	}
	assertSoleSessionRevoked(t, pool, f)
	if !auditActionExists(t, pool, f.tenantID, "casino.launch_failed") {
		t.Fatal("expected a casino.launch_failed audit record for a circuit-open launch")
	}
}

// TestLaunchGame_ResolverErrorRevokesAuditsAndMapsToProviderUnavailable
// closes code review F1/F2.1's resolver-error gap: an outbound-credential
// resolution failure is a launch failure like any other (revoked + audited)
// and, per the fix, maps to errors.Is(ErrProviderUnavailable) - a 503 to
// the player, not a generic 500 - since an unconfigured/rotated vendor
// credential is not something the player caused.
func TestLaunchGame_ResolverErrorRevokesAuditsAndMapsToProviderUnavailable(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	_, err := orch.LaunchGame(context.Background(), pool, failingOutboundResolver{}, LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for a resolver error, got %v", err)
	}
	assertSoleSessionRevoked(t, pool, f)
	if !auditActionExists(t, pool, f.tenantID, "casino.launch_failed") {
		t.Fatal("expected a casino.launch_failed audit record for a resolver error")
	}
}

type failingOutboundResolver struct{}

func (failingOutboundResolver) Resolve(context.Context, providercred.TenantTxRunner, uuid.UUID, string) (providercred.OutboundCredential, error) {
	return providercred.OutboundCredential{}, errors.New("simulated credential store outage")
}

// assertSoleSessionRevoked reads f's one launch session and requires it be
// 'revoked' - the phase C outcome for every launch failure after phase A's
// session mint.
func assertSoleSessionRevoked(t *testing.T, pool *db.Pool, f casinoFixture) {
	t.Helper()
	var count int
	var status LaunchSessionStatus
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM casino_launch_sessions WHERE tenant_id = $1 AND player_account_id = $2`,
			f.tenantID, f.playerAccountID).Scan(&count); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT status FROM casino_launch_sessions WHERE tenant_id = $1 AND player_account_id = $2`,
			f.tenantID, f.playerAccountID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("read launch session: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one launch session, got %d", count)
	}
	if status != LaunchSessionRevoked {
		t.Fatalf("expected the session revoked, got status %q", status)
	}
}
