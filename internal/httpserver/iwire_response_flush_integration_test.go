//go:build integration

package httpserver

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// Final round, as corrected by the I-wire final review (F1/F2): the ONLY
// integration-tested early flush is the kill-switch engage, flushed INLINE right
// after its response is written (never from a defer: a deferred flush turns a
// panic into a 200, see iwire_panic_status_integration_test.go). The webhook,
// casino and simulation-generic paths do NOT flush; their post-response alert
// work runs after the response write but before the handler returns. The proof
// has no wall-clock assertion: the detached alert INSERT blocks on an advisory
// lock the test holds; the client must receive status and headers while that raise
// is still blocked; only then is the lock released and the alert must appear.
// Note this proves status line and headers only: the chunked body's terminating
// chunk (EOF) is still sent when the handler returns, after the raise.

const flushLockKeyBase int64 = 0x1F1A5000

// within runs f and fails the test if it does not finish (failure guard only;
// never an assertion about how long the happy path takes).
func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("%s did not return while the post-response alert work was blocked: the response was not flushed before the alert work", what)
	}
}

func TestIWire_ResponseFlush_KillSwitchEngage_ClientGetsResponseBeforeTheRaiseCompletes(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	admin := a.tenantStaff(tenant, "tenant_admin")
	tok := a.token(admin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	key := flushLockKeyBase + 1
	alertinject.InstallBlockDetached(t, a.pool, tenant, key)
	release := alertinject.HoldAdvisoryLock(t, a.pool, key)

	var status int
	within(t, "kill-switch engage", func() {
		// Read the status line and headers only: a.do would ReadAll the body,
		// which (chunked after a flush) ends only when the handler returns.
		req, err := http.NewRequest("POST", a.srv.URL+"/v1/admin/tenants/"+tenant.String()+"/payments/kill-switches",
			strings.NewReader(`{"provider_scope":"*","operation_scope":"deposit","reason_code":"flush_check"}`))
		if err != nil {
			t.Error(err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		status = resp.StatusCode
		defer func() { _ = resp.Body.Close() }()
	})
	if status != http.StatusOK {
		t.Fatalf("engage: %d", status)
	}
	if rows := alertinject.ForSubject(t, a.pool, tenant); len(rows) != 0 {
		t.Fatalf("the raise must still be blocked when the client has its response, got %+v", rows)
	}
	release()
	a.srv.Close() // waits for the handler (and its post-response raise) to finish
	if rows := alertinject.Find(alertinject.ForSubject(t, a.pool, tenant), string(alerting.KindPaymentKillSwitchEngaged)); len(rows) != 1 {
		t.Fatalf("after the lock is released the engage alert must exist, got %+v", rows)
	}
}
