//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// serializableURL returns the runtime URL with default_transaction_isolation=repeatable read set for
// every connection (what a misconfigured pool would look like).
func serializableURL(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	// options=-c default_transaction_isolation=repeatable\ read, percent-encoded by hand (the
	// libpq options value keeps a literal backslash-space; url.Values would encode a space as '+').
	u.RawQuery += "&options=-c%20default_transaction_isolation%3Drepeatable%5C%20read"
	return u.String()
}

// The executor requires READ COMMITTED (ADR 0112 12.11): the Go service refuses to open a governed
// transaction under another isolation level, and the database independently refuses an
// approval-based pending -> executing under REPEATABLE READ (LA011).
func TestIsolation_ApprovalBasedExecution_RequiresReadCommitted(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	brand := w.pendingBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	r := w.mustRequest(w.req, w.activation(sub, jid))

	// Go: a pool whose sessions default to REPEATABLE READ is refused before any write.
	rr := connect(t, serializableURL(t, w.runtimeURL))
	var iso string
	if err := rr.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&iso)
	}); err != nil || iso != "repeatable read" {
		t.Fatalf("the probe pool must really be REPEATABLE READ: %q err=%v", iso, err)
	}
	svc, err := launchgov.New(launchgov.Config{Pool: rr, Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Decide(ctx, w.ap1, tid, r.ID, launchgov.DecideInput{Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: w.seen(r)})
	if !errors.Is(err, launchgov.ErrIsolationLevel) {
		t.Fatalf("a REPEATABLE READ session must be refused by the service: %v", err)
	}
	act := w.activeBrand(tid)
	if _, err := svc.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: launchgov.Subject{TenantID: tid, BrandID: &act}, ReasonCode: "operator_request"}); !errors.Is(err, launchgov.ErrIsolationLevel) {
		t.Fatalf("a REPEATABLE READ suspension must be refused by the service: %v", err)
	}
	if _, apps, _ := w.svc.GetRequest(ctx, w.ap1, tid, r.ID); len(apps) != 0 {
		t.Fatalf("the refused attempts recorded %d approvals", len(apps))
	}

	// The database, independently: a raw approval-based execution under REPEATABLE READ (LA011), with
	// real proofs so the proof guard is not what refuses it.
	err = rr.WithPlatformAdmin(ctx, w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		if err := prooftest.AttachForSession(ctx, tx, actorproof.OpLaunchApprove, r.ID.String(), r.PayloadHash); err != nil {
			return err
		}
		if err := rawApproval(ctx, tx, r.ID, r.PayloadHash, "approve"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, r.ID)
		return err
	})
	w.wantCode("approval-based execution under REPEATABLE READ", err, "LA011")
	if !strings.Contains(err.Error(), "READ COMMITTED") {
		t.Fatalf("the refusal must name the cause: %v", err)
	}
}
