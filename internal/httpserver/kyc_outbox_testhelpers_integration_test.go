//go:build integration

package httpserver

// PRH-2 E1 (ADR 0095 section 38, ADR 0106): no HTTP path calls a KYC vendor any
// more. A test that previously relied on the synchronous create/upload vendor
// call now drives the OUTBOX WORKER explicitly, with the same orchestrator and
// outbound credential resolver the server under test was built with.

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

var kycTestWorkers struct {
	mu sync.Mutex
	ws []*kyc.OutboxWorker
}

// purgeKYCOutbox deletes every outbox row through the OWNER connection with the
// guard trigger disabled and FORCE RLS lifted in one transaction (fixture only:
// the guard forbids DELETE for every production session), so rows an earlier
// test left behind never reach this test's worker pass.
func purgeKYCOutbox(t *testing.T) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		return
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("owner connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("owner begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, stmt := range []string{
		`ALTER TABLE kyc_submission_outbox DISABLE TRIGGER USER`,
		`ALTER TABLE kyc_submission_outbox NO FORCE ROW LEVEL SECURITY`,
		`DELETE FROM kyc_submission_outbox`,
		`ALTER TABLE kyc_submission_outbox FORCE ROW LEVEL SECURITY`,
		`ALTER TABLE kyc_submission_outbox ENABLE TRIGGER USER`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("purge outbox %q: %v", stmt, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("purge commit: %v", err)
	}
}

// registerKYCOutboxWorker builds a worker from the server-under-test's own
// orchestrator and resolver and registers it for drainKYCOutbox until the test
// ends.
func registerKYCOutboxWorker(t *testing.T, pool *db.Pool, orch *kyc.Orchestrator, creds kyc.OutboundCredentialResolver) *kyc.OutboxWorker {
	t.Helper()
	w := kyc.NewOutboxWorker(pool, orch, creds)
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
	purgeKYCOutbox(t)
	t.Cleanup(func() { purgeKYCOutbox(t) })
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
