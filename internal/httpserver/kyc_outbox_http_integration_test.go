//go:build integration

package httpserver

// PRH-2 E1 (ADR 0095 section 38, ADR 0106 sections 7.1/7.2): the HTTP surface of
// the KYC outbox. No HTTP path calls a KYC vendor; create returns 201 with the
// phase-A `unverified` row, a repeat create returns 200 with the player's
// EXISTING verification (caller-owned, same body shape); a player response never
// carries an outbox field; the staff reads carry a read-only derived
// `submission_state`.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// countingKYCAdapter counts the vendor calls actually made.
type countingKYCAdapter struct {
	*kyc.MockKYCProvider
	creates, submits *atomic.Int32
}

func (a countingKYCAdapter) CreateVerification(ctx context.Context, in kyc.CreateVerificationInput) (kyc.ProviderResult, error) {
	a.creates.Add(1)
	return a.MockKYCProvider.CreateVerification(ctx, in)
}

func (a countingKYCAdapter) SubmitVerification(ctx context.Context, ref string, docs []kyc.SubmittedDocument, call kyc.CallContext) (kyc.ProviderResult, error) {
	a.submits.Add(1)
	return a.MockKYCProvider.SubmitVerification(ctx, ref, docs, call)
}

func newCountingKYCServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer) (*httptest.Server, countingKYCAdapter) {
	t.Helper()
	adapter := countingKYCAdapter{MockKYCProvider: kyc.NewMockKYCProvider(), creates: new(atomic.Int32), submits: new(atomic.Int32)}
	srv := httptest.NewServer(New(Deps{
		Logger:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                     pool,
		AuthIssuer:             issuer,
		ServiceName:            "platform-api-test",
		AccessTokenTTL:         5 * time.Minute,
		RefreshTokenTTL:        time.Hour,
		PersonResolver:         identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:        newKYCOrchestratorWithWorker(t, pool, map[string]kyc.KYCProvider{"mock": adapter}, kyc.NewMockWebhookCredentials(adapter.MockKYCProvider)),
		KYCWebhookEnabled:      true,
		DocumentStorage:        kyc.NewMockDocumentStorageProvider(),
		MalwareScanner:         kyc.NewMockMalwareScanner(),
		KYCOutboundCredentials: kyc.NewMockOutboundResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv, adapter
}

func mustJSON(t *testing.T, raw []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

// outboxInternals are field names that must never appear in a player-facing
// response (and, for the staff read, never alongside submission_state).
var outboxInternals = []string{"outbox", "claim", "failed_attempts", "idempotency_key", "lease", "next_attempt", "document_ids", "attempts"}

func assertNoOutboxInternals(t *testing.T, label, body string) {
	t.Helper()
	low := strings.ToLower(body)
	for _, f := range outboxInternals {
		if strings.Contains(low, f) {
			t.Errorf("%s leaks outbox internals (%q): %s", label, f, body)
		}
	}
}

// Create: 201 + unverified, NO vendor call; repeat -> 200 with the SAME
// verification and the same body shape; another player gets their OWN (D2).
func TestKYCOutboxHTTP_CreateAsynchronous_RepeatReturnsExisting_CallerOwned(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, adapter := newCountingKYCServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	alice := mustRegisterPlayer(t, srv, brand.Slug)
	bob := mustRegisterPlayer(t, srv, brand.Slug)

	create := func(token string) (*http.Response, string, map[string]any) {
		resp := postJSON(t, srv, "/v1/me/kyc/verifications", token, map[string]any{})
		raw := rawResponseBody(t, resp)
		var m map[string]any
		mustJSON(t, raw, &m)
		return resp, string(raw), m
	}
	r1, body1, v1 := create(alice.Tokens.AccessToken)
	if r1.StatusCode != http.StatusCreated || v1["status"] != "unverified" {
		t.Fatalf("expected 201 + unverified, got %d %s", r1.StatusCode, body1)
	}
	assertNoOutboxInternals(t, "create response", body1)
	if adapter.creates.Load() != 0 {
		t.Fatalf("the HTTP create path must make NO vendor call, got %d", adapter.creates.Load())
	}

	r2, body2, v2 := create(alice.Tokens.AccessToken)
	if r2.StatusCode != http.StatusOK || v2["id"] != v1["id"] {
		t.Fatalf("a repeat create must return 200 with the existing verification, got %d %s", r2.StatusCode, body2)
	}
	if len(v1) != len(v2) {
		t.Fatalf("the 200 body must have the same shape as the 201 body: %s vs %s", body1, body2)
	}
	assertNoOutboxInternals(t, "repeat-create response", body2)

	_, bodyBob, vBob := create(bob.Tokens.AccessToken)
	if vBob["id"] == v1["id"] || vBob["player_account_id"] != bob.ID.String() {
		t.Fatalf("D2: Bob must get his OWN verification, never Alice's: %s", bodyBob)
	}

	// Still no vendor call; after the worker drains, one call per player.
	if adapter.creates.Load() != 0 {
		t.Fatalf("no vendor call may have happened yet, got %d", adapter.creates.Load())
	}
	drainKYCOutbox(t)
	if adapter.creates.Load() != 2 {
		t.Fatalf("the worker must make exactly one create call per player (2), got %d", adapter.creates.Load())
	}
	// Once the create is sent, a new create starts a new verification (201).
	r3, body3, v3 := create(alice.Tokens.AccessToken)
	if r3.StatusCode != http.StatusCreated || v3["id"] == v1["id"] {
		t.Fatalf("a create after the live row is sent must start a NEW verification (201), got %d %s", r3.StatusCode, body3)
	}
}

// Upload: no vendor call; the response is unchanged; no outbox field.
func TestKYCOutboxHTTP_UploadMakesNoVendorCall(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, adapter := newCountingKYCServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	resp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	var v map[string]any
	decodeBody(t, resp, &v)
	drainKYCOutbox(t) // the create is sent
	creates := adapter.creates.Load()

	up := postMultipartDocument(t, srv, player.Tokens.AccessToken, v["id"].(string), "passport", "p.png", tinyPNG)
	body := string(rawResponseBody(t, up))
	if up.StatusCode != http.StatusCreated {
		t.Fatalf("upload: expected 201, got %d %s", up.StatusCode, body)
	}
	assertNoOutboxInternals(t, "upload response", body)
	if adapter.submits.Load() != 0 || adapter.creates.Load() != creates {
		t.Fatalf("the HTTP upload path must make NO vendor call: submits=%d", adapter.submits.Load())
	}
	drainKYCOutbox(t)
	if adapter.submits.Load() != 1 {
		t.Fatalf("the worker must submit exactly once, got %d", adapter.submits.Load())
	}
	// Re-listing the player's own verifications shows no outbox field.
	list := getJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken)
	assertNoOutboxInternals(t, "player list", string(rawResponseBody(t, list)))
}

// Staff reads (T-I): the derived submission_state, read-only, with no counters,
// document data or outbox ids; a player response carries none of it.
func TestKYCOutboxHTTP_StaffReadsCarrySubmissionState_TI(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _ := newCountingKYCServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-outbox-state-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-outbox-state-1")

	resp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	var created map[string]any
	decodeBody(t, resp, &created)

	staffState := func() (casesState, accountState string) {
		cases := getJSON(t, srv, "/v1/admin/kyc/cases", tokens.AccessToken)
		body := string(rawResponseBody(t, cases))
		var page struct {
			Items []map[string]any `json:"items"`
		}
		mustJSON(t, []byte(body), &page)
		if len(page.Items) != 1 {
			t.Fatalf("expected one case, got %s", body)
		}
		assertNoOutboxInternals(t, "staff cases response", strings.NewReplacer("submission_last_error_class", "", "submission_cancel_reason", "").Replace(body))
		casesState, _ = page.Items[0]["submission_state"].(string)

		acct := getJSON(t, srv, "/v1/admin/kyc/verifications?player_account_id="+player.ID.String(), tokens.AccessToken)
		abody := string(rawResponseBody(t, acct))
		var rows []map[string]any
		mustJSON(t, []byte(abody), &rows)
		if len(rows) != 1 {
			t.Fatalf("expected one verification, got %s", abody)
		}
		accountState, _ = rows[0]["submission_state"].(string)
		return casesState, accountState
	}
	if c, a := staffState(); c != "queued" || a != "queued" {
		t.Fatalf("a waiting create must show queued, got cases=%q account=%q", c, a)
	}
	drainKYCOutbox(t)
	if c, a := staffState(); c != "sent" || a != "sent" {
		t.Fatalf("a sent create must show sent, got cases=%q account=%q", c, a)
	}
	// The player-facing shape has no submission_state at all.
	list := string(rawResponseBody(t, getJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken)))
	if strings.Contains(list, "submission") {
		t.Fatalf("a player response must carry no submission state: %s", list)
	}
}
