//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// ADR 0112 slice 6 (c): the keyset comparison is the TUPLE (created_at, id): the cursor row itself
// is excluded, a row with the same timestamp and a smaller id is included, a larger id excluded.
// (created_at is forced to now() by the database, so equal timestamps cannot be fabricated; the
// tuple semantics are probed by pairing a real row's timestamp with extreme ids.)
func TestQueuePage_KeysetTupleSemantics_AndAuthorization(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		b := w.pendingBrand(tid)
		in := w.activation(launchgov.Subject{TenantID: tid, BrandID: &b}, jid)
		in.AcknowledgedCheckCodes = w.nonPassing(in)
		ids = append(ids, w.mustRequest(w.req, in).ID)
	}

	full, err := w.svc.ListRequestsAcrossTenants(ctx, w.ap1, nil, 200, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Items) < 3 {
		t.Fatalf("queue has %d rows", len(full.Items))
	}
	for i := 1; i < len(full.Items); i++ {
		p, c := full.Items[i-1], full.Items[i]
		if p.CreatedAt.Before(c.CreatedAt) || (p.CreatedAt.Equal(c.CreatedAt) && p.ID.String() < c.ID.String()) {
			t.Fatalf("not (created_at, id) DESC at %d", i)
		}
	}
	k := full.Items[1] // a row with a newer neighbour above it and an older one below
	has := func(p launchgov.QueuePage, id uuid.UUID) bool {
		for _, it := range p.Items {
			if it.ID == id {
				return true
			}
		}
		return false
	}
	page := func(c launchgov.QueueCursor) launchgov.QueuePage {
		t.Helper()
		p, err := w.svc.ListRequestsAcrossTenants(ctx, w.ap1, nil, 200, &c)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	maxID := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	minID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	if has(page(launchgov.QueueCursor{CreatedAt: k.CreatedAt, ID: k.ID}), k.ID) {
		t.Error("the cursor row itself must be excluded")
	}
	if !has(page(launchgov.QueueCursor{CreatedAt: k.CreatedAt, ID: maxID}), k.ID) {
		t.Error("same timestamp, larger cursor id: the row must be included (id is part of the key)")
	}
	if has(page(launchgov.QueueCursor{CreatedAt: k.CreatedAt, ID: minID}), k.ID) {
		t.Error("same timestamp, smaller cursor id: the row must be excluded")
	}
	// Everything after the cursor row is returned, in order, and the page above it is not.
	after := page(launchgov.QueueCursor{CreatedAt: k.CreatedAt, ID: k.ID})
	if has(after, full.Items[0].ID) && full.Items[0].CreatedAt.After(k.CreatedAt) {
		t.Error("a newer row must not reappear after the cursor")
	}
	// limit: default 50, cap 200, next cursor iff more rows exist.
	one, err := w.svc.ListRequestsAcrossTenants(ctx, w.ap1, nil, 1, nil)
	if err != nil || len(one.Items) != 1 || one.Next == nil || one.Next.ID != one.Items[0].ID {
		t.Fatalf("limit 1: %+v %v", one, err)
	}
	if big, err := w.svc.ListRequestsAcrossTenants(ctx, w.ap1, nil, 100000, nil); err != nil || len(big.Items) > launchgov.QueueMaxLimit {
		t.Fatalf("cap: %d %v", len(big.Items), err)
	}
	// Names and the caller's approvability ride on every row; the requester sees self_approval.
	for _, it := range full.Items {
		if it.TenantName == nil || it.Approvability == nil {
			t.Fatalf("row without names / approvability: %+v", it)
		}
	}
	mine, _ := w.svc.ListRequestsAcrossTenants(ctx, w.req, nil, 200, nil)
	for _, it := range mine.Items {
		if it.RequestedBy == w.req.StaffID && it.Status == launchgov.ReqPending && (it.Approvability.CanApprove || it.Approvability.Reason != launchgov.CannotSelfApproval) {
			t.Fatalf("own pending request: %+v", it.Approvability)
		}
	}
	_ = ids

	// Authorization: a tenant actor and a platform actor without :read are refused; reads are
	// never an acting session.
	if _, err := w.svc.ListRequestsAcrossTenants(ctx, w.tenantStaff(tid, auth.RoleTenantAdmin), nil, 10, nil); !errors.Is(err, launchgov.ErrForbidden) {
		t.Errorf("tenant actor: %v", err)
	}
	if _, err := w.svc.ListRequestsAcrossTenants(ctx, launchgov.Actor{StaffID: w.ap1.StaffID, Scope: launchgov.ScopePlatform, Role: auth.RoleSupport}, nil, 10, nil); !errors.Is(err, launchgov.ErrForbidden) {
		t.Errorf("platform support role: %v", err)
	}
}

// The prior-reject rule only looks back RequestTTL: a reject older than the window no longer blocks
// the Person's approval of a no-note variant. (The clock is shifted so the variant is still pending
// while the earlier reject has aged out; created_at is the database's, the service clock is ours.)
func TestApprovability_PriorRejectWindow(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	b := w.pendingBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &b}

	in := w.activation(sub, jid)
	in.AcknowledgedCheckCodes = w.nonPassing(in)
	first := w.mustRequest(w.req, in)
	if _, err := w.svc.Decide(ctx, w.ap1, tid, first.ID, launchgov.DecideInput{Decision: launchgov.DecisionReject, ReasonCode: "policy_concern"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	in2 := w.activation(sub, jid)
	in2.ConditionsNote = ""
	in2.AcknowledgedCheckCodes = w.nonPassing(in2)
	variant := w.mustRequest(w.req, in2)

	find := func(svc *launchgov.Service, who launchgov.Actor) *launchgov.Approvability {
		t.Helper()
		page, err := svc.ListRequestsAcrossTenants(ctx, who, []string{launchgov.ReqPending}, 200, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			if it.ID == variant.ID {
				return it.Approvability
			}
		}
		t.Fatal("variant not in the queue")
		return nil
	}
	if a := find(w.svc, w.ap1); a.CanApprove || a.Reason != launchgov.CannotPriorReject {
		t.Fatalf("inside the window: %+v", a)
	}
	if a := find(w.svc, w.ap2); !a.CanApprove {
		t.Fatalf("a Person that never rejected: %+v", a)
	}
	late := w.service(launchgov.Config{Clock: func() time.Time { return variant.CreatedAt.Add(launchgov.RequestTTL - 500*time.Millisecond) }})
	if a := find(late, w.ap1); !a.CanApprove {
		t.Fatalf("a reject older than the window must not block: %+v", a)
	}
}
