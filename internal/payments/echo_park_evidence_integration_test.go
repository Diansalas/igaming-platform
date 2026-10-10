//go:build integration

package payments

// GOV-R32 x GOV-R33: a destination_mismatch park caused by an unexpected echo from an Unsupported adapter is a destination
// park, so r32's durable park evidence must be written with the REPORTED outcome. A park triggered by a SUCCESS records
// `succeeded` and M4 not-paid is then refused; a park triggered by a decline records `declined` and keeps its governed exit.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
)

func TestEchoPark_UnexpectedEchoOnSuccess_RecordsSucceeded_AndRefusesNotPaid(t *testing.T) {
	m := newM4World(t) // the MOCK declares Unsupported
	wr, pend := m.payout(1020)
	x := *pend.ProviderReference
	// Any echo from an Unsupported adapter is untrusted.
	m.prov.setStatus(x, StatusResult{Outcome: OutcomeSucceeded, ProviderReference: x, Amount: 1020, AssetCode: "EUR",
		DestinationEcho: &payoutinstrument.DestinationEcho{Fingerprint: strings.Repeat("ab", 32), Kid: "any-kid"}})
	if err := PollPayoutStatus(context.Background(), m.pool, m.orch, MockCredentialResolver{}, m.f.tenantID, pend, time.Now().Add(time.Minute), nil); err != nil {
		t.Fatalf("poll: %v", err)
	}
	fresh := m.attempt(pend.ID)
	if fresh.State != AttemptDisputed || fresh.TerminalReason == nil || *fresh.TerminalReason != TerminalReasonDestinationMismatch {
		t.Fatalf("setup: %s %v", fresh.State, fresh.TerminalReason)
	}
	if n := m.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='payments.payout_parked_destination' AND target_id=$2 AND metadata->>'echo_verdict'='unexpected_from_unsupported'`, m.f.tenantID, pend.ID.String()); n != 1 {
		t.Fatalf("unexpected-echo park audit rows = %d", n)
	}
	if ev := m.r32ParkEvidence(pend.ID); !ev["succeeded/query_status"] {
		t.Fatalf("the success that triggered the park was not recorded durably: %v", ev)
	}
	p := &b11Parked{shape: "echo", wr: wr, stale: pend, fresh: fresh, ref: x, base: m.b11Snap(wr.ID, pend.ID)}
	m.declineOn(p, "r33-D-"+strings.ReplaceAll(pend.ID.String(), "-", "")[:12])
	m.mustEvidence(pend.ID, M4VerdictContradictory)
	_, err := m.request(m.acting, m.m4In(pend.ID, ResolutionM4EvidenceNotPaid, &m4AnyLine))
	k3RequireCode(t, err, "MR062")
}

func TestEchoPark_UnexpectedEchoOnDecline_RecordsDeclined(t *testing.T) {
	m := newM4World(t)
	_, pend := m.payout(1021)
	x := *pend.ProviderReference
	m.prov.setStatus(x, StatusResult{Outcome: OutcomeDeclined, ProviderReference: x, Amount: 1021, AssetCode: "EUR", DeclineReason: "insufficient_funds",
		DestinationEcho: &payoutinstrument.DestinationEcho{Fingerprint: strings.Repeat("ab", 32), Kid: "any-kid"}})
	if err := PollPayoutStatus(context.Background(), m.pool, m.orch, MockCredentialResolver{}, m.f.tenantID, pend, time.Now().Add(time.Minute), nil); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if a := m.attempt(pend.ID); a.State != AttemptDisputed {
		t.Fatalf("attempt = %s", a.State)
	}
	ev := m.r32ParkEvidence(pend.ID)
	if ev["succeeded/query_status"] || !ev["declined/query_status"] {
		t.Fatalf("a decline-triggered park must record declined only: %v", ev)
	}
}
