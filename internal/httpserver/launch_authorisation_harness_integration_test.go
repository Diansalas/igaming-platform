//go:build integration

// ADR 0112 slice 4: shared harness of the launch authorisation HTTP integration tests. Every
// assertion about what the application can do runs through the real HTTP stack as the
// igaming_runtime role (non-superuser, non-BYPASSRLS, asserted); the owner pool only seeds
// fixtures (the LF2 path) and reads for assertions. Requires TEST_DATABASE_URL and
// TEST_RUNTIME_DATABASE_URL (a skip is NOT evidence).
package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

type lhWorld struct {
	t      *testing.T
	owner  *db.Pool
	rt     *db.Pool
	api    *cgAPI
	svc    *launchgov.Service
	ops    launchfix.OperatorIDs
	issuer *auth.Issuer
}

func lhConnect(t *testing.T, u string) *db.Pool {
	t.Helper()
	p, err := db.Connect(context.Background(), u, 40, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func newLHWorld(t *testing.T) *lhWorld {
	t.Helper()
	ou, ru := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if ou == "" || ru == "" {
		t.Skip("TEST_DATABASE_URL and TEST_RUNTIME_DATABASE_URL are required (a skip is not evidence)")
	}
	w := &lhWorld{t: t, owner: lhConnect(t, ou), rt: lhConnect(t, ru)}
	var who string
	var super, bypass bool
	if err := w.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user::text, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&who, &super, &bypass)
	}); err != nil {
		t.Fatal(err)
	}
	if who != "igaming_runtime" || super || bypass {
		t.Fatalf("the HTTP server must run as igaming_runtime, non-superuser, non-BYPASSRLS; got %q super=%v bypass=%v", who, super, bypass)
	}
	prooftest.Install(t, ou)
	launchfix.SeedOperators(t)
	w.ops = launchfix.Operators()

	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	w.issuer = auth.NewIssuer(keys, "lh-test", "lh-test")
	w.svc = w.service(launchgov.Config{})
	w.serve(w.svc)
	return w
}

func (w *lhWorld) service(cfg launchgov.Config) *launchgov.Service {
	w.t.Helper()
	cfg.Pool = w.rt
	if cfg.Environment == "" {
		cfg.Environment = "test"
	}
	if cfg.LockTimeout == "" {
		cfg.LockTimeout = "2s"
	}
	if cfg.Backoff == nil {
		cfg.Backoff = func(n int) time.Duration { return 20 * time.Millisecond * time.Duration(n) }
	}
	s, err := launchgov.New(cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	return s
}

// serve (re)starts the HTTP server over the runtime pool with the given launch service.
func (w *lhWorld) serve(svc *launchgov.Service) {
	w.t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:              slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                  w.rt,
		AuthIssuer:          w.issuer,
		ServiceName:         "lh-test",
		AccessTokenTTL:      time.Hour,
		RefreshTokenTTL:     time.Hour,
		PersonResolver:      identityresolution.NewMockPersonResolver(),
		LaunchAuthorisation: svc,
	}))
	w.t.Cleanup(srv.Close)
	w.api = &cgAPI{t: w.t, pool: w.rt, issuer: w.issuer, srv: srv}
}

func (w *lhWorld) do(method, path, token string, body any) cgResp {
	w.t.Helper()
	return w.api.do(method, path, token, body)
}

// platform tokens: the fixture requester and the two fixture approvers (three distinct Persons).
func (w *lhWorld) tokReq() string {
	return w.api.token(w.ops.Requester, uuid.Nil, auth.RolePlatformAdmin)
}
func (w *lhWorld) tokAp1() string {
	return w.api.token(w.ops.Approver1, uuid.Nil, auth.RolePlatformAdmin)
}
func (w *lhWorld) tokAp2() string {
	return w.api.token(w.ops.Approver2, uuid.Nil, auth.RolePlatformAdmin)
}

// ---- fixtures ---------------------------------------------------------------------------

func (w *lhWorld) activeTenant() uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "lh", "lh-"+uuid.NewString(), "under_platform_licence")
		id = tn.ID
		return err
	}); err != nil {
		w.t.Fatalf("seed active tenant: %v", err)
	}
	return id
}

func (w *lhWorld) pendingTenant() uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.rt.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := identity.CreateTenant(ctx, tx, "lh pending", "lh-p-"+uuid.NewString(), "under_platform_licence")
		id = tn.ID
		return err
	}); err != nil {
		w.t.Fatalf("create pending tenant: %v", err)
	}
	return id
}

func (w *lhWorld) activeBrand(tenantID uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := launchfix.CreateBrand(ctx, tx, tenantID, "lh brand", "lh-b-"+uuid.NewString())
		id = b.ID
		return err
	}); err != nil {
		w.t.Fatalf("seed active brand: %v", err)
	}
	return id
}

func (w *lhWorld) pendingBrand(tenantID uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := identity.CreateBrand(ctx, tx, tenantID, "lh pending brand", "lh-pb-"+uuid.NewString())
		id = b.ID
		return err
	}); err != nil {
		w.t.Fatalf("create pending brand: %v", err)
	}
	return id
}

func (w *lhWorld) jurisdiction(tenantID uuid.UUID) uuid.UUID {
	w.t.Helper()
	ctx := context.Background()
	jid := uuid.New()
	if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'lh fixture')`, jid, "LH-"+jid.String()[:12])
		return err
	}); err != nil {
		w.t.Fatalf("seed jurisdiction: %v", err)
	}
	if err := w.owner.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenant_jurisdiction_configs
			(tenant_id, jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, allowed_currencies, allowed_payment_methods, effective_from)
			VALUES ($1, $2, 'kyc1', 'aml1', 'rg1', '["EUR"]', '["card"]', now() - interval '1 hour')`, tenantID, jid)
		return err
	}); err != nil {
		w.t.Fatalf("seed jurisdiction config: %v", err)
	}
	return jid
}

// staff seeds an active staff user of a tenant (person-linked) and returns its id.
func (w *lhWorld) staff(tenantID uuid.UUID, role identity.StaffRole) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		p, err := identity.CreatePerson(ctx, tx)
		if err != nil {
			return err
		}
		s, err := identity.CreateStaffUser(ctx, tx, tenantID, "lh-"+uuid.NewString()+"@fixture.invalid", "!x", role, &p.ID)
		id = s.ID
		return err
	}); err != nil {
		w.t.Fatalf("seed tenant staff: %v", err)
	}
	return id
}

func (w *lhWorld) tenantTok(tenantID, staffID uuid.UUID, role auth.Role) string {
	return w.api.token(staffID, tenantID, role)
}

func (w *lhWorld) status(tenantID uuid.UUID, brandID *uuid.UUID) string {
	w.t.Helper()
	var s string
	err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if brandID != nil {
			return tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1`, *brandID).Scan(&s)
		}
		return tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&s)
	})
	if err != nil {
		w.t.Fatalf("read status: %v", err)
	}
	return s
}

func (w *lhWorld) requestCount(tenantID uuid.UUID) int {
	w.t.Helper()
	var n int
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM launch_authorisation_requests WHERE tenant_id = $1`, tenantID).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *lhWorld) auditCount(tenantID uuid.UUID, action string) int {
	w.t.Helper()
	var plat, ten int
	// Platform-scope rows (tenant_id NULL, subject_tenant_id = the tenant) and tenant-scope rows are
	// visible to different sessions (audit_log RLS), so read both.
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1 AND subject_tenant_id = $2 AND tenant_id IS NULL`, action, tenantID).Scan(&plat)
	}); err != nil {
		w.t.Fatal(err)
	}
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1 AND tenant_id = $2`, action, tenantID).Scan(&ten)
	}); err != nil {
		w.t.Fatal(err)
	}
	return plat + ten
}

// playerWithWallet seeds a player and an EUR wallet in the brand (owner path).
func (w *lhWorld) playerWithWallet(tenantID, brandID uuid.UUID) (player, walletID uuid.UUID) {
	w.t.Helper()
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		br, err := identity.GetBrandByID(ctx, tx, brandID)
		if err != nil {
			return err
		}
		p, err := identity.RegisterPlayer(ctx, tx, br, "lh-"+uuid.NewString()+"@fixture.invalid", "!x")
		if err != nil {
			return err
		}
		wl, err := wallet.GetOrCreate(ctx, tx, tenantID, brandID, p.ID, "EUR")
		if err != nil {
			return err
		}
		player, walletID = p.ID, wl.ID
		return nil
	}); err != nil {
		w.t.Fatalf("seed player + wallet: %v", err)
	}
	return player, walletID
}

// withdrawal inserts a withdrawal request in the given state (owner fixture, legacy binding).
func (w *lhWorld) withdrawal(tenantID, brandID, player, walletID uuid.UUID, state string) {
	w.t.Helper()
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return pitest.WithoutBindingGuard(ctx, tx, func() error {
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key)
				VALUES ($1, $2, $3, $4, 'EUR', 1000, $5, $6)`, tenantID, brandID, player, walletID, state, "lh-"+uuid.NewString())
			return err
		})
	}); err != nil {
		w.t.Fatalf("seed withdrawal: %v", err)
	}
}

// ---- request bodies / paths -------------------------------------------------------------

func lhBase(tenantID uuid.UUID) string {
	return "/v1/admin/tenants/" + tenantID.String() + "/launch-authorisations"
}

func lhReadinessPath(tenantID uuid.UUID, brandID *uuid.UUID, action string, jid *uuid.UUID) string {
	q := url.Values{}
	q.Set("action", action)
	if brandID != nil {
		q.Set("brand_id", brandID.String())
	}
	if jid != nil {
		q.Set("jurisdiction_ids", jid.String())
		q.Set("licensing_model", launchgov.LicModelOther)
		q.Set("licensing_status", launchgov.LicStatusConditional)
	}
	return "/v1/admin/tenants/" + tenantID.String() + "/launch-readiness?" + q.Encode()
}

type lhReadiness struct {
	Readiness struct {
		ReadinessHash        string   `json:"readiness_hash"`
		NonPassingCheckCodes []string `json:"non_passing_check_codes"`
		Checks               []struct {
			Code, Result string
		} `json:"checks"`
	} `json:"readiness"`
	Disclaimer string `json:"disclaimer"`
}

// nonPassing asks the readiness route (as the platform requester) for the checks to acknowledge.
func (w *lhWorld) nonPassing(tenantID uuid.UUID, brandID *uuid.UUID, action string, jid *uuid.UUID) []string {
	w.t.Helper()
	res := w.do("GET", lhReadinessPath(tenantID, brandID, action, jid), w.tokReq(), nil)
	if res.status != http.StatusOK {
		w.t.Fatalf("readiness preview: %d %s", res.status, res.body)
	}
	var rd lhReadiness
	res.decode(w.t, &rd)
	return rd.Readiness.NonPassingCheckCodes
}

func lhActivationBody(brandID *uuid.UUID, jid uuid.UUID, acks []string) map[string]any {
	b := map[string]any{
		"action": "activate", "reason_code": "initial_launch",
		"licensing": map[string]any{
			"model": launchgov.LicModelOther, "status": launchgov.LicStatusConditional,
			"determination_reference": "ext-ref-001", "determination_by_role": "external counsel",
		},
		"responsible_operator_name": "Operator Ltd", "responsible_operator_registration": "REG-4711",
		"jurisdiction_ids": []string{jid.String()}, "conditions_note": "review again after the first quarter",
		"acknowledged_check_codes": acks,
	}
	if brandID != nil {
		b["brand_id"] = brandID.String()
	}
	return b
}

type lhRequestResp struct {
	Disclaimer string `json:"disclaimer"`
	Request    struct {
		ID                string   `json:"id"`
		TenantID          string   `json:"tenant_id"`
		Status            string   `json:"status"`
		Action            string   `json:"action"`
		RequiredApprovals int      `json:"required_approvals"`
		RequestedBy       string   `json:"requested_by"`
		RequestedByScope  string   `json:"requested_by_scope"`
		PayloadHash       string   `json:"payload_hash"`
		Acks              []string `json:"acknowledged_check_codes"`
		Approvals         []struct {
			Decision  string `json:"decision"`
			DecidedBy string `json:"decided_by"`
		} `json:"approvals"`
	} `json:"request"`
	Readiness *struct {
		ReadinessHash string `json:"readiness_hash"`
	} `json:"readiness"`
	Executed   bool     `json:"executed"`
	Superseded []string `json:"superseded_request_ids"`
	Decision   string   `json:"decision"`
}

func (r cgResp) code(t *testing.T) string {
	t.Helper()
	var e struct {
		Code       string   `json:"code"`
		CheckCodes []string `json:"check_codes"`
	}
	r.decode(t, &e)
	return e.Code
}

func (r cgResp) checkCodes(t *testing.T) []string {
	t.Helper()
	var e struct {
		CheckCodes []string `json:"check_codes"`
	}
	r.decode(t, &e)
	return e.CheckCodes
}

// createRequest files a request and returns the decoded 201 response (fatal otherwise).
func (w *lhWorld) createRequest(token string, tenantID uuid.UUID, body map[string]any) lhRequestResp {
	w.t.Helper()
	res := w.do("POST", lhBase(tenantID), token, body)
	if res.status != http.StatusCreated {
		w.t.Fatalf("create request: %d %s", res.status, res.body)
	}
	var out lhRequestResp
	res.decode(w.t, &out)
	return out
}

// seenHash is the readiness_hash an approver is shown for a pending request (the GET response).
func (w *lhWorld) seenHash(token string, tenantID uuid.UUID, requestID string) string {
	w.t.Helper()
	res := w.do("GET", lhBase(tenantID)+"/"+requestID, token, nil)
	if res.status != http.StatusOK {
		w.t.Fatalf("get request: %d %s", res.status, res.body)
	}
	var out lhRequestResp
	res.decode(w.t, &out)
	if out.Readiness == nil || len(out.Readiness.ReadinessHash) != 64 {
		w.t.Fatalf("a pending request must show the current readiness hash: %s", res.body)
	}
	return out.Readiness.ReadinessHash
}

func (w *lhWorld) decide(token string, tenantID uuid.UUID, requestID, decision, reason, seen string) cgResp {
	w.t.Helper()
	body := map[string]any{"reason_code": reason}
	if seen != "" {
		body["seen_readiness_hash"] = seen
	}
	return w.do("POST", lhBase(tenantID)+"/"+requestID+"/"+decision, token, body)
}

func (w *lhWorld) approve(token string, tenantID uuid.UUID, requestID string) cgResp {
	w.t.Helper()
	return w.decide(token, tenantID, requestID, "approve", "reviewed_ok", w.seenHash(token, tenantID, requestID))
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// forceClosed reproduces a subject closed BEFORE migration 0130 (the sanctioned fixture exception).
func forceClosed(t *testing.T, w *lhWorld, tenantID uuid.UUID, brandID *uuid.UUID) {
	t.Helper()
	if err := launchfix.ForceClosed(context.Background(), t, w.rt, tenantID, brandID); err != nil {
		t.Fatalf("force closed: %v", err)
	}
}
