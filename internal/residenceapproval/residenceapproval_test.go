package residenceapproval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/db"
)

type neverPool struct{ called bool }

func (p *neverPool) WithPrincipalScope(context.Context, uuid.UUID, uuid.UUID, db.TxFunc) error {
	p.called = true
	return nil
}

func TestModes_ClosedAndOrdered(t *testing.T) {
	if ModeNone.Rank() >= ModeIntoOpenMarket.Rank() || ModeIntoOpenMarket.Rank() >= ModeAll.Rank() {
		t.Fatal("strictness order must be none < into_open_market < all")
	}
	for _, m := range []Mode{"", "ALL", "open", "some", "none "} {
		if m.Valid() || m.Rank() != -1 {
			t.Errorf("%q must be invalid", m)
		}
	}
}

func TestReasonCode_IsACodeNeverFreeText(t *testing.T) {
	for _, ok := range []string{"a", "policy_review", "REF-2026.10:a"} {
		if !ValidReasonCode(ok) {
			t.Errorf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"", " x", "a b", "_a", "a\n", strings.Repeat("a", 65), "é"} {
		if ValidReasonCode(bad) {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// Invalid input is refused BEFORE any database work or any proof is signed.
func TestService_RefusesBadInputBeforeAnyDatabaseWork(t *testing.T) {
	pool := &neverPool{}
	svc, err := New(Config{Pool: pool})
	if err != nil {
		t.Fatal(err)
	}
	a := Actor{StaffID: uuid.New(), Scope: ScopeTenant, TenantID: uuid.New()}
	ctx := context.Background()
	if _, err := svc.SetPolicy(ctx, a, "bogus", "r"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad mode: %v", err)
	}
	if _, err := svc.SetPolicy(ctx, a, ModeAll, "not a code"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad reason: %v", err)
	}
	if _, err := svc.ApprovePolicy(ctx, a, uuid.Nil, "r"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("nil policy: %v", err)
	}
	if err := svc.ApproveDetermination(ctx, a, uuid.Nil, time.Now(), "r"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("nil verification: %v", err)
	}
	if err := svc.ApproveDetermination(ctx, a, uuid.New(), time.Time{}, "r"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("zero determined_at: %v", err)
	}
	// Only a tenant staff session exists: any other scope (including platform_acting, which is not built) is refused.
	for _, bad := range []Actor{{}, {StaffID: uuid.New(), Scope: ScopeTenant}, {StaffID: uuid.New(), TenantID: uuid.New(), Scope: "platform"},
		{StaffID: uuid.New(), TenantID: uuid.New(), Scope: "platform_acting"}} {
		if _, err := svc.SetPolicy(ctx, bad, ModeAll, "r"); !errors.Is(err, ErrActorScope) {
			t.Errorf("actor %+v: %v, want ErrActorScope", bad, err)
		}
	}
	if pool.called {
		t.Fatal("the database must not be reached for any of the refused inputs")
	}
}

func TestClassify_RDCodes(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code, Message: "m"} }
	for code, want := range map[string]error{
		"RD001": ErrForbidden, "CG001": ErrForbidden, "CG020": ErrForbidden, "RD002": ErrInvalidInput, "RD003": ErrNotPending,
		"RD004": ErrFourEyes, "RD006": ErrNoPerson, "RD040": ErrNotFound, "23505": ErrDuplicate, "AP002": ErrForbidden,
	} {
		if got := classify(pg(code)); !errors.Is(got, want) {
			t.Errorf("%s: %v, want %v", code, got, want)
		}
	}
	other := errors.New("plain")
	if classify(other) != other || classify(nil) != nil {
		t.Error("non-database errors pass through, nil stays nil")
	}
}

// The proof payload of each call is exactly the canonical text migration 0181's functions hash. This pins the Go side of the
// binding (the SQL side is pinned by the integration test): a drift would make every governed write fail closed.
func TestProofPayloads_AreTheCanonicalForm(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	pol := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	if actorproof.Digest(tenantS(tenant), actorproof.S("all"), actorproof.S("r1")) ==
		actorproof.Digest(tenantS(tenant), actorproof.S("none"), actorproof.S("r1")) {
		t.Fatal("the mode must be bound into the digest")
	}
	if actorproof.Digest(tenantS(tenant), actorproof.S(pol.String()), actorproof.S("r1")) ==
		actorproof.Digest(tenantS(tenant), actorproof.S(pol.String()), actorproof.S("r2")) {
		t.Fatal("the reason code must be bound into the digest")
	}
	at := time.Date(2026, 10, 20, 1, 2, 3, 456789000, time.UTC)
	if got := *actorproof.TS(&at); got != "2026-10-20T01:02:03.456789Z" {
		t.Fatalf("determined_at text = %q, SQL to_char(... 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') form expected", got)
	}
}
