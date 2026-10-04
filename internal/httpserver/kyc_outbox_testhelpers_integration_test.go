//go:build integration

package httpserver

// PRH-2 E1 (ADR 0095 section 38, ADR 0106): no HTTP path calls a KYC vendor any
// more. A test that previously relied on the synchronous create/upload vendor
// call now drives the OUTBOX WORKER explicitly, with the same orchestrator and
// outbound credential resolver the server under test was built with.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

var kycTestWorkers struct {
	mu sync.Mutex
	ws []*kyc.OutboxWorker
}

// ---- parallel-package safety (code review F1) ----
//
// CI runs every package's integration tests in parallel against ONE database.
// A test never deletes outbox rows, and a test worker claims only the tenants
// the running top-level test created (kyc.OutboxWorker.ClaimScope, nil in
// production), so it never touches another test's or package's rows.

var kycScope struct {
	mu  sync.Mutex
	top string
	ids []uuid.UUID
}

// registerKYCScopeTenant adds a tenant the running test created to the claim
// scope; a new top-level test starts with an empty scope.
func registerKYCScopeTenant(t *testing.T, id uuid.UUID) {
	t.Helper()
	top := strings.SplitN(t.Name(), "/", 2)[0]
	kycScope.mu.Lock()
	defer kycScope.mu.Unlock()
	if kycScope.top != top {
		kycScope.top, kycScope.ids = top, nil
	}
	kycScope.ids = append(kycScope.ids, id)
}

func currentKYCScope() []uuid.UUID {
	kycScope.mu.Lock()
	defer kycScope.mu.Unlock()
	return append([]uuid.UUID{}, kycScope.ids...)
}

// registerKYCOutboxWorker builds a worker from the server-under-test's own
// orchestrator and resolver and registers it for drainKYCOutbox until the test
// ends.
func registerKYCOutboxWorker(t *testing.T, pool *db.Pool, orch *kyc.Orchestrator, creds kyc.OutboundCredentialResolver) *kyc.OutboxWorker {
	t.Helper()
	w := kyc.NewOutboxWorker(pool, orch, creds)
	w.ClaimScope = currentKYCScope
	kycTestWorkers.mu.Lock()
	kycTestWorkers.ws = append(kycTestWorkers.ws, w)
	kycTestWorkers.mu.Unlock()
	t.Cleanup(func() {
		kycTestWorkers.mu.Lock()
		defer kycTestWorkers.mu.Unlock()
		for i, x := range kycTestWorkers.ws {
			if x == w {
				kycTestWorkers.ws = append(kycTestWorkers.ws[:i], kycTestWorkers.ws[i+1:]...)
				break
			}
		}
	})
	return w
}

// newKYCOrchestratorWithWorker is kyc.NewOrchestrator plus a registered outbox
// worker over the SAME orchestrator and the MOCK outbound resolver (the one the
// builders pass as KYCOutboundCredentials).
func newKYCOrchestratorWithWorker(t *testing.T, pool *db.Pool, providers map[string]kyc.KYCProvider, resolver webhookauth.Resolver) *kyc.Orchestrator {
	t.Helper()
	orch := kyc.NewOrchestrator(providers, resolver)
	registerKYCOutboxWorker(t, pool, orch, kyc.NewMockOutboundResolver())
	return orch
}

// drainKYCOutbox runs every registered worker until a pass claims nothing
// (bounded). It is the test-side stand-in for the production background loop.
func drainKYCOutbox(t *testing.T) {
	t.Helper()
	kycTestWorkers.mu.Lock()
	ws := append([]*kyc.OutboxWorker(nil), kycTestWorkers.ws...)
	kycTestWorkers.mu.Unlock()
	for _, w := range ws {
		for i := 0; i < 50; i++ {
			if st := w.RunPass(context.Background()); st.Claimed == 0 {
				break
			}
		}
	}
}
