//go:build integration

package marketgate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/geolocation"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 10, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// world is one tenant bound to its own licence with one brand, plus a
// platform-admin staff principal that authors the configuration rows.
type world struct {
	tenantID  uuid.UUID
	brandID   uuid.UUID
	licenceID uuid.UUID
	admin     uuid.UUID
}

func seedWorld(t *testing.T, pool *db.Pool) world {
	t.Helper()
	w := world{tenantID: uuid.New(), brandID: uuid.New(), licenceID: uuid.New(), admin: uuid.New()}
	jid := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), w.admin, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Gate Tenant', 'under_platform_licence')`,
			w.tenantID, "g-"+w.tenantID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Gate Jurisdiction')`, jid, "GJ-"+jid.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`,
			w.licenceID, jid, "LIC-"+w.licenceID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, w.tenantID, w.licenceID); err != nil {
			return err
		}
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status)
			VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, 'active')`, w.admin, w.admin.String()+"@example.com", personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	err = pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'Gate Brand', 'pending_launch')`,
			w.brandID, w.tenantID, "gb-"+w.brandID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed brand: %v", err)
	}
	return w
}

func actor(id uuid.UUID, reason string) operatingmarket.ActorContext {
	return operatingmarket.ActorContext{ActorID: id, ReasonCode: reason}
}

func (w world) enableCeiling(t *testing.T, pool *db.Pool, cc string) {
	t.Helper()
	err := pool.WithPlatformAdmin(context.Background(), w.admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.CreateLicenceCountryCeilingVersion(ctx, tx, operatingmarket.CreateLicenceCountryCeilingVersionParams{
			LicenceID: w.licenceID, CountryCode: cc, State: operatingmarket.StateEnabled, Status: operatingmarket.StatusActive,
			AuthorizationReference: "legal-ref", Actor: actor(w.admin, "enable-ceiling"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable ceiling: %v", err)
	}
}

func (w world) tenantPolicy(t *testing.T, pool *db.Pool, cc string, state operatingmarket.State) {
	t.Helper()
	if err := w.tenantPolicyErr(pool, cc, state); err != nil {
		t.Fatalf("tenant policy %s/%s: %v", cc, state, err)
	}
}

func (w world) tenantPolicyErr(pool *db.Pool, cc string, state operatingmarket.State) error {
	auth := ""
	if state == operatingmarket.StateEnabled {
		auth = "compliance-ref"
	}
	return pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, operatingmarket.CreateOperatingCountryPolicyVersionParams{
			Scope: operatingmarket.ScopeTenant, TenantID: w.tenantID, CountryCode: cc, State: state, Status: operatingmarket.StatusActive,
			AuthorizationReference: auth, Actor: actor(w.admin, "gate-test"),
		})
		return err
	})
}

func (w world) brandPolicy(t *testing.T, pool *db.Pool, cc string, state operatingmarket.State) {
	t.Helper()
	auth := ""
	if state == operatingmarket.StateEnabled {
		auth = "compliance-ref"
	}
	b := w.brandID
	err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, operatingmarket.CreateOperatingCountryPolicyVersionParams{
			Scope: operatingmarket.ScopeBrand, TenantID: w.tenantID, BrandID: &b, CountryCode: cc, State: state, Status: operatingmarket.StatusActive,
			AuthorizationReference: auth, Actor: actor(w.admin, "gate-test"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("brand policy: %v", err)
	}
}

func (w world) operationPolicy(t *testing.T, pool *db.Pool, cc, op string, product *string, state operatingmarket.State) {
	t.Helper()
	auth := ""
	if state == operatingmarket.StateEnabled {
		auth = "compliance-ref"
	}
	b := w.brandID
	err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, operatingmarket.CreateOperatingCountryPolicyVersionParams{
			Scope: operatingmarket.ScopeOperation, TenantID: w.tenantID, BrandID: &b, OperationCode: op, ProductCode: product,
			CountryCode: cc, State: state, Status: operatingmarket.StatusActive, AuthorizationReference: auth, Actor: actor(w.admin, "gate-test"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("operation policy: %v", err)
	}
}

// openMarket configures a fully-permitting market (ceiling + tenant rung).
func (w world) openMarket(t *testing.T, pool *db.Pool, cc string) {
	t.Helper()
	w.enableCeiling(t, pool, cc)
	w.tenantPolicy(t, pool, cc, operatingmarket.StateEnabled)
}

func (w world) req(op Operation) Request {
	r := Request{TenantID: w.tenantID, BrandID: w.brandID, Operation: op, RequestID: "req-" + uuid.NewString()[:8]}
	if op != OpRegistration {
		r.PlayerAccountID = uuid.New()
	}
	return r
}

// check runs the gate exactly as production does: in a tenant-scoped,
// non-player-scoped transaction that COMMITS on a refusal (the refusal's
// audit row must persist).
func (w world) check(pool *db.Pool, req Request, ev Evidence) (Decision, error) {
	var d Decision
	err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = Check(ctx, tx, req, ev)
		return err
	})
	return d, err
}

func evidenceFor(cc string) Evidence {
	return New(geolocation.NewStaticLocator(cc)).Locate(context.Background(), LocateRequest{})
}

type auditRow struct {
	Action   string
	Outcome  string
	Metadata map[string]any
	ActorTyp string
}

func (w world) refusals(t *testing.T, pool *db.Pool, requestID string) []auditRow {
	t.Helper()
	var out []auditRow
	err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action, outcome, metadata, actor_type FROM audit_log WHERE tenant_id = $1 AND action = $2 AND request_id = $3`,
			w.tenantID, AuditActionRefused, requestID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r auditRow
			var raw []byte
			if err := rows.Scan(&r.Action, &r.Outcome, &raw, &r.ActorTyp); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &r.Metadata); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	return out
}

var allOps = []Operation{OpRegistration, OpDeposit, OpLaunch}

func TestCheck_AllowedMarketPassesAtEveryPoint(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	for _, op := range allOps {
		req := w.req(op)
		d, err := w.check(pool, req, evidenceFor("MT"))
		if err != nil || !d.Allowed || d.Code != "" {
			t.Fatalf("%s: want allowed, got %+v err=%v", op, d, err)
		}
		if got := w.refusals(t, pool, req.RequestID); len(got) != 0 {
			t.Fatalf("%s: an allowed request must not write a refusal row, got %d", op, len(got))
		}
	}
}

func TestCheck_NoMarketConfigurationFailsClosedAtEveryPoint(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool) // brand with NO operating-market rows at all
	for _, op := range allOps {
		req := w.req(op)
		d, err := w.check(pool, req, evidenceFor("MT"))
		if err != nil || d.Allowed || d.Code != ReasonNotPermitted {
			t.Fatalf("%s: unconfigured brand must be refused with %s, got %+v err=%v", op, ReasonNotPermitted, d, err)
		}
		rows := w.refusals(t, pool, req.RequestID)
		if len(rows) != 1 {
			t.Fatalf("%s: want exactly 1 refusal audit row, got %d", op, len(rows))
		}
	}
}

func TestCheck_BlockedMarketRefusedAtEveryPoint(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	w.enableCeiling(t, pool, "DE")
	// DE: ceiling enabled but the tenant never opted in -> not_configured.
	// MT: tenant disabled after being enabled.
	w.tenantPolicy(t, pool, "MT", operatingmarket.StateDisabled)
	for _, cc := range []string{"MT", "DE", "FR"} { // disabled, not opted in, no ceiling
		for _, op := range allOps {
			d, err := w.check(pool, w.req(op), evidenceFor(cc))
			if err != nil || d.Allowed || d.Code != ReasonNotPermitted {
				t.Fatalf("%s/%s: want refusal %s, got %+v err=%v", cc, op, ReasonNotPermitted, d, err)
			}
		}
	}
}

func TestCheck_NarrowerRungsBlockOnlyTheirOperation(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	casino := "casino"
	w.operationPolicy(t, pool, "MT", "deposit", nil, operatingmarket.StateDisabled)
	w.operationPolicy(t, pool, "MT", "wagering", &casino, operatingmarket.StateDisabled)
	want := map[Operation]bool{OpRegistration: true, OpDeposit: false, OpLaunch: false}
	for op, allowed := range want {
		d, err := w.check(pool, w.req(op), evidenceFor("MT"))
		if err != nil || d.Allowed != allowed {
			t.Fatalf("%s: want allowed=%v, got %+v err=%v", op, allowed, d, err)
		}
	}
}

func TestCheck_BrandRungDisableBlocksEveryOperation(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	w.brandPolicy(t, pool, "MT", operatingmarket.StateDisabled)
	for _, op := range allOps {
		d, err := w.check(pool, w.req(op), evidenceFor("MT"))
		if err != nil || d.Allowed {
			t.Fatalf("%s: brand disable must refuse, got %+v err=%v", op, d, err)
		}
	}
}

func TestCheck_UnknownEvidenceFailsClosedEvenWhenMarketIsOpen(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	unknowns := map[string]Evidence{
		"zero value":      {},
		"default static":  New(geolocation.NewStaticLocator("")).Locate(context.Background(), LocateRequest{}),
		"nil locator":     New(nil).Locate(context.Background(), LocateRequest{}),
		"erroring vendor": New(&fakeLocator{err: errors.New("down")}).Locate(context.Background(), LocateRequest{}),
	}
	for name, ev := range unknowns {
		for _, op := range allOps {
			req := w.req(op)
			d, err := w.check(pool, req, ev)
			if err != nil || d.Allowed || d.Code != ReasonEvidenceUnavailable {
				t.Fatalf("%s/%s: want %s, got %+v err=%v", name, op, ReasonEvidenceUnavailable, d, err)
			}
			rows := w.refusals(t, pool, req.RequestID)
			if len(rows) != 1 || rows[0].Metadata["evidence"] != "unknown" {
				t.Fatalf("%s/%s: want 1 audit row with evidence=unknown, got %+v", name, op, rows)
			}
		}
	}
}

func TestCheck_ConfigurationChangeTakesEffectImmediatelyAndIsVersioned(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	ev := evidenceFor("MT")
	must := func(want bool, step string) {
		t.Helper()
		d, err := w.check(pool, w.req(OpDeposit), ev)
		if err != nil || d.Allowed != want {
			t.Fatalf("%s: want allowed=%v, got %+v err=%v", step, want, d, err)
		}
	}
	must(true, "initially open")
	w.tenantPolicy(t, pool, "MT", operatingmarket.StateDisabled)
	must(false, "immediately after disable (next request, no cache)")
	w.tenantPolicy(t, pool, "MT", operatingmarket.StateEnabled)
	must(true, "immediately after re-enable")
	w.operationPolicy(t, pool, "MT", "deposit", nil, operatingmarket.StateDisabled)
	must(false, "immediately after an operation-rung disable")

	// Versioned: the history keeps every tenant-rung version.
	var versions []operatingmarket.PolicyRecord
	err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		versions, err = operatingmarket.ListOperatingCountryPolicyVersions(ctx, tx, operatingmarket.HistoryQuery{
			TenantID: w.tenantID, Scope: operatingmarket.ScopeTenant, CountryCode: "MT",
		})
		return err
	})
	if err != nil || len(versions) != 3 {
		t.Fatalf("want 3 tenant-rung versions (enabled, disabled, enabled), got %d err=%v", len(versions), err)
	}
}

func TestCheck_TenantIsolation(t *testing.T) {
	pool := testPool(t)
	a := seedWorld(t, pool)
	b := seedWorld(t, pool)
	a.openMarket(t, pool, "MT")
	// Tenant B has no configuration: A's open market must not leak to it.
	d, err := b.check(pool, b.req(OpDeposit), evidenceFor("MT"))
	if err != nil || d.Allowed {
		t.Fatalf("tenant B must be refused despite tenant A's open market, got %+v err=%v", d, err)
	}
	// B's transaction asked about A's identifiers: the scope assertion
	// refuses (an error, which the caller treats as a refusal).
	err = pool.WithTenant(context.Background(), b.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Check(ctx, tx, a.req(OpDeposit), evidenceFor("MT"))
		return err
	})
	if err == nil || !errors.Is(err, operatingmarket.ErrTransactionScope) {
		t.Fatalf("a tenant-B transaction checking tenant A's request must fail with ErrTransactionScope, got %v", err)
	}
	// And the reverse: A is unaffected by B.
	d, err = a.check(pool, a.req(OpDeposit), evidenceFor("MT"))
	if err != nil || !d.Allowed {
		t.Fatalf("tenant A must still be allowed, got %+v err=%v", d, err)
	}
}

func TestCheck_PlayerScopedTransactionFailsClosed(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	player := uuid.New()
	err := pool.WithPlayerScope(context.Background(), w.tenantID, player, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Check(ctx, tx, w.req(OpDeposit), evidenceFor("MT"))
		return err
	})
	if err == nil {
		t.Fatal("a player-scoped transaction must not be accepted (it cannot read policy rows): expected an error")
	}
}

func TestCheck_InvalidRequestsAreErrors(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	bad := []Request{
		{TenantID: uuid.Nil, BrandID: w.brandID, Operation: OpDeposit},
		{TenantID: w.tenantID, BrandID: uuid.Nil, Operation: OpDeposit},
		{TenantID: w.tenantID, BrandID: w.brandID, Operation: Operation("withdrawal")},
		{TenantID: w.tenantID, BrandID: w.brandID, Operation: ""},
	}
	for i, r := range bad {
		d, err := w.check(pool, r, evidenceFor("MT"))
		if err == nil || d.Allowed || !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("case %d: want ErrInvalidRequest and not allowed, got %+v err=%v", i, d, err)
		}
	}
}

func TestCheck_RefusalAuditCarriesIdsAndCodesOnly(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	w.tenantPolicy(t, pool, "MT", operatingmarket.StateDisabled)
	req := w.req(OpDeposit)
	if d, err := w.check(pool, req, evidenceFor("MT")); err != nil || d.Allowed {
		t.Fatalf("want refusal, got %+v err=%v", d, err)
	}
	rows := w.refusals(t, pool, req.RequestID)
	if len(rows) != 1 {
		t.Fatalf("want 1 audit row, got %d", len(rows))
	}
	r := rows[0]
	if r.Outcome != "denied" || r.ActorTyp != "player" {
		t.Fatalf("unexpected audit row: %+v", r)
	}
	if r.Metadata["operation"] != "deposit" || r.Metadata["reason_code"] != ReasonNotPermitted ||
		r.Metadata["evidence"] != "resolved" || r.Metadata["market_outcome"] != string(operatingmarket.OutcomeDisabledByTenant) ||
		r.Metadata["policy_version"] != operatingmarket.PolicyVersion || r.Metadata["brand_id"] != w.brandID.String() {
		t.Fatalf("unexpected audit metadata: %+v", r.Metadata)
	}
	raw, _ := json.Marshal(r.Metadata)
	if strings.Contains(string(raw), "MT") || strings.Contains(strings.ToLower(string(raw)), "country") {
		t.Fatalf("audit metadata leaks the evidenced country: %s", raw)
	}
	allowedKeys := map[string]bool{"operation": true, "reason_code": true, "evidence": true, "brand_id": true, "market_outcome": true, "policy_version": true}
	for k := range r.Metadata {
		if !allowedKeys[k] {
			t.Fatalf("unexpected audit metadata key %q in %s", k, raw)
		}
	}
}

// A registration refusal has no player: the actor is the system and the
// row carries no player id.
func TestCheck_RegistrationRefusalAuditHasNoPlayer(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	req := w.req(OpRegistration)
	if d, err := w.check(pool, req, Evidence{}); err != nil || d.Allowed {
		t.Fatalf("want refusal, got %+v err=%v", d, err)
	}
	rows := w.refusals(t, pool, req.RequestID)
	if len(rows) != 1 || rows[0].ActorTyp != "system" {
		t.Fatalf("want 1 system-actor row, got %+v", rows)
	}
}

// The refusal row must survive: Check returns a Decision (not an error),
// so the surrounding transaction commits it. If Check returned an error
// for a refusal, WithTenant would roll the audit row back (the
// LaunchGameResult.Denied lesson).
func TestCheck_RefusalAuditIsCommittedWithTheSurroundingTransaction(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	req := w.req(OpLaunch)
	var d Decision
	if err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = Check(ctx, tx, req, evidenceFor("MT"))
		return err
	}); err != nil || d.Allowed {
		t.Fatalf("want refusal without error, got %+v err=%v", d, err)
	}
	if got := w.refusals(t, pool, req.RequestID); len(got) != 1 {
		t.Fatalf("the refusal audit row did not persist: %d", len(got))
	}
}

// Race: requests run concurrently with configuration writes. Every
// decision must be a clean allow/refuse (never an error other than a
// diagnosed concurrent-write), the final state must be honoured by the
// very next request, and nothing may panic or deadlock. Run with -race.
func TestCheck_ConcurrentConfigChangeVersusRequests(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	ev := evidenceFor("MT")

	const writers, readers, rounds = 2, 8, 12
	var wg sync.WaitGroup
	var checkErrs, allowed, refused atomic.Int64
	stop := make(chan struct{})

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				d, err := w.check(pool, w.req(OpDeposit), ev)
				switch {
				case err != nil:
					checkErrs.Add(1)
				case d.Allowed:
					allowed.Add(1)
				default:
					refused.Add(1)
				}
			}
		}()
	}
	var wwg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wwg.Add(1)
		go func(i int) {
			defer wwg.Done()
			for r := 0; r < rounds; r++ {
				state := operatingmarket.StateDisabled
				if (r+i)%2 == 0 {
					state = operatingmarket.StateEnabled
				}
				// A concurrent write for the same key may legitimately lose
				// (ErrConcurrentPolicyWrite); anything else is a failure.
				if err := w.tenantPolicyErr(pool, "MT", state); err != nil && !errors.Is(err, operatingmarket.ErrConcurrentPolicyWrite) {
					t.Errorf("writer %d round %d: %v", i, r, err)
				}
			}
		}(i)
	}
	wwg.Wait()
	close(stop)
	wg.Wait()

	if checkErrs.Load() != 0 {
		t.Fatalf("%d gate evaluations errored under concurrent configuration change", checkErrs.Load())
	}
	if allowed.Load()+refused.Load() == 0 {
		t.Fatal("no request completed")
	}

	// Deterministic tail: whatever the interleaving, the committed final
	// configuration is what the next request sees.
	w.tenantPolicy(t, pool, "MT", operatingmarket.StateDisabled)
	if d, err := w.check(pool, w.req(OpDeposit), ev); err != nil || d.Allowed {
		t.Fatalf("after the final disable the next request must be refused, got %+v err=%v", d, err)
	}
	w.tenantPolicy(t, pool, "MT", operatingmarket.StateEnabled)
	if d, err := w.check(pool, w.req(OpDeposit), ev); err != nil || !d.Allowed {
		t.Fatalf("after the final enable the next request must be allowed, got %+v err=%v", d, err)
	}
}

// A refusal row is written once per refused evaluation (race-safe: N
// concurrent refused requests -> N rows, none lost).
func TestCheck_ConcurrentRefusalsAreEachAudited(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool) // unconfigured -> every request refused
	const n = 16
	var wg sync.WaitGroup
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		req := w.req(OpLaunch)
		ids[i] = req.RequestID
		go func() {
			defer wg.Done()
			if d, err := w.check(pool, req, evidenceFor("MT")); err != nil || d.Allowed {
				t.Errorf("want refusal, got %+v err=%v", d, err)
			}
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if got := w.refusals(t, pool, id); len(got) != 1 {
			t.Fatalf("request %s: want exactly 1 refusal row, got %d", id, len(got))
		}
	}
}

func TestGuard_DelegatesToCheck(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	g := New(geolocation.NewStaticLocator("MT"))
	ev := g.Locate(context.Background(), LocateRequest{TenantID: w.tenantID, BrandID: w.brandID})
	var d Decision
	err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = g.Guard(w.req(OpDeposit), ev)(ctx, tx)
		return err
	})
	if err != nil || !d.Allowed {
		t.Fatalf("guard: want allowed, got %+v err=%v", d, err)
	}
}
