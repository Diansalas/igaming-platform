package jurisdiction

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Pure unit tests for canonical-model §6.2 scenario 9 ("unavailable
// resolver"): "refused(dependency_unavailable); the operation fails
// closed... Falling back to a previously-known-good answer... is
// prohibited". These cases were previously untested anywhere in the
// repository - resolver_test.go's own existing cases cover Resolve's
// INPUT gates (scenarios that never touch the database at all), but
// nothing exercised what Resolve does when its ONE real dependency
// (Postgres, via ReadOnlyQuerier) fails or returns integrity-broken data.
// That is exactly the gap this file closes, using a fake ReadOnlyQuerier
// rather than a real database, since:
//
//   - a genuine, non-ErrNoRows query error is what "unavailable resolver"
//     actually is (a dropped connection, a statement timeout, context
//     cancellation) - none of which the real Postgres test fixture can be
//     made to produce reliably or cheaply; and
//   - the licences.jurisdiction_id -> jurisdictions(id) / tenants.
//     licence_id -> licences(id) FK pair (confirmed at \d tenants / \d
//     licences against the live schema) makes the "licence row exists but
//     names a jurisdiction that doesn't" registry-integrity case
//     UNREACHABLE via any real database state - which is exactly why
//     resolver.go's own comment calls it "a registry integrity problem"
//     handled defensively rather than a state the schema permits. A fake
//     querier is the only way to exercise that defensive branch at all.
//
// In every case, the load-bearing assertion is the SAME one RISK/SEC's
// own "unavailable resolver" ruling makes: a dependency failure must
// surface as a genuine Go error (or refused(dependency_unavailable)) that
// every consumer's existing "if err != nil { return err }" / "if
// Outcome() != Resolved { deny }" shape already fails closed on - never a
// fabricated Resolution, and never a silent Unresolved that a future
// reader could mistake for an ordinary data gap (canonical-model §2.2:
// "unresolved vs. refused is not cosmetic... collapsing them makes an
// outage indistinguishable from a data gap").

// fakeRow is a minimal pgx.Row whose Scan is caller-supplied, letting
// each test case control exactly what QueryRow's second link in the
// chain (Row.Scan) returns without a real connection.
type fakeRow struct {
	scan func(dest ...any) error
}

func (f fakeRow) Scan(dest ...any) error { return f.scan(dest...) }

// fakeQuerier implements ReadOnlyQuerier, returning one canned pgx.Row per
// QueryRow call in sequence (resolveTenantLicence issues exactly two,
// in a fixed order: tenants.licence_id, then the licences/jurisdictions
// join). Query is never called by anything Resolve/IsActive exercise
// today and panics if it ever is, so a future accidental non-QueryRow
// read is caught immediately rather than silently returning zero rows.
type fakeQuerier struct {
	rows []pgx.Row
	n    int
}

func (f *fakeQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.n >= len(f.rows) {
		panic("fakeQuerier: more QueryRow calls than rows configured - resolver.go's own query sequence changed; update this test")
	}
	row := f.rows[f.n]
	f.n++
	return row
}

func (f *fakeQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	panic("fakeQuerier: Query is not used by any Stage 4I code path under test")
}

var errSimulatedConnectionFailure = errors.New("simulated: connection reset by peer")

// scanLicenceIDInto returns a fakeRow whose Scan sets the caller's
// *uuid.UUID (resolveTenantLicence's `var licenceID *uuid.UUID;
// Scan(&licenceID)`) to point at id.
func scanLicenceIDInto(id uuid.UUID) fakeRow {
	return fakeRow{scan: func(dest ...any) error {
		ptr := dest[0].(**uuid.UUID)
		v := id
		*ptr = &v
		return nil
	}}
}

func scanErr(err error) fakeRow {
	return fakeRow{scan: func(dest ...any) error { return err }}
}

func TestResolve_TenantLicenceLookupFailure_PropagatesAsGoError(t *testing.T) {
	q := &fakeQuerier{rows: []pgx.Row{scanErr(errSimulatedConnectionFailure)}}
	res, err := Resolve(context.Background(), q, Params{
		TenantID: uuid.New(), OperationClass: OperationCatalogueAvailability, RequestedByActorType: ActorSystem,
	})
	if err == nil {
		t.Fatal("expected a genuine Go error when the tenant-licence lookup itself fails - never a fabricated Resolution")
	}
	if !errors.Is(err, errSimulatedConnectionFailure) {
		t.Fatalf("expected the underlying error to be preserved (errors.Is), got %v", err)
	}
	if res.Outcome() == Resolved {
		t.Fatal("a failed lookup must never produce a Resolved outcome")
	}
	// The zero Resolution accompanying the error must itself be
	// structurally unusable - Code()/ID() unreachable - so a caller that
	// carelessly ignores the error still cannot launder a value out of it.
	if _, err := res.Code(); err == nil {
		t.Fatal("Code() must be unreachable on the zero Resolution returned alongside an error")
	}
}

func TestResolve_LicenceJurisdictionJoinFailure_PropagatesAsGoError(t *testing.T) {
	licenceID := uuid.New()
	q := &fakeQuerier{rows: []pgx.Row{
		scanLicenceIDInto(licenceID),
		scanErr(errSimulatedConnectionFailure),
	}}
	res, err := Resolve(context.Background(), q, Params{
		TenantID: uuid.New(), OperationClass: OperationCatalogueAvailability, RequestedByActorType: ActorSystem,
	})
	if err == nil {
		t.Fatal("expected a genuine Go error when the licence->jurisdiction join fails")
	}
	if !errors.Is(err, errSimulatedConnectionFailure) {
		t.Fatalf("expected the underlying error to be preserved (errors.Is), got %v", err)
	}
	if res.Outcome() == Resolved {
		t.Fatal("a failed lookup must never produce a Resolved outcome")
	}
}

// TestResolve_OrphanedLicenceReference_RefusesDependencyUnavailable proves
// the defensive branch resolver.go's own comment names: tenants.licence_id
// naming a licence that does not resolve to a registered jurisdiction is
// treated as refused(dependency_unavailable) - a registry INTEGRITY
// problem, never silently collapsed into unresolved(no_signal) ("not
// configured yet"). canonical-model §2.2: conflating these makes an
// outage indistinguishable from a data gap. This state is unreachable via
// the real schema's own FK pair (tenants.licence_id -> licences(id),
// licences.jurisdiction_id -> jurisdictions(id)) - which is exactly why a
// fake querier, not an integration test, is what exercises it.
func TestResolve_OrphanedLicenceReference_RefusesDependencyUnavailable(t *testing.T) {
	licenceID := uuid.New()
	q := &fakeQuerier{rows: []pgx.Row{
		scanLicenceIDInto(licenceID),
		scanErr(pgx.ErrNoRows),
	}}
	res, err := Resolve(context.Background(), q, Params{
		TenantID: uuid.New(), OperationClass: OperationCatalogueAvailability, RequestedByActorType: ActorSystem,
	})
	if err != nil {
		t.Fatalf("unexpected error (this case is a REFUSED outcome, not a Go error): %v", err)
	}
	if res.Outcome() != Refused {
		t.Fatalf("expected Refused, got %s(%s)", res.Outcome(), res.Reason())
	}
	if res.Reason() != ReasonDependencyUnavailable {
		t.Fatalf("expected dependency_unavailable, got %q - a registry integrity gap must never be reported as an ordinary no_signal data gap", res.Reason())
	}
	if _, err := res.Code(); err == nil {
		t.Fatal("Code() must be unreachable for a Refused outcome")
	}
}

// TestIsActive_GenuineQueryFailure_FailsClosed proves R-2b's own read
// accessor (canonical-model §4.2: "Risk consumes it read-only through one
// narrow accessor") does not collapse a genuine dependency failure into
// the same `false` it returns for "no row configured" - a caller cannot
// tell a real outage apart from "resolution is not active here" if both
// return (false, nil), which would make an outage look identical to an
// intentional off switch.
func TestIsActive_GenuineQueryFailure_FailsClosed(t *testing.T) {
	q := &fakeQuerier{rows: []pgx.Row{scanErr(errSimulatedConnectionFailure)}}
	active, err := IsActive(context.Background(), q, uuid.New(), OperationPlay)
	if err == nil {
		t.Fatal("expected a genuine Go error when the resolution-active lookup itself fails")
	}
	if !errors.Is(err, errSimulatedConnectionFailure) {
		t.Fatalf("expected the underlying error to be preserved (errors.Is), got %v", err)
	}
	if active {
		t.Fatal("a failed lookup must never report active=true")
	}
}

// TestIsActive_NoRowConfigured_FailsClosedFalse is the ordinary,
// non-failure counterpart to the case above: no row at all is a
// LEGITIMATE, expected state (nothing has ever set this fact), and is
// reported as false with NO error - distinct from a genuine dependency
// failure, which is always a non-nil error (proven above). Collapsing
// these two into the same return shape would be the exact
// outage-vs-data-gap conflation canonical-model §2.2 forbids.
func TestIsActive_NoRowConfigured_FailsClosedFalse(t *testing.T) {
	q := &fakeQuerier{rows: []pgx.Row{scanErr(pgx.ErrNoRows)}}
	active, err := IsActive(context.Background(), q, uuid.New(), OperationPlay)
	if err != nil {
		t.Fatalf("unexpected error for the ordinary no-row-configured case: %v", err)
	}
	if active {
		t.Fatal("expected false for a (tenant, operation_class) pair with no row at all")
	}
}
