//go:build integration && drillseed

// A12 backup/restore/PITR drill: SYNTHETIC world seeder. NOT a regression test: it is excluded from every
// normal build (needs BOTH the `integration` and the `drillseed` tag) and refuses to run unless
// DRILL_SEED_ALLOW=throwaway-cluster is set and the target is a throwaway drill port (never 5432).
//
// It builds the world through the platform's OWN services and HTTP handlers (the same in-process server
// the integration suite uses), so every ledger invariant, trigger, RLS policy and audit row is produced
// by real code paths: tenants and brands, HTTP-registered players, deposits (initiate + mock-PSP
// settlement), casino launch + wager/win rounds (mock provider, many), sportsbook bets and a settlement,
// withdrawals in several states (requested, approved, submitted, completed, rejected, cancelled) through
// the finance four-eyes endpoints, K2 manual adjustments through the governed path (capability grants,
// platform baseline policy, submit/approve/reject/cancel), bonus campaign/offer/manual-grant issue, an
// economic-operation intent, explicit alerts, and one reconciliation run per tenant.
//
// Run by deploy/backup/drill.sh; parameters via DRILL_SEED_* environment variables. No real data, no real
// credential: passwords are random per run.
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

func drillEnvInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n >= 0 {
			return n
		}
	}
	return def
}

type drillTally struct {
	mu sync.Mutex
	m  map[string]int
}

func (d *drillTally) add(k string, n int) { d.mu.Lock(); d.m[k] += n; d.mu.Unlock() }

type drillHTTP struct {
	base string
	c    *http.Client
}

func (h drillHTTP) do(method, path, token string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

type drillPlayer struct {
	ID     uuid.UUID
	Token  string
	Email  string
	Tenant *drillTenant
}

type drillTenant struct {
	T       identity.Tenant
	Brand   identity.Brand
	Admin   uuid.UUID
	Finance []uuid.UUID
	Risk    uuid.UUID
	Bonus   uuid.UUID
	Players []*drillPlayer
}

func drillRawStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID, role string) uuid.UUID {
	t.Helper()
	id, person := uuid.New(), uuid.New()
	insert := func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		var tid any
		if tenantID != uuid.Nil {
			tid = tenantID
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id) VALUES ($1, $2, $3, 'x', $4, 'active', $5)`,
			id, tid, "drill-"+id.String()+"@example.invalid", role, person)
		return err
	}
	var err error
	if tenantID == uuid.Nil {
		err = pool.WithoutTenant(context.Background(), insert)
	} else {
		err = pool.WithTenant(context.Background(), tenantID, insert)
	}
	if err != nil {
		t.Fatalf("drill staff %s: %v", role, err)
	}
	return id
}

func TestDrillSeedWorld(t *testing.T) {
	if os.Getenv("DRILL_SEED_ALLOW") != "throwaway-cluster" {
		t.Skip("DRILL_SEED_ALLOW=throwaway-cluster is required (this seeder is for the A12 throwaway drill cluster only)")
	}
	raw := os.Getenv("TEST_DATABASE_URL")
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Port() == "5432" || u.Port() == "5433" {
		t.Fatalf("refusing target %q: the seeder only runs against a throwaway drill cluster on 127.0.0.1 (never :5432)", u.Host)
	}
	cfg := struct{ tenants, players, depositsPerPlayer, rounds, workers int }{
		tenants: drillEnvInt("DRILL_SEED_TENANTS", 3), players: drillEnvInt("DRILL_SEED_PLAYERS", 8),
		depositsPerPlayer: drillEnvInt("DRILL_SEED_DEPOSITS", 3), rounds: drillEnvInt("DRILL_SEED_ROUNDS", 100),
		workers: drillEnvInt("DRILL_SEED_WORKERS", 6),
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, raw, 24, 10*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	prooftest.Install(t, raw)
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": testJWTSecret})
	if err != nil {
		t.Fatal(err)
	}
	issuer := auth.NewIssuer(keys, "platform-api-test", "platform-api-test")

	payOrch, payMock := newMockOrchestrator()
	casOrch, _ := newMockCasinoOrchestrator()
	deps := financialTestDeps(pool, issuer, payOrch)
	deps.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	deps.AccessTokenTTL = 12 * time.Hour
	deps.RefreshTokenTTL = 24 * time.Hour
	deps.AuthRateLimitPerMinute = 1_000_000 // seeding registers many players from one IP; the limiter is not under test here
	deps.PaymentsMockSettlementEnabled = true
	deps.CasinoOrchestrator = casOrch
	deps.CasinoOutboundCredentials = casino.NewMockOutboundResolver()
	deps.CasinoPlaySimulationEnabled = true
	deps.SportsbookEnabled = true
	deps.SportsbookSettlementSimulationEnabled = true
	deps.PersonResolver = identityresolution.NewMockPersonResolver()
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)
	h := drillHTTP{base: srv.URL, c: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}}
	tally := &drillTally{m: map[string]int{}}
	t0 := time.Now()
	logf := func(f string, a ...any) {
		t.Logf("[seed +%5.1fs] "+f, append([]any{time.Since(t0).Seconds()}, a...)...)
	}

	stTok := func(id, tenantID uuid.UUID, role auth.Role) string {
		return authtest.ClaimedStaffToken(t, pool, testStaffMFAKeys(), issuer, id, tenantID, role)
	}
	must := func(code int, body []byte, err error, want int, what string) {
		t.Helper()
		if err != nil || code != want {
			t.Fatalf("%s: want %d got %d err=%v body=%s", what, want, code, err, body)
		}
	}

	// ---- platform actors (K2 policy authors, grant approver) and one shared game + sportsbook selection ----
	pa := drillRawStaff(t, pool, uuid.Nil, "platform_admin")
	pb := drillRawStaff(t, pool, uuid.Nil, "platform_admin")
	grantApprover := drillRawStaff(t, pool, uuid.Nil, "platform_admin")
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR", "USD")
	selection := mustSeedSportsbookSelection(t, pool, 250, 100) // 2.5x

	// K2 platform baseline for EUR, through the policy-change API (four-eyes: pa proposes, pb approves).
	code, body, err := h.do("POST", "/v1/admin/financial-policy-changes", stTok(pa, uuid.Nil, auth.RolePlatformAdmin), map[string]any{
		"change_kind": "policy", "operation_kind": "ledger_adjustment", "level": "platform", "asset_code": "EUR", "base_required_approvals": 1,
	})
	must(code, body, err, http.StatusCreated, "propose K2 baseline")
	var ch struct {
		ID          string `json:"id"`
		ContentHash string `json:"content_hash"`
	}
	_ = json.Unmarshal(body, &ch)
	code, body, err = h.do("POST", "/v1/admin/financial-policy-changes/"+ch.ID+"/approve", stTok(pb, uuid.Nil, auth.RolePlatformAdmin),
		map[string]any{"content_hash": ch.ContentHash, "reason_code": "drill_baseline"})
	must(code, body, err, http.StatusOK, "approve K2 baseline")

	// ---- tenants, brands, staff, provider capability, game availability ----
	var tenants []*drillTenant
	for i := 0; i < cfg.tenants; i++ {
		dt := &drillTenant{}
		suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		err := launchfix.OwnerFor(t, pool).WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			dt.T, err = launchfix.CreateTenant(ctx, tx, fmt.Sprintf("Drill Tenant %d", i+1), "drill-t"+strconv.Itoa(i+1)+"-"+suffix, "under_platform_licence")
			return err
		})
		if err != nil {
			t.Fatalf("tenant: %v", err)
		}
		dt.Brand = mustCreateBrand(t, pool, dt.T)
		dt.Admin = drillRawStaff(t, pool, dt.T.ID, "tenant_admin")
		dt.Finance = []uuid.UUID{drillRawStaff(t, pool, dt.T.ID, "finance"), drillRawStaff(t, pool, dt.T.ID, "finance"), drillRawStaff(t, pool, dt.T.ID, "finance")}
		dt.Risk = drillRawStaff(t, pool, dt.T.ID, "risk_manager")
		dt.Bonus = drillRawStaff(t, pool, dt.T.ID, "bonus_operations")
		mustRegisterCapability(t, pool, dt.T.ID, payMock)
		code, body, err := h.do("PUT", "/v1/admin/casino/providers/mock-casino/capability", stTok(dt.Admin, dt.T.ID, auth.RoleTenantAdmin), validCasinoCapabilityBody())
		must(code, body, err, http.StatusOK, "casino capability")
		mustEnableCasinoGameForTenant(t, pool, dt.T.ID, game.ID)
		// withdrawal policy: one finance approval suffices below the threshold
		if err := pool.WithTenant(ctx, dt.T.ID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
				VALUES ($1, 'EUR', 100000000, 1, now() - interval '1 hour')`, dt.T.ID)
			return err
		}); err != nil {
			t.Fatalf("withdrawal policy: %v", err)
		}
		tenants = append(tenants, dt)
	}
	logf("tenants/brands/staff/capabilities ready: %d", len(tenants))

	// ---- players: HTTP registration (bounded concurrency; Argon2id is memory hard) ----
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var pmu sync.Mutex
	var regErr atomic.Value
	for _, dt := range tenants {
		for p := 0; p < cfg.players; p++ {
			wg.Add(1)
			go func(dt *drillTenant, p int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				email := fmt.Sprintf("drill-%s-%d@example.invalid", strings.ReplaceAll(uuid.NewString(), "-", "")[:10], p)
				pw := "drill-" + strings.ReplaceAll(uuid.NewString(), "-", "")
				code, b, err := h.do("POST", "/v1/auth/register", "", map[string]string{"brand_slug": dt.Brand.Slug, "email": email, "password": pw})
				if err != nil || code != http.StatusCreated {
					regErr.Store(fmt.Sprintf("register: %d %v %s", code, err, b))
					return
				}
				var tp tokenPairResponse
				_ = json.Unmarshal(b, &tp)
				code, b, err = h.do("GET", "/v1/me", tp.AccessToken, nil)
				var me meResponse
				if err != nil || code != 200 || json.Unmarshal(b, &me) != nil {
					regErr.Store(fmt.Sprintf("me: %d %v", code, err))
					return
				}
				id, _ := uuid.Parse(me.ID)
				pmu.Lock()
				dt.Players = append(dt.Players, &drillPlayer{ID: id, Token: tp.AccessToken, Email: email, Tenant: dt})
				pmu.Unlock()
			}(dt, p)
		}
	}
	wg.Wait()
	if e := regErr.Load(); e != nil {
		t.Fatalf("%v", e)
	}
	for _, dt := range tenants {
		for _, p := range dt.Players {
			mustActivatePlayer(t, pool, dt.T.ID, p.ID)
		}
	}
	logf("players registered and activated: %d", cfg.tenants*cfg.players)

	// ---- per-player activity (deposits, casino rounds, sportsbook bets), bounded concurrency ----
	var actErr atomic.Value
	failf := func(f string, a ...any) { actErr.CompareAndSwap(nil, fmt.Sprintf(f, a...)) }
	deposit := func(p *drillPlayer, asset string, amount int64) bool {
		code, b, err := h.do("POST", "/v1/me/deposits", p.Token, map[string]any{
			"asset_code": asset, "amount": amount, "payment_method": "card", "idempotency_key": uuid.NewString()})
		if err != nil || code != 201 {
			failf("deposit initiate %d %v %s", code, err, b)
			return false
		}
		var in depositIntentResponse
		_ = json.Unmarshal(b, &in)
		code, b, err = h.do("POST", "/v1/me/deposits/"+in.ID+"/simulate-callback", p.Token, map[string]any{})
		if err != nil || code != 200 {
			failf("deposit settle %d %v %s", code, err, b)
			return false
		}
		tally.add("deposits_"+asset, 1)
		return true
	}
	wsem := make(chan struct{}, cfg.workers)
	for _, dt := range tenants {
		for pi, p := range dt.Players {
			wg.Add(1)
			go func(dt *drillTenant, pi int, p *drillPlayer) {
				defer wg.Done()
				wsem <- struct{}{}
				defer func() { <-wsem }()
				rnd := rand.New(rand.NewSource(int64(pi)*7919 + time.Now().UnixNano()))
				for d := 0; d < cfg.depositsPerPlayer; d++ {
					if !deposit(p, "EUR", int64(20_000+rnd.Intn(180_000))) {
						return
					}
				}
				if pi%3 == 0 {
					if !deposit(p, "USD", int64(10_000+rnd.Intn(90_000))) {
						return
					}
				}
				code, b, err := h.do("POST", "/v1/me/casino/games/"+game.ID.String()+"/launch", p.Token, map[string]string{"asset_code": "EUR", "mode": "real"})
				if err != nil || code != http.StatusCreated {
					failf("launch %d %v %s", code, err, b)
					return
				}
				var l launchCasinoGameResponse
				_ = json.Unmarshal(b, &l)
				for r := 0; r < cfg.rounds; r++ {
					stake := int64(100 + rnd.Intn(2_000))
					code, b, err = h.do("POST", "/v1/me/casino/sessions/"+l.SessionID+"/wager", p.Token, map[string]any{"stake_amount": stake, "idempotency_key": uuid.NewString()})
					if err != nil || code != 200 || !bytes.Contains(b, []byte(`"succeeded"`)) {
						// out of funds: top up and carry on
						if !deposit(p, "EUR", 150_000) {
							return
						}
						continue
					}
					tally.add("casino_wagers", 1)
					if rnd.Intn(100) < 45 {
						win := int64(50 + rnd.Intn(4_000))
						code, b, err = h.do("POST", "/v1/me/casino/sessions/"+l.SessionID+"/win", p.Token, map[string]any{"win_amount": win, "idempotency_key": uuid.NewString()})
						if err != nil || code != 200 {
							failf("win %d %v %s", code, err, b)
							return
						}
						tally.add("casino_wins", 1)
					}
				}
				// sportsbook: two bets
				for k := 0; k < 2; k++ {
					code, b, err = h.do("POST", "/v1/me/sportsbook/bets", p.Token, placeBetRequestBody(selection, int64(500+rnd.Intn(1500)), 250, 100, uuid.NewString()))
					if err != nil || code != http.StatusCreated {
						failf("bet %d %v %s", code, err, b)
						return
					}
					tally.add("sportsbook_bets", 1)
				}
			}(dt, pi, p)
		}
	}
	wg.Wait()
	if e := actErr.Load(); e != nil {
		t.Fatalf("player activity: %v", e)
	}
	logf("player activity done: %v", tally.m)

	// ---- per tenant: sportsbook settlement, withdrawals, adjustments, bonus, alerts, reconciliation ----
	for ti, dt := range tenants {
		fin := func(i int) string { return stTok(dt.Finance[i], dt.T.ID, auth.RoleFinance) }
		// sportsbook: settle each player's first bet (won / lost / void mix) via the risk manager simulation route
		for pi, p := range dt.Players {
			code, b, err := h.do("GET", "/v1/me/sportsbook/bets", p.Token, nil)
			if err != nil || code != 200 {
				t.Fatalf("list bets: %d %v", code, err)
			}
			var page pagedResponse[betResponse]
			_ = json.Unmarshal(b, &page)
			if len(page.Items) == 0 {
				continue
			}
			bet := page.Items[0]
			var ev map[string]any
			switch pi % 3 {
			case 0:
				ev = map[string]any{"event_type": "settle", "generation": 1, "outcome": "won", "asset_code": "EUR", "payout_amount": bet.PotentialReturn}
			case 1:
				ev = map[string]any{"event_type": "settle", "generation": 1, "outcome": "lost", "asset_code": "EUR", "payout_amount": 0}
			default:
				continue // leave open
			}
			code, b, err = h.do("POST", simulatePath(bet.ID), stTok(dt.Risk, dt.T.ID, auth.RoleRiskManager), ev)
			if err != nil || code != 200 {
				t.Fatalf("settle bet: %d %v %s", code, err, b)
			}
			tally.add("sportsbook_settled", 1)
		}

		// withdrawals: six requests through the withdrawal service, then finance endpoints
		type wd struct{ id, state string }
		var wds []wd
		for k := 0; k < 6 && k < len(dt.Players); k++ {
			p := dt.Players[k]
			wr := mustCreateWithdrawalRequest(t, pool, dt.T.ID, dt.Brand.ID, p.ID, walletIDFor(t, pool, dt.T.ID, p.ID, "EUR"), "EUR", int64(1_000+k*250))
			wds = append(wds, wd{wr.ID.String(), "requested"})
		}
		// the player cancels their own request while it is still 'requested' (before any staff review)
		if len(wds) > 4 {
			code, b, err := h.do("POST", "/v1/me/withdrawals/"+wds[4].id+"/cancel", dt.Players[4].Token, nil)
			if err != nil || code >= 300 {
				t.Logf("cancel withdrawal returned %d (%s); continuing", code, b)
			}
		}
		// opening the review queue moves requested -> in review (the same call the Back Office makes)
		code, b, err := h.do("GET", "/v1/admin/withdrawals", fin(0), nil)
		must(code, b, err, 200, "open review queue")
		for k, w := range wds {
			switch k {
			case 0, 1, 2: // approve (+ submit, + resolve for k=0)
				code, b, err := h.do("POST", "/v1/admin/withdrawals/"+w.id+"/approve", fin(0), nil)
				must(code, b, err, 200, "approve withdrawal")
				if k <= 1 {
					code, b, err = h.do("POST", "/v1/admin/withdrawals/"+w.id+"/submit", fin(0), map[string]string{"payment_method": "bank_transfer"})
					must(code, b, err, 200, "submit withdrawal")
				}
				if k == 0 {
					code, b, err = h.do("POST", "/v1/admin/withdrawals/"+w.id+"/resolve", fin(0), nil)
					must(code, b, err, 200, "resolve withdrawal")
				}
			case 3:
				code, b, err := h.do("POST", "/v1/admin/withdrawals/"+w.id+"/reject", fin(0), map[string]string{"reason_code": "drill_rejection"})
				if err != nil || code >= 300 {
					t.Logf("reject withdrawal returned %d (%s); continuing", code, b)
				}
			}
			tally.add("withdrawals", 1)
		}

		// K2 manual adjustments through the governed path: grants, then submit / approve / reject / cancel
		grant := func(grantee uuid.UUID, c capability.Capability) {
			var reqID uuid.UUID
			if err := pool.WithPrincipalScope(ctx, dt.T.ID, dt.Admin, func(ctx context.Context, tx pgx.Tx) error {
				r, err := capability.CreateRequest(ctx, tx, dt.T.ID, capability.NewRequestInput{GranteeStaffID: grantee, Capability: c, ReasonCode: "drill_seed"})
				reqID = r.ID
				return err
			}); err != nil {
				t.Fatalf("grant request: %v", err)
			}
			if err := pool.WithPlatformAdmin(ctx, grantApprover, func(ctx context.Context, tx pgx.Tx) error {
				_, _, err := capability.DecideAndGrant(ctx, tx, dt.T.ID, reqID, "approve", "drill_seed")
				return err
			}); err != nil {
				t.Fatalf("grant approve: %v", err)
			}
		}
		for _, f := range dt.Finance[:2] {
			grant(f, capability.CapabilityLedgerAdjustmentInitiate)
			grant(f, capability.CapabilityLedgerAdjustmentApprove)
		}
		var wallets []uuid.UUID
		for _, p := range dt.Players[:min(4, len(dt.Players))] {
			wallets = append(wallets, walletIDFor(t, pool, dt.T.ID, p.ID, "EUR"))
		}
		base := "/v1/admin/tenants/" + dt.T.ID.String() + "/manual-adjustments"
		for k := 0; k < 6; k++ {
			dir := "credit_player"
			if k%3 == 2 {
				dir = "debit_player"
			}
			code, b, err := h.do("POST", base, stTok(dt.Finance[0], dt.T.ID, auth.RoleFinance), map[string]any{
				"wallet_id": wallets[k%len(wallets)].String(), "asset_code": "EUR", "direction": dir,
				"amount_minor_units": strconv.Itoa(100 + 25*k), "reason_code": "operational_error_correction", "note": "A12 drill synthetic adjustment",
			})
			must(code, b, err, http.StatusCreated, "submit adjustment")
			var d maDTO
			_ = json.Unmarshal(b, &d)
			f2 := stTok(dt.Finance[1], dt.T.ID, auth.RoleFinance)
			switch {
			case k <= 3:
				code, b, err = h.do("POST", base+"/"+d.ID+"/approve", f2, map[string]any{"payload_hash": d.PayloadHash, "reason_code": "drill_seed"})
				if err != nil || code != 200 {
					t.Logf("approve adjustment %d (%s)", code, b)
				} else {
					tally.add("adjustments_executed", 1)
				}
			case k == 4:
				code, b, err = h.do("POST", base+"/"+d.ID+"/reject", f2, map[string]any{"payload_hash": d.PayloadHash, "reason_code": "drill_seed"})
				if err != nil || code != 200 {
					t.Logf("reject adjustment %d (%s)", code, b)
				}
			default:
				code, b, err = h.do("POST", base+"/"+d.ID+"/cancel", stTok(dt.Finance[0], dt.T.ID, auth.RoleFinance), nil)
				if err != nil || code != 200 {
					t.Logf("cancel adjustment %d (%s)", code, b)
				}
			}
			tally.add("adjustments_submitted", 1)
		}

		// bonus: campaign + offer (service) and manual grants issued (HTTP, status issued) for three players
		bt := stTok(dt.Bonus, dt.T.ID, auth.RoleBonusOperations)
		for k := 0; k < len(dt.Players) && tally.m["bonus_grants_issued"] < (ti+1)*3; k += 3 { // USD-wallet holders are every third player
			p := dt.Players[k]
			// one offer (and campaign) per grant: a player holds at most one grant per offer
			offerID, offerVersionID := seedBonusOfferForHTTP(t, pool, dt.T.ID, dt.Brand.ID, dt.Bonus)
			var campaignID, campaignVersionID uuid.UUID
			if err := pool.WithTenant(ctx, dt.T.ID, func(ctx context.Context, tx pgx.Tx) error {
				o, err := bonus.GetOfferByID(ctx, tx, offerID)
				campaignID, campaignVersionID = o.CampaignID, o.CampaignVersionID
				return err
			}); err != nil {
				t.Fatalf("bonus offer: %v", err)
			}
			code, b, err := h.do("POST", "/v1/admin/bonus/economic-operations", bt, map[string]any{
				"operation_type": "bonus_manual_grant", "subject_scope": "single_subject", "subject_ref": p.ID.String(),
				"asset_code": "USD", "intended_aggregate_value": "100000", "recipient_ceiling": 1, "idempotency_key": "eoi-" + uuid.NewString()})
			if err != nil || code >= 300 {
				t.Logf("economic operation %d (%s)", code, b)
				continue
			}
			var eoi economicOperationResponse
			_ = json.Unmarshal(b, &eoi)
			code, b, err = h.do("POST", "/v1/admin/bonus/manual-grants", bt, map[string]any{
				"player_account_id": p.ID.String(), "campaign_id": campaignID.String(), "campaign_version_id": campaignVersionID.String(),
				"offer_id": offerID.String(), "offer_version_id": offerVersionID.String(), "asset_code": "USD",
				"funding_source": "operator", "parent_operation_id": eoi.OperationID})
			if err != nil || code != 201 {
				t.Logf("manual grant %d (%s)", code, b)
				continue
			}
			tally.add("bonus_grants_issued", 1)
		}

		// alerts (explicit, tenant scope) and one reconciliation run per tenant
		if err := alerting.RaiseDetached(ctx, alerting.NewTenantRunner(pool, dt.T.ID), alerting.Alert{
			Kind: alerting.KindReconciliationRunFailed, SubjectTenantID: dt.T.ID, Discriminator: "drill-seed-" + strconv.Itoa(ti),
			Attributes: map[string]alerting.AttrValue{"stream": "drill", "phase": "seed", "sqlstate_class": "XX"},
		}); err != nil {
			t.Logf("explicit alert: %v", err)
		} else {
			tally.add("alerts_explicit", 1)
		}
		now := time.Now().UTC()
		if err := pool.WithTenant(ctx, dt.T.ID, func(ctx context.Context, tx pgx.Tx) error {
			run, ms, err := reconciliation.RunLedgerVsProjection(ctx, tx, dt.T.ID, now.Add(-24*time.Hour), now)
			if err != nil {
				return err
			}
			if len(ms) != 0 {
				return fmt.Errorf("seed-time reconciliation found %d mismatches (run %s)", len(ms), run.ID)
			}
			return nil
		}); err != nil {
			t.Fatalf("reconciliation: %v", err)
		}
		tally.add("reconciliation_runs", 1)
		logf("tenant %d finished", ti+1)
	}

	var tenantIDs []string
	for _, dt := range tenants {
		tenantIDs = append(tenantIDs, dt.T.ID.String())
	}
	rep := map[string]any{"tally": tally.m, "seconds": time.Since(t0).Seconds(), "tenants": cfg.tenants, "players_per_tenant": cfg.players, "tenant_ids": tenantIDs}
	if out := os.Getenv("DRILL_SEED_REPORT"); out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(out, b, 0o600); err != nil {
			t.Fatalf("write report: %v", err)
		}
	}
	logf("DONE %v", tally.m)
}
