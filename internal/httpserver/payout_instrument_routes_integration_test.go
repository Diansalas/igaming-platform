//go:build integration

package httpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/admission"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
)

const (
	routeIBAN1 = "GB82WEST12345698765432"
	routeIBAN2 = "DE89370400440532013000"
)

type piLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *piLogBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *piLogBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func piKey32(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

type piEnv struct {
	owner, rt *db.Pool
	srv       *httptest.Server
	logs      *piLogBuffer
	clock     *admission.FakeClock
	svc       *payoutinstrument.Service
	tenant    identity.Tenant
	brand     identity.Brand
}

func newPIEnv(t *testing.T, withService bool) *piEnv {
	t.Helper()
	owner, issuer := testEnv(t)
	rt := gateRuntimePool(t)
	e := &piEnv{owner: owner, rt: rt, logs: &piLogBuffer{}, clock: admission.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}
	deps := Deps{
		Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		DB:     rt, AuthIssuer: issuer, ServiceName: "platform-api-test", AccessTokenTTL: 5 * time.Minute, RefreshTokenTTL: time.Hour,
		PersonResolver:                  identityresolution.NewMockPersonResolver(),
		PayoutInstrumentRegisterLimiter: NewPayoutInstrumentRegisterLimiter(e.clock),
	}
	if withService {
		keys, err := payoutinstrument.NewKeys("m1", map[string][]byte{"m1": piKey32(t)}, "f1", map[string][]byte{"f1": piKey32(t)})
		if err != nil {
			t.Fatal(err)
		}
		e.svc, err = payoutinstrument.NewService(keys, payoutinstrument.DefaultKinds(), payoutinstrument.NewMockVerifier())
		if err != nil {
			t.Fatal(err)
		}
		deps.PayoutInstruments = e.svc
	}
	e.srv = httptest.NewServer(New(deps))
	t.Cleanup(e.srv.Close)
	e.tenant = mustCreateTenant(t, owner)
	e.brand = mustCreateBrand(t, owner, e.tenant)
	return e
}

func (e *piEnv) player(t *testing.T) registeredPlayer {
	t.Helper()
	p := mustRegisterPlayer(t, e.srv, e.brand.Slug)
	mustActivatePlayer(t, e.owner, e.tenant.ID, p.ID)
	return p
}

func (e *piEnv) staff(t *testing.T, role identity.StaffRole) string {
	t.Helper()
	const pw = "a-decent-password-1"
	s := mustCreateStaff(t, e.owner, e.tenant.ID, role, pw)
	return mustLoginStaff(t, e.srv, e.tenant.Slug, s.Email, pw).AccessToken
}

func ibanBody(iban string) map[string]any {
	return map[string]any{"kind": "bank_account", "rail": "sepa", "asset_codes": []string{"EUR"}, "detail": map[string]string{"iban": iban}}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func doReq(t *testing.T, e *piEnv, method, path, token string, body any) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, readBody(t, resp)
}

type viewJSON struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Rail        string `json:"rail"`
	DisplayMask string `json:"display_mask"`
	State       string `json:"state"`
}

func TestPayoutInstrumentRoutes_RegisterListRevoke(t *testing.T) {
	e := newPIEnv(t, true)
	p := e.player(t)

	code, body := doReq(t, e, "POST", "/v1/me/payout-instruments", p.Tokens.AccessToken, ibanBody(routeIBAN1))
	if code != http.StatusCreated {
		t.Fatalf("register: %d %s", code, body)
	}
	var v viewJSON
	_ = json.Unmarshal([]byte(body), &v)
	if v.State != "verified" || v.DisplayMask != "GB****5432" || v.Kind != "bank_account" || v.ID == "" {
		t.Fatalf("view = %+v", v)
	}
	// Nothing but the mask is detail-derived: no fingerprint, ciphertext, seal, detail or IBAN.
	for _, banned := range []string{"fingerprint", "ciphertext", "nonce", "seal", "detail", "GB82WEST", "12345698765432", "person", "kid"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(banned)) {
			t.Errorf("register response leaks %q: %s", banned, body)
		}
	}
	// Idempotent: the same destination again returns the same instrument (200).
	code, body2 := doReq(t, e, "POST", "/v1/me/payout-instruments", p.Tokens.AccessToken, ibanBody("gb82 west 1234 5698 7654 32"))
	var v2 viewJSON
	_ = json.Unmarshal([]byte(body2), &v2)
	if code != http.StatusOK || v2.ID != v.ID {
		t.Fatalf("idempotent register: %d %s", code, body2)
	}
	// List: masks only.
	code, list := doReq(t, e, "GET", "/v1/me/payout-instruments", p.Tokens.AccessToken, nil)
	var views []viewJSON
	_ = json.Unmarshal([]byte(list), &views)
	if code != http.StatusOK || len(views) != 1 || views[0].ID != v.ID {
		t.Fatalf("list: %d %s", code, list)
	}
	for _, banned := range []string{"fingerprint", "ciphertext", "GB82WEST", "player_account_id"} {
		if strings.Contains(list, banned) {
			t.Errorf("list leaks %q: %s", banned, list)
		}
	}
	// A client-supplied id (or any unknown field) is refused.
	b := ibanBody(routeIBAN2)
	b["id"] = uuid.NewString()
	if code, _ := doReq(t, e, "POST", "/v1/me/payout-instruments", p.Tokens.AccessToken, b); code != http.StatusBadRequest {
		t.Fatalf("a client-supplied id must be refused (400), got %d", code)
	}
	// Revoke: another player cannot, the owner can, repeat is idempotent.
	q := e.player(t)
	if code, _ := doReq(t, e, "POST", "/v1/me/payout-instruments/"+v.ID+"/revoke", q.Tokens.AccessToken, nil); code != http.StatusNotFound {
		t.Fatalf("revoking another player's instrument: %d", code)
	}
	for i := 0; i < 2; i++ {
		code, body := doReq(t, e, "POST", "/v1/me/payout-instruments/"+v.ID+"/revoke", p.Tokens.AccessToken, nil)
		var rv viewJSON
		_ = json.Unmarshal([]byte(body), &rv)
		if code != http.StatusOK || rv.State != "revoked" {
			t.Fatalf("revoke #%d: %d %s", i, code, body)
		}
	}
	if code, _ := doReq(t, e, "POST", "/v1/me/payout-instruments/not-a-uuid/revoke", p.Tokens.AccessToken, nil); code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", code)
	}
	// Missing fields.
	if code, _ := doReq(t, e, "POST", "/v1/me/payout-instruments", p.Tokens.AccessToken, map[string]any{"kind": "bank_account"}); code != http.StatusBadRequest {
		t.Fatalf("missing fields: %d", code)
	}
	// Request bodies are never logged: the IBAN appears nowhere in the server log.
	if logs := e.logs.String(); strings.Contains(logs, "GB82") || strings.Contains(logs, "12345698765432") || strings.Contains(strings.ToLower(logs), "iban\"") {
		t.Fatalf("a request body leaked into the log:\n%s", logs)
	}
}

func TestPayoutInstrumentRoutes_GenericConflictResponse(t *testing.T) {
	e := newPIEnv(t, true)
	p1, p2 := e.player(t), e.player(t)
	if code, body := doReq(t, e, "POST", "/v1/me/payout-instruments", p1.Tokens.AccessToken, ibanBody(routeIBAN1)); code != http.StatusCreated {
		t.Fatalf("p1 register: %d %s", code, body)
	}
	type refusal struct{ code, msg string }
	parse := func(body string) refusal {
		var env struct{ Code, Message string }
		_ = json.Unmarshal([]byte(body), &env)
		return refusal{env.Code, env.Message}
	}
	conflictCode, conflictBody := doReq(t, e, "POST", "/v1/me/payout-instruments", p2.Tokens.AccessToken, ibanBody(routeIBAN1))
	if conflictCode != http.StatusConflict {
		t.Fatalf("conflict: %d %s", conflictCode, conflictBody)
	}
	want := parse(conflictBody)
	if want.code != "PAYOUT_INSTRUMENT_NOT_ACCEPTED" {
		t.Fatalf("conflict code = %q", want.code)
	}
	// Every other registration refusal is byte-identical (no enumeration oracle).
	others := map[string]map[string]any{
		"invalid iban":     ibanBody("GB00INVALID"),
		"unknown kind":     {"kind": "nope", "rail": "sepa", "asset_codes": []string{"EUR"}, "detail": map[string]string{"iban": routeIBAN2}},
		"rail mismatch":    {"kind": "bank_account", "rail": "card", "asset_codes": []string{"EUR"}, "detail": map[string]string{"iban": routeIBAN2}},
		"card number":      {"kind": "synthetic_test", "rail": "synthetic", "asset_codes": []string{"EUR"}, "detail": map[string]string{"label": "ok", "pan": "4111111111111111"}},
		"unknown asset":    {"kind": "bank_account", "rail": "sepa", "asset_codes": []string{"XXX"}, "detail": map[string]string{"iban": routeIBAN2}},
		"unknown detail k": {"kind": "bank_account", "rail": "sepa", "asset_codes": []string{"EUR"}, "detail": map[string]string{"iban": routeIBAN2, "x": "y"}},
	}
	for name, b := range others {
		e.clock.Advance(7 * time.Minute) // keep the registration limiter out of this test
		code, body := doReq(t, e, "POST", "/v1/me/payout-instruments", p2.Tokens.AccessToken, b)
		if code != http.StatusConflict || parse(body) != want {
			t.Errorf("%s: %d %s, want the generic %+v", name, code, body, want)
		}
	}
	// The specific reason went to the audit row (compliance-visible), never the response.
	var n int
	if err := e.owner.WithTenant(context.Background(), e.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'payout_instrument.registration_conflict' AND metadata->>'reason' = 'fingerprint_conflict' AND actor_id = $1`, p2.ID).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("conflict audit rows = %d (%v)", n, err)
	}
	if err := e.owner.WithTenant(context.Background(), e.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'payout_instrument.registration_refused' AND metadata->>'reason' = 'pan_refused'`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("PAN refusal audit rows = %d (%v)", n, err)
	}
	// p2 owns nothing after the refusals.
	if code, list := doReq(t, e, "GET", "/v1/me/payout-instruments", p2.Tokens.AccessToken, nil); code != 200 || strings.TrimSpace(list) != "[]" {
		t.Fatalf("p2 list = %d %s", code, list)
	}
}

func TestPayoutInstrumentRoutes_RateLimitPerPlayer(t *testing.T) {
	e := newPIEnv(t, true)
	p, other := e.player(t), e.player(t)
	post := func(tok string, i int) (int, http.Header) {
		b, _ := json.Marshal(map[string]any{"kind": "synthetic_test", "rail": "synthetic", "asset_codes": []string{"EUR"}, "detail": map[string]string{"label": fmt.Sprintf("rl-%d", i)}})
		req, _ := http.NewRequest("POST", e.srv.URL+"/v1/me/payout-instruments", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = readBody(t, resp)
		return resp.StatusCode, resp.Header
	}
	for i := 0; i < 5; i++ {
		if code, _ := post(p.Tokens.AccessToken, i); code != http.StatusCreated {
			t.Fatalf("request %d within the burst: %d", i, code)
		}
	}
	code, h := post(p.Tokens.AccessToken, 99)
	if code != http.StatusTooManyRequests || h.Get("Retry-After") == "" {
		t.Fatalf("6th request: %d Retry-After=%q", code, h.Get("Retry-After"))
	}
	// Refused attempts consume budget too (a refusal does not reset the bucket).
	if code, _ := post(p.Tokens.AccessToken, 100); code != http.StatusTooManyRequests {
		t.Fatalf("still limited: %d", code)
	}
	// Per player: another player is unaffected.
	if code, _ := post(other.Tokens.AccessToken, 1001); code != http.StatusCreated {
		t.Fatalf("another player must not be limited: %d", code)
	}
	// Time refills the bucket.
	e.clock.Advance(7 * time.Minute)
	if code, _ := post(p.Tokens.AccessToken, 101); code != http.StatusCreated {
		t.Fatalf("after refill: %d", code)
	}
}

func TestPayoutInstrumentRoutes_AuthzAndStaffSuspend(t *testing.T) {
	e := newPIEnv(t, true)
	p := e.player(t)
	_, body := doReq(t, e, "POST", "/v1/me/payout-instruments", p.Tokens.AccessToken, ibanBody(routeIBAN1))
	var v viewJSON
	_ = json.Unmarshal([]byte(body), &v)
	compliance := e.staff(t, identity.StaffRole("compliance"))
	finance := e.staff(t, identity.StaffRole("finance"))
	tenantAdmin := e.staff(t, identity.StaffRole("tenant_admin"))
	support := e.staff(t, identity.StaffRole("support"))

	listPath := "/v1/admin/players/" + p.ID.String() + "/payout-instruments"
	suspendPath := "/v1/admin/payout-instruments/" + v.ID + "/suspend"
	susp := map[string]string{"reason_code": "aml_review"}

	// Unauthenticated: 401 everywhere.
	for _, c := range []struct{ m, path string }{{"POST", "/v1/me/payout-instruments"}, {"GET", "/v1/me/payout-instruments"},
		{"POST", "/v1/me/payout-instruments/" + v.ID + "/revoke"}, {"GET", listPath}, {"POST", suspendPath}} {
		if code, _ := doReq(t, e, c.m, c.path, "", map[string]string{}); code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d", c.m, c.path, code)
		}
	}
	// Staff tokens on the player routes and a player token on the staff routes: 403.
	for _, tok := range []string{compliance, finance, tenantAdmin} {
		if code, _ := doReq(t, e, "GET", "/v1/me/payout-instruments", tok, nil); code != http.StatusForbidden {
			t.Errorf("staff on a player route: %d", code)
		}
	}
	for _, c := range []struct{ m, path string }{{"GET", listPath}, {"POST", suspendPath}} {
		if code, _ := doReq(t, e, c.m, c.path, p.Tokens.AccessToken, susp); code != http.StatusForbidden {
			t.Errorf("player on %s: %d", c.path, code)
		}
	}
	// Read: finance and compliance yes; tenant_admin and support no.
	for name, tc := range map[string]struct {
		tok  string
		want int
	}{"compliance": {compliance, 200}, "finance": {finance, 200}, "tenant_admin": {tenantAdmin, 403}, "support": {support, 403}} {
		code, body := doReq(t, e, "GET", listPath, tc.tok, nil)
		if code != tc.want {
			t.Errorf("%s read: %d, want %d", name, code, tc.want)
		}
		if code == 200 {
			for _, banned := range []string{"fingerprint", "ciphertext", "GB82WEST"} {
				if strings.Contains(body, banned) {
					t.Errorf("staff list leaks %q", banned)
				}
			}
			if !strings.Contains(body, v.ID) || !strings.Contains(body, "GB****5432") {
				t.Errorf("staff list = %s", body)
			}
		}
	}
	// Suspend: compliance only; a reason is required; no staff create/verify/unsuspend/edit route exists.
	for name, tok := range map[string]string{"finance": finance, "tenant_admin": tenantAdmin, "support": support} {
		if code, _ := doReq(t, e, "POST", suspendPath, tok, susp); code != http.StatusForbidden {
			t.Errorf("%s suspend: %d", name, code)
		}
	}
	if code, _ := doReq(t, e, "POST", suspendPath, compliance, map[string]string{}); code != http.StatusBadRequest {
		t.Errorf("suspend without a reason: %d", code)
	}
	code, sb := doReq(t, e, "POST", suspendPath, compliance, susp)
	var sv viewJSON
	_ = json.Unmarshal([]byte(sb), &sv)
	if code != http.StatusOK || sv.State != "suspended" {
		t.Fatalf("compliance suspend: %d %s", code, sb)
	}
	if code, _ := doReq(t, e, "POST", suspendPath, compliance, susp); code != http.StatusOK {
		t.Fatalf("repeat suspend must be idempotent: %d", code)
	}
	for _, c := range []struct{ m, path string }{{"POST", "/v1/admin/payout-instruments"}, {"POST", suspendPath[:len(suspendPath)-len("suspend")] + "verify"},
		{"POST", suspendPath[:len(suspendPath)-len("suspend")] + "unsuspend"}, {"PATCH", "/v1/admin/payout-instruments/" + v.ID}} {
		if code, _ := doReq(t, e, c.m, c.path, compliance, susp); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s must not exist, got %d", c.m, c.path, code)
		}
	}
	// The player sees the suspension, cannot re-verify it away, and a suspended instrument cannot be re-registered into verified.
	_, list := doReq(t, e, "GET", "/v1/me/payout-instruments", p.Tokens.AccessToken, nil)
	if !strings.Contains(list, `"suspended"`) {
		t.Fatalf("player list = %s", list)
	}
	code, again := doReq(t, e, "POST", "/v1/me/payout-instruments", p.Tokens.AccessToken, ibanBody(routeIBAN1))
	var av viewJSON
	_ = json.Unmarshal([]byte(again), &av)
	if code != http.StatusOK || av.State != "suspended" || av.ID != v.ID {
		t.Fatalf("re-registering a suspended destination must return it still suspended: %d %s", code, again)
	}
	// Cross-tenant: staff of another tenant gets 404 on suspend and an empty list.
	other := newPIEnvSecondTenant(t, e)
	if code, _ := doReq(t, e, "POST", suspendPath, other, susp); code != http.StatusNotFound {
		t.Errorf("cross-tenant suspend: %d", code)
	}
	if code, ol := doReq(t, e, "GET", listPath, other, nil); code != 200 || strings.TrimSpace(ol) != "[]" {
		t.Errorf("cross-tenant list: %d %s", code, ol)
	}
}

// newPIEnvSecondTenant returns a compliance token of a DIFFERENT tenant on the same server.
func newPIEnvSecondTenant(t *testing.T, e *piEnv) string {
	t.Helper()
	t2 := mustCreateTenant(t, e.owner)
	const pw = "a-decent-password-1"
	s := mustCreateStaff(t, e.owner, t2.ID, identity.StaffRole("compliance"), pw)
	return mustLoginStaff(t, e.srv, t2.Slug, s.Email, pw).AccessToken
}

func TestPayoutInstrumentRoutes_UnavailableWithoutKeys(t *testing.T) {
	e := newPIEnv(t, false)
	p := e.player(t)
	for _, c := range []struct{ m, path string }{{"POST", "/v1/me/payout-instruments"}, {"GET", "/v1/me/payout-instruments"},
		{"POST", "/v1/me/payout-instruments/" + uuid.NewString() + "/revoke"}} {
		if code, _ := doReq(t, e, c.m, c.path, p.Tokens.AccessToken, ibanBody(routeIBAN1)); code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a service: %d, want 503", c.m, c.path, code)
		}
	}
	compliance := e.staff(t, identity.StaffRole("compliance"))
	if code, _ := doReq(t, e, "POST", "/v1/admin/payout-instruments/"+uuid.NewString()+"/suspend", compliance, map[string]string{"reason_code": "x_y"}); code != http.StatusServiceUnavailable {
		t.Errorf("suspend without a service: %d", code)
	}
}

func TestPayoutInstrumentRoutes_ConcurrentDoubleRegister(t *testing.T) {
	e := newPIEnv(t, true)
	p := e.player(t)
	e.clock.Advance(0)
	var wg sync.WaitGroup
	ids := make([]string, 4)
	codes := make([]int, 4)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, body := doReq(t, e, "POST", "/v1/me/payout-instruments", p.Tokens.AccessToken, ibanBody(routeIBAN2))
			var v viewJSON
			_ = json.Unmarshal([]byte(body), &v)
			ids[i], codes[i] = v.ID, code
		}(i)
	}
	wg.Wait()
	created := 0
	for i := range ids {
		if codes[i] != http.StatusCreated && codes[i] != http.StatusOK {
			t.Fatalf("concurrent register %d: %d", i, codes[i])
		}
		if codes[i] == http.StatusCreated {
			created++
		}
		if ids[i] != ids[0] {
			t.Fatalf("different instruments: %v", ids)
		}
	}
	if created < 1 {
		t.Fatal("no 201")
	}
	var n int
	if err := e.owner.WithTenant(context.Background(), e.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payout_instruments WHERE player_account_id = $1`, p.ID).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("instrument rows = %d (%v)", n, err)
	}
}
