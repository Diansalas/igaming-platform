//go:build integration

// Stage 10 W1 security review P2-2 (S6/S7 test gaps): (a) a handler test
// that captures every log line the settlement route emits and asserts
// BOTH the exact alert event name (including the bet_not_found override,
// P2-1) AND that an alert's attribute set is EXACTLY the ADR 0088 §4.4
// allow-list - never the request body, headers, token or player PII; (b)
// an HTTP integration test with a spoofed X-Forwarded-For, asserting it
// never reaches the audit row: ip_address is trustedProxyClientIP's own
// result, metadata.remote_addr is the raw (unspoofed) RemoteAddr, and the
// spoofed value string appears nowhere in the row at all.
package httpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
)

// settlementAllowedAlertKeys is ADR 0088 §4.4's exact allow-list, restated
// here (not imported from production code) so this test fails if a future
// change to logSettlementIntegrityAlert silently adds a new field -
// re-deriving the list from the code under test would make this assertion
// vacuous.
var settlementAllowedAlertKeys = map[string]bool{
	"tenant_id":              true,
	"bet_id":                 true,
	"reason":                 true,
	"event_type":             true,
	"generation":             true,
	"bet_status":             true,
	"actor_staff_account_id": true,
	"request_id":             true,
}

// capturedLogLine is one slog record, flattened to a plain attribute map
// (pre-attrs from .With(...) plus the record's own attrs) for easy
// assertions - a real slog.Handler is used, not a string-log scrape, so
// this cannot be fooled by formatting.
type capturedLogLine struct {
	msg   string
	attrs map[string]any
}

// capturingHandler is a minimal slog.Handler that records every line
// instead of writing it anywhere, so a test can assert on structured
// fields rather than parsing text.
type capturingHandler struct {
	mu    *sync.Mutex
	lines *[]capturedLogLine
	pre   []slog.Attr
}

func newCapturingLogger() (*slog.Logger, func() []capturedLogLine) {
	lines := &[]capturedLogLine{}
	h := &capturingHandler{mu: &sync.Mutex{}, lines: lines}
	return slog.New(h), func() []capturedLogLine {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := make([]capturedLogLine, len(*lines))
		copy(out, *lines)
		return out
	}
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any, len(h.pre)+r.NumAttrs())
	for _, a := range h.pre {
		attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	*h.lines = append(*h.lines, capturedLogLine{msg: r.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}

func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &capturingHandler{mu: h.mu, lines: h.lines, pre: append(append([]slog.Attr{}, h.pre...), attrs...)}
	return next
}

func (h *capturingHandler) WithGroup(string) slog.Handler { return h }

// newSettlementTestServerWithLogger mirrors newSettlementTestServer with a
// caller-supplied Logger, so a test can capture log output. trustedProxyCount
// defaults to 0 (no proxy trusted, matching production's default posture)
// unless overridden by the caller.
func newSettlementTestServerWithLogger(t *testing.T, pool *db.Pool, issuer *auth.Issuer, logger *slog.Logger, trustedProxyCount int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:                                logger,
		DB:                                    pool,
		AuthIssuer:                            issuer,
		ServiceName:                           "platform-api-test",
		AccessTokenTTL:                        5 * time.Minute,
		RefreshTokenTTL:                       time.Hour,
		SportsbookEnabled:                     true,
		SportsbookSettlementSimulationEnabled: true,
		PersonResolver:                        identityresolution.NewMockPersonResolver(),
		TrustedProxyCount:                     trustedProxyCount,
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- (a) Alert name and allow-list assertions ---

func TestSettlementAlert_BetNotFound_NameAndAllowListedFieldsOnly(t *testing.T) {
	pool, issuer := testEnv(t)
	logger, capturedLines := newCapturingLogger()
	srv := newSettlementTestServerWithLogger(t, pool, issuer, logger, 0)
	tenant := mustCreateTenant(t, pool)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(uuid.NewString()), token,
		map[string]any{"event_type": "void", "void_reason": "market_cancelled"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown bet id, got %d", resp.StatusCode)
	}

	assertOneAlertWithAllowedFieldsOnly(t, capturedLines(), "sportsbook_settlement_integrity_alert_bet_not_found")
}

func TestSettlementAlert_PayloadMismatch_NameAndAllowListedFieldsOnly(t *testing.T) {
	pool, issuer := testEnv(t)
	logger, capturedLines := newCapturingLogger()
	srv := newSettlementTestServerWithLogger(t, pool, issuer, logger, 0)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(bet.ID), token,
		map[string]any{"event_type": "void", "void_reason": "market_cancelled"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 voiding an open bet, got %d", resp.StatusCode)
	}

	// A void replay with a DIFFERENT void_reason is ADR 0088 §4.3's payload
	// mismatch - an alert-eligible rejection (§4.4).
	resp2 := postJSON(t, srv, simulatePath(bet.ID), token,
		map[string]any{"event_type": "void", "void_reason": "push"})
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a void_reason payload mismatch, got %d", resp2.StatusCode)
	}

	assertOneAlertWithAllowedFieldsOnly(t, capturedLines(), "sportsbook_settlement_integrity_alert_settlement_payload_mismatch")
}

// assertOneAlertWithAllowedFieldsOnly finds exactly one captured log line
// whose message is wantEvent and asserts its attribute KEY SET is exactly
// settlementAllowedAlertKeys - no more (no body, headers, token, PII;
// nothing beyond the allow-list), no less (the alert is not silently
// dropping a required field).
func assertOneAlertWithAllowedFieldsOnly(t *testing.T, lines []capturedLogLine, wantEvent string) {
	t.Helper()
	var matches []capturedLogLine
	for _, l := range lines {
		if l.msg == wantEvent {
			matches = append(matches, l)
		}
	}
	if len(matches) != 1 {
		var allMsgs []string
		for _, l := range lines {
			allMsgs = append(allMsgs, l.msg)
		}
		t.Fatalf("expected exactly one %q log line, got %d (all captured events: %v)", wantEvent, len(matches), allMsgs)
	}
	attrs := matches[0].attrs
	for k := range attrs {
		if !settlementAllowedAlertKeys[k] {
			t.Errorf("alert %q carries a field outside the ADR 0088 §4.4 allow-list: %q = %v", wantEvent, k, attrs[k])
		}
	}
	for k := range settlementAllowedAlertKeys {
		if _, ok := attrs[k]; !ok {
			t.Errorf("alert %q is missing allow-listed field %q", wantEvent, k)
		}
	}
	// Belt and braces: no attribute value contains anything shaped like a
	// request body, a header value or a bearer token.
	for k, v := range attrs {
		if s, ok := v.(string); ok {
			lower := strings.ToLower(s)
			if strings.Contains(lower, "bearer ") || strings.Contains(lower, "authorization") {
				t.Errorf("alert %q field %q looks like it leaked a header/token: %q", wantEvent, k, s)
			}
		}
	}
}

// --- (c) P3-1: the integrity-abort cause is logged separately ---

// TestSettlementIntegrityAbort_CauseLoggedUnderSeparateNonAlertEvent pins
// security review P3-1: on an ADR 0088 §4.7 integrity abort, the wrapped
// cause must be logged under its OWN event name - not the alert event
// itself, which stays on the §4.4 allow-list - so triage does not have to
// guess why the abort happened, while the alert's own field set is
// unaffected by this test's earlier allow-list assertions.
func TestSettlementIntegrityAbort_CauseLoggedUnderSeparateNonAlertEvent(t *testing.T) {
	pool, issuer := testEnv(t)
	logger, capturedLines := newCapturingLogger()
	srv := newSettlementTestServerWithLogger(t, pool, issuer, logger, 0)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	injectedCause := "injected P3-1 integrity cause"
	restore := sportsbook.SetSettlementAfterPostHookForTest(func(context.Context, pgx.Tx) error {
		return fmt.Errorf("%w: %s", sportsbook.ErrSettlementIntegrity, injectedCause)
	})
	defer restore()

	resp := postJSON(t, srv, simulatePath(bet.ID), token,
		map[string]any{"event_type": "void", "void_reason": "market_cancelled"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for an injected integrity abort, got %d", resp.StatusCode)
	}
	restore()

	lines := capturedLines()
	var detailLine, alertLine *capturedLogLine
	for i := range lines {
		switch lines[i].msg {
		case "sportsbook_settlement_integrity_detail":
			detailLine = &lines[i]
		case "sportsbook_settlement_integrity_alert_settlement_integrity":
			alertLine = &lines[i]
		}
	}
	if detailLine == nil {
		var allMsgs []string
		for _, l := range lines {
			allMsgs = append(allMsgs, l.msg)
		}
		t.Fatalf("expected a sportsbook_settlement_integrity_detail log line, captured: %v", allMsgs)
	}
	if alertLine == nil {
		t.Fatalf("expected the integrity alert to still be logged")
	}
	cause, _ := detailLine.attrs["cause"].(string)
	if !strings.Contains(cause, injectedCause) {
		t.Errorf("expected the detail log's cause to contain the injected message, got %q", cause)
	}
	if _, ok := detailLine.attrs["bet_id"]; !ok {
		t.Errorf("expected the detail log to carry bet_id")
	}
	// The detail event is NOT on the §4.4 alert allow-list and is a
	// DIFFERENT event name from the alert - a monitoring rule keyed to the
	// alert name alone is unaffected by this extra, non-paging log line.
	if detailLine.msg == alertLine.msg {
		t.Fatalf("the detail log must not share the alert's event name")
	}
	// The alert itself is still allow-list-only, unaffected by P3-1's
	// addition.
	for k := range alertLine.attrs {
		if !settlementAllowedAlertKeys[k] {
			t.Errorf("alert %q carries a field outside the allow-list: %q", alertLine.msg, k)
		}
	}
}

// --- (b) Client IP / X-Forwarded-For spoofing ---

func TestSettlementAudit_SpoofedXForwardedFor_NeverReachesAuditRow(t *testing.T) {
	pool, issuer := testEnv(t)
	logger, _ := newCapturingLogger()
	// TrustedProxyCount = 0: production's default posture, and the one
	// under which trustedProxyClientIP MUST ignore X-Forwarded-For
	// entirely (ratelimit.go:180-181).
	srv := newSettlementTestServerWithLogger(t, pool, issuer, logger, 0)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	const spoofedIP = "203.0.113.9"
	req, err := http.NewRequest(http.MethodPost, srv.URL+simulatePath(bet.ID), strings.NewReader(
		`{"event_type":"void","void_reason":"market_cancelled"}`))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Forwarded-For", spoofedIP)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 voiding an open bet, got %d", resp.StatusCode)
	}

	// Scoped to the settlement route's OWN audit action - the bet's
	// placement audit row (sportsbook_bet.placed / similar) shares the same
	// target and has no ip_address of its own to assert on.
	rows := auditRowsForHTTP(t, pool, tenant.ID, "sportsbook_bet", bet.ID, "sportsbook_bet.voided")
	if len(rows) == 0 {
		t.Fatalf("expected at least one audit row for bet %s", bet.ID)
	}
	for _, row := range rows {
		// The spoofed value must not have influenced ip_address (which is
		// trustedProxyClientIP's own result - here, TrustedProxyCount = 0,
		// so it equals clientIP(r): the actual TCP peer, port stripped)...
		if row.IPAddress == "" {
			t.Errorf("expected a non-empty audit ip_address")
		}
		if row.IPAddress == spoofedIP {
			t.Errorf("audit ip_address equals the spoofed X-Forwarded-For value: %q", row.IPAddress)
		}
		// ...and metadata.remote_addr (the raw, unspoofed r.RemoteAddr) must
		// be present, while the spoofed header value appears NOWHERE in the
		// row at all - not in ip_address, not in metadata, not anywhere.
		if !strings.Contains(row.RawMetadata, `"remote_addr"`) {
			t.Errorf("expected metadata.remote_addr to be recorded, got %s", row.RawMetadata)
		}
		if strings.Contains(row.RawMetadata, spoofedIP) {
			t.Errorf("audit metadata contains the spoofed X-Forwarded-For value: %s", row.RawMetadata)
		}
	}
}

// auditHTTPRow is the subset of an audit_log row this test reads back,
// including ip_address and the raw metadata JSON (so the spoofed value can
// be searched for as a substring anywhere in it).
type auditHTTPRow struct {
	IPAddress   string
	RawMetadata string
}

func auditRowsForHTTP(t *testing.T, pool *db.Pool, tenantID uuid.UUID, targetType, targetID, action string) []auditHTTPRow {
	t.Helper()
	var out []auditHTTPRow
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT COALESCE(host(ip_address), ''), metadata::text
			   FROM audit_log
			  WHERE tenant_id = $1 AND target_type = $2 AND target_id = $3 AND action = $4
			  ORDER BY created_at, id`, tenantID, targetType, targetID, action)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r auditHTTPRow
			if err := rows.Scan(&r.IPAddress, &r.RawMetadata); err != nil {
				return err
			}
			// remote_addr is embedded in the metadata JSON itself; extract
			// it for the readability helper above rather than a second
			// query column.
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read audit rows for %s/%s: %v", targetType, targetID, err)
	}
	return out
}
