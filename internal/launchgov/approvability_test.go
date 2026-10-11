package launchgov

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// ADR 0112 slice 6 (d), DB-free: the pure can_approve rule, its precedence (the order Decide
// refuses in), the closed catalogue, and the cursor codec.

var (
	t0      = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	me      = uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	myPers  = uuid.MustParse("00000000-0000-0000-0000-0000000000b1")
	other   = uuid.MustParse("00000000-0000-0000-0000-0000000000a2")
	otherPr = uuid.MustParse("00000000-0000-0000-0000-0000000000b2")
)

func approver() Actor {
	return Actor{StaffID: me, Scope: ScopePlatform, Role: auth.RolePlatformAdmin}
}

func pendingReq() Request {
	return Request{
		ID: uuid.New(), Status: ReqPending, ExpiresAt: t0.Add(time.Hour),
		RequestedBy: other, RequestedByPersonID: otherPr, ConditionsNote: "",
	}
}

func okFacts() approvalFacts { return approvalFacts{MyPerson: myPers} }

func TestEvalApprovability_Matrix(t *testing.T) {
	expired := pendingReq()
	expired.Status = ReqExpired
	pastDue := pendingReq()
	pastDue.ExpiresAt = t0 // not before now: expired by time even though status still says pending
	executed := pendingReq()
	executed.Status = ReqExecuted
	self := pendingReq()
	self.RequestedBy = me
	samePerson := pendingReq()
	samePerson.RequestedByPersonID = myPers
	noReqPerson := pendingReq()
	noReqPerson.RequestedByPersonID = uuid.Nil
	withNote := pendingReq()
	withNote.ConditionsNote = "changed the licence reference"
	blankNote := pendingReq()
	blankNote.ConditionsNote = " \t\n"

	tenantAdmin := Actor{StaffID: me, Scope: ScopeTenant, TenantID: uuid.New(), Role: auth.RoleTenantAdmin}
	platformNoPerm := Actor{StaffID: me, Scope: ScopePlatform, Role: auth.RoleCompliance}
	complianceTenant := Actor{StaffID: me, Scope: ScopeTenant, TenantID: uuid.New(), Role: auth.RoleCompliance}
	platformWithTenant := Actor{StaffID: me, Scope: ScopePlatform, TenantID: uuid.New(), Role: auth.RolePlatformAdmin}

	cases := []struct {
		name   string
		a      Actor
		r      Request
		f      approvalFacts
		can    bool
		reason string
	}{
		{"happy path", approver(), pendingReq(), okFacts(), true, ""},
		{"tenant admin never", tenantAdmin, pendingReq(), okFacts(), false, CannotNotPlatformApprover},
		{"compliance in a platform scope has no :approve", platformNoPerm, pendingReq(), okFacts(), false, CannotNotPlatformApprover},
		{"compliance tenant", complianceTenant, pendingReq(), okFacts(), false, CannotNotPlatformApprover},
		{"platform actor carrying a tenant", platformWithTenant, pendingReq(), okFacts(), false, CannotNotPlatformApprover},
		{"unknown role", Actor{StaffID: me, Scope: ScopePlatform, Role: "nope"}, pendingReq(), okFacts(), false, CannotNotPlatformApprover},
		{"empty scope", Actor{StaffID: me, Role: auth.RolePlatformAdmin}, pendingReq(), okFacts(), false, CannotNotPlatformApprover},
		{"expired status", approver(), expired, okFacts(), false, CannotExpired},
		{"pending but past expires_at", approver(), pastDue, okFacts(), false, CannotExpired},
		{"executed", approver(), executed, okFacts(), false, CannotNotPending},
		{"self approval", approver(), self, okFacts(), false, CannotSelfApproval},
		{"same person", approver(), samePerson, okFacts(), false, CannotSamePerson},
		{"caller has no person", approver(), pendingReq(), approvalFacts{}, false, CannotNoPerson},
		{"requester has no person", approver(), noReqPerson, okFacts(), false, CannotNoPerson},
		{"already decided", approver(), pendingReq(), approvalFacts{MyPerson: myPers, DecidedByMyPerson: true}, false, CannotAlreadyDecided},
		{"prior reject, no note", approver(), pendingReq(), approvalFacts{MyPerson: myPers, PriorRejectByMyPerson: true}, false, CannotPriorReject},
		{"prior reject, blank note", approver(), blankNote, approvalFacts{MyPerson: myPers, PriorRejectByMyPerson: true}, false, CannotPriorReject},
		{"prior reject, with a change note", approver(), withNote, approvalFacts{MyPerson: myPers, PriorRejectByMyPerson: true}, true, ""},
		// precedence: the first failing rule in Decide's order wins
		{"self beats same person", approver(), func() Request { r := self; r.RequestedByPersonID = myPers; return r }(), okFacts(), false, CannotSelfApproval},
		{"not pending beats self", approver(), func() Request { r := self; r.Status = ReqRejected; return r }(), okFacts(), false, CannotNotPending},
		{"expired beats self", approver(), func() Request { r := self; r.Status = ReqExpired; return r }(), okFacts(), false, CannotExpired},
		{"not approver beats everything", tenantAdmin, self, approvalFacts{DecidedByMyPerson: true, PriorRejectByMyPerson: true}, false, CannotNotPlatformApprover},
		{"same person beats already decided", approver(), samePerson, approvalFacts{MyPerson: myPers, DecidedByMyPerson: true}, false, CannotSamePerson},
		{"already decided beats prior reject", approver(), pendingReq(), approvalFacts{MyPerson: myPers, DecidedByMyPerson: true, PriorRejectByMyPerson: true}, false, CannotAlreadyDecided},
	}
	for _, c := range cases {
		got := evalApprovability(c.a, c.r, t0, c.f)
		if got.CanApprove != c.can || got.Reason != c.reason {
			t.Errorf("%s: got {%v %q}, want {%v %q}", c.name, got.CanApprove, got.Reason, c.can, c.reason)
		}
	}
}

// One second before expiry is still approvable; the boundary instant is not (Decide uses
// now.Before(expires_at)).
func TestEvalApprovability_ExpiryBoundary(t *testing.T) {
	r := pendingReq()
	r.ExpiresAt = t0.Add(time.Second)
	if !evalApprovability(approver(), r, t0, okFacts()).CanApprove {
		t.Error("one second before expiry must be approvable")
	}
	r.ExpiresAt = t0
	if got := evalApprovability(approver(), r, t0, okFacts()); got.CanApprove || got.Reason != CannotExpired {
		t.Errorf("at the boundary: %+v", got)
	}
}

// Every reason the rule can return is in the closed catalogue, the catalogue has no duplicates and
// no reason could name a person (lowercase snake_case words only).
func TestCannotApproveReasons_ClosedAndPersonFree(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range CannotApproveReasons {
		if seen[r] {
			t.Errorf("duplicate reason %s", r)
		}
		seen[r] = true
		if strings.Trim(r, "abcdefghijklmnopqrstuvwxyz_") != "" {
			t.Errorf("reason %q is not a plain code", r)
		}
		if strings.Contains(r, "-") {
			t.Errorf("reason %q", r)
		}
	}
	for _, r := range []string{CannotNotPlatformApprover, CannotNotPending, CannotExpired, CannotSelfApproval, CannotSamePerson, CannotNoPerson, CannotAlreadyDecided, CannotPriorReject} {
		if !seen[r] {
			t.Errorf("%s missing from CannotApproveReasons", r)
		}
	}
	if len(seen) != 8 {
		t.Errorf("closed catalogue changed: %d", len(seen))
	}
}

func TestCursor_RoundTripAndStrictness(t *testing.T) {
	id := uuid.New()
	c := QueueCursor{CreatedAt: time.Date(2026, 10, 11, 12, 34, 56, 123456000, time.UTC), ID: id}
	got, err := DecodeCursor(EncodeCursor(c))
	if err != nil || !got.CreatedAt.Equal(c.CreatedAt) || got.ID != id {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	bad := []string{"", "!!!", "djEuMS4x", EncodeCursor(QueueCursor{CreatedAt: time.UnixMicro(0), ID: id}), strings.Repeat("A", 200)}
	for _, tok := range bad {
		if _, err := DecodeCursor(tok); err == nil {
			t.Errorf("%q must be rejected", tok)
		}
	}
	enc := func(raw string) string { return b64(raw) }
	for _, raw := range []string{
		"v2.1700000000000000." + id.String(),
		"v1.1700000000000000",
		"v1.+1700000000000000." + id.String(),
		"v1.01700000000000000." + id.String(),
		"v1.1700000000000000.not-a-uuid",
		"v1.1700000000000000." + uuid.Nil.String(),
		"v1.1700000000000000." + strings.ToUpper(id.String()),
		"v1.1700000000000000." + id.String() + ".x",
		"v1.-5." + id.String(),
	} {
		if _, err := DecodeCursor(enc(raw)); err == nil {
			t.Errorf("%q must be rejected", raw)
		}
	}
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
