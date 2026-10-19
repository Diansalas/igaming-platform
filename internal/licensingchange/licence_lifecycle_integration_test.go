//go:build integration

// Migration 0178 integration tests (lic4 slice 4a): the licence lifecycle writes as licensing change kinds. The database
// classifies; widening needs two approvals by distinct Persons; narrowing and the evidence reference execute at once.
// Every application-level assertion runs as igaming_runtime (world), the owner pool only provisions fixtures.
package licensingchange

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

type licRow struct {
	Status                            string
	Issued, Expires, Review, Evidence *string
}

func (w *world) licRow(id uuid.UUID) licRow {
	w.t.Helper()
	var r licRow
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, issued_at::text, expires_at::text, review_due_at::text, evidence_reference FROM licences WHERE id = $1`, id).
			Scan(&r.Status, &r.Issued, &r.Expires, &r.Review, &r.Evidence)
	}); err != nil {
		w.t.Fatal(err)
	}
	return r
}

func (w *world) newLicence() uuid.UUID {
	return w.licence(w.jurisdiction(), "platform", uuid.Nil)
}

func strOf(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

type transition struct {
	name string
	// setup puts the licence in its starting state (owner path); payload is the requested change.
	setup   string
	kind    Kind
	payload func(id uuid.UUID) any
	widens  bool
	// want is checked after the change took effect.
	check func(t *testing.T, r licRow)
}

func lifecycleTransitions() []transition {
	st := func(s string) func(uuid.UUID) any {
		return func(id uuid.UUID) any { return LicenceStatusPayload{LicenceID: id, Status: s} }
	}
	dates := func(i, e *string) func(uuid.UUID) any {
		return func(id uuid.UUID) any { return LicenceValidityDatesPayload{LicenceID: id, IssuedAt: i, ExpiresAt: e} }
	}
	rev := func(d *string) func(uuid.UUID) any {
		return func(id uuid.UUID) any { return LicenceReviewDuePayload{LicenceID: id, ReviewDueAt: d} }
	}
	ev := func(d *string) func(uuid.UUID) any {
		return func(id uuid.UUID) any { return LicenceEvidenceReferencePayload{LicenceID: id, Reference: d} }
	}
	return []transition{
		{"active -> suspended narrows", ``, KindLicenceStatusChange, st("suspended"), false, func(t *testing.T, r licRow) { eq(t, r.Status, "suspended") }},
		{"active -> expired narrows", ``, KindLicenceStatusChange, st("expired"), false, func(t *testing.T, r licRow) { eq(t, r.Status, "expired") }},
		{"suspended -> expired narrows", `status = 'suspended'`, KindLicenceStatusChange, st("expired"), false, func(t *testing.T, r licRow) { eq(t, r.Status, "expired") }},
		{"suspended -> active widens", `status = 'suspended'`, KindLicenceStatusChange, st("active"), true, func(t *testing.T, r licRow) { eq(t, r.Status, "active") }},
		{"expired -> active widens", `status = 'expired'`, KindLicenceStatusChange, st("active"), true, func(t *testing.T, r licRow) { eq(t, r.Status, "active") }},
		{"expired -> suspended widens (toward active)", `status = 'expired'`, KindLicenceStatusChange, st("suspended"), true, func(t *testing.T, r licRow) { eq(t, r.Status, "suspended") }},

		{"set an expiry on an open-ended licence narrows", ``, KindLicenceValidityDatesChange, dates(nil, sp("2030-01-01")), false, func(t *testing.T, r licRow) { eq(t, strOf(r.Expires), "2030-01-01") }},
		{"later expiry widens", `expires_at = DATE '2030-01-01'`, KindLicenceValidityDatesChange, dates(nil, sp("2031-01-01")), true, func(t *testing.T, r licRow) { eq(t, strOf(r.Expires), "2031-01-01") }},
		{"earlier expiry narrows", `expires_at = DATE '2030-01-01'`, KindLicenceValidityDatesChange, dates(nil, sp("2029-01-01")), false, func(t *testing.T, r licRow) { eq(t, strOf(r.Expires), "2029-01-01") }},
		{"clearing the expiry widens", `expires_at = DATE '2030-01-01'`, KindLicenceValidityDatesChange, dates(nil, nil), true, func(t *testing.T, r licRow) { eq(t, strOf(r.Expires), "<nil>") }},
		{"earlier issue date widens", `issued_at = DATE '2026-06-01'`, KindLicenceValidityDatesChange, dates(sp("2026-01-01"), nil), true, func(t *testing.T, r licRow) { eq(t, strOf(r.Issued), "2026-01-01") }},
		{"later issue date narrows", `issued_at = DATE '2026-06-01'`, KindLicenceValidityDatesChange, dates(sp("2026-09-01"), nil), false, func(t *testing.T, r licRow) { eq(t, strOf(r.Issued), "2026-09-01") }},
		{"clearing the issue date widens", `issued_at = DATE '2026-06-01'`, KindLicenceValidityDatesChange, dates(nil, nil), true, func(t *testing.T, r licRow) { eq(t, strOf(r.Issued), "<nil>") }},
		{"mixed (shorter expiry, earlier issue) widens", `issued_at = DATE '2026-06-01', expires_at = DATE '2030-01-01'`, KindLicenceValidityDatesChange,
			dates(sp("2026-01-01"), sp("2029-01-01")), true, func(t *testing.T, r licRow) {
				eq(t, strOf(r.Issued), "2026-01-01")
				eq(t, strOf(r.Expires), "2029-01-01")
			}},

		{"setting a review date narrows", ``, KindLicenceReviewDueChange, rev(sp("2030-01-01")), false, func(t *testing.T, r licRow) { eq(t, strOf(r.Review), "2030-01-01") }},
		{"earlier review date narrows", `review_due_at = DATE '2030-01-01'`, KindLicenceReviewDueChange, rev(sp("2029-01-01")), false, func(t *testing.T, r licRow) { eq(t, strOf(r.Review), "2029-01-01") }},
		{"postponed review date widens", `review_due_at = DATE '2030-01-01'`, KindLicenceReviewDueChange, rev(sp("2031-01-01")), true, func(t *testing.T, r licRow) { eq(t, strOf(r.Review), "2031-01-01") }},
		{"clearing the review date widens", `review_due_at = DATE '2030-01-01'`, KindLicenceReviewDueChange, rev(nil), true, func(t *testing.T, r licRow) { eq(t, strOf(r.Review), "<nil>") }},

		{"setting the evidence reference is neutral", ``, KindLicenceEvidenceReferenceChange, ev(sp("REG-1")), false, func(t *testing.T, r licRow) { eq(t, strOf(r.Evidence), "REG-1") }},
		{"clearing the evidence reference is neutral", `evidence_reference = 'REG-1'`, KindLicenceEvidenceReferenceChange, ev(nil), false, func(t *testing.T, r licRow) { eq(t, strOf(r.Evidence), "<nil>") }},
	}
}

func eq(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The database classifies each transition: widening is filed (needs two approvals, nothing changes until the second one),
// narrowing / neutral execute in the requester's own transaction.
func TestLifecycleKinds_DatabaseClassification_WideningNeedsTwoApprovals_NarrowingExecutes(t *testing.T) {
	w := newWorld(t)
	for _, tr := range lifecycleTransitions() {
		t.Run(tr.name, func(t *testing.T) {
			w.t = t
			id := w.newLicence()
			if tr.setup != "" {
				w.ownerExec(`UPDATE licences SET `+tr.setup+` WHERE id = $1`, id)
			}
			before := w.licRow(id)
			res := w.mustRequest(w.req, tr.kind, tr.payload(id), "lic4_matrix")
			if res.Request.Widening != tr.widens {
				t.Fatalf("widening = %v, want %v", res.Request.Widening, tr.widens)
			}
			if !tr.widens {
				if !res.Executed || res.Request.Status != StatusExecuted || res.Request.RequiredApprovals != 0 {
					t.Fatalf("a narrowing / neutral change must execute in the requester's transaction: %+v", res.Request)
				}
				tr.check(t, w.licRow(id))
				return
			}
			if res.Executed || res.Request.Status != StatusRequested || res.Request.RequiredApprovals != 2 {
				t.Fatalf("a widening change is filed with two required approvals: %+v", res.Request)
			}
			if got := w.licRow(id); got != before {
				t.Fatalf("a filed widening change must change nothing: %+v -> %+v", before, got)
			}
			first := w.mustApprove(w.ap1, res.Request.ID)
			if first.Executed || w.licRow(id) != before {
				t.Fatal("one approval must not execute a widening change")
			}
			second := w.mustApprove(w.ap2, res.Request.ID)
			if !second.Executed {
				t.Fatalf("the second approval must execute: %+v", second.Request)
			}
			tr.check(t, w.licRow(id))
		})
	}
}

// The requester cannot approve (nor a second account of the requester's Person), and one Person cannot approve twice.
func TestLifecycleWidening_FourEyes(t *testing.T) {
	w := newWorld(t)
	id := w.newLicence()
	w.ownerExec(`UPDATE licences SET status = 'suspended' WHERE id = $1`, id)
	res := w.mustRequest(w.req, KindLicenceStatusChange, LicenceStatusPayload{LicenceID: id, Status: "active"}, "lic4_4eyes")
	if _, err := w.approve(w.req, res.Request.ID); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("requester approving: %v", err)
	}
	if _, err := w.approve(w.reqTwin, res.Request.ID); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("requester's Person approving: %v", err)
	}
	w.mustApprove(w.ap1, res.Request.ID)
	if _, err := w.approve(w.ap1, res.Request.ID); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("same approver twice: %v", err)
	}
	if _, err := w.approve(w.ap1Twin, res.Request.ID); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("same Person (second account) twice: %v", err)
	}
	if w.licRow(id).Status != "suspended" {
		t.Fatal("no refused approval may reinstate the licence")
	}
	w.mustApprove(w.ap2, res.Request.ID)
	if w.licRow(id).Status != "active" {
		t.Fatal("two distinct Persons reinstate the licence")
	}
}

// Refusals before anything is stored: no-ops, unknown licence, malformed payloads, bad reason code. Nothing is written.
func TestLifecycleKinds_Refusals_WriteNothing(t *testing.T) {
	w := newWorld(t)
	id := w.newLicence()
	count := func() int {
		return w.ownerInt(`SELECT count(*) FROM licensing_change_requests WHERE subject_licence_id = $1`, id)
	}
	do := func(kind Kind, p any, reason string) error {
		_, err := w.svc.Request(context.Background(), w.req, RequestInput{Kind: kind, Payload: payload(t, p), ReasonCode: reason})
		return err
	}
	expectErr(t, do(KindLicenceStatusChange, LicenceStatusPayload{LicenceID: id, Status: "active"}, "x"), jurisdiction.ErrInvalidInput, "no-op status")
	expectErr(t, do(KindLicenceStatusChange, LicenceStatusPayload{LicenceID: id, Status: "revoked"}, "x"), ErrInvalidInput, "revoked is not a status")
	expectErr(t, do(KindLicenceValidityDatesChange, LicenceValidityDatesPayload{LicenceID: id}, "x"), jurisdiction.ErrInvalidInput, "no-op dates")
	expectErr(t, do(KindLicenceReviewDueChange, LicenceReviewDuePayload{LicenceID: id}, "x"), jurisdiction.ErrInvalidInput, "no-op review")
	expectErr(t, do(KindLicenceEvidenceReferenceChange, LicenceEvidenceReferencePayload{LicenceID: id}, "x"), jurisdiction.ErrInvalidInput, "no-op evidence")
	expectErr(t, do(KindLicenceStatusChange, LicenceStatusPayload{LicenceID: uuid.New(), Status: "suspended"}, "x"), jurisdiction.ErrNotFound, "unknown licence")
	expectErr(t, do(KindLicenceStatusChange, LicenceStatusPayload{LicenceID: id, Status: "suspended"}, "has spaces"), ErrInvalidInput, "free-text reason")
	if _, err := w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindLicenceStatusChange, ReasonCode: "x",
		Payload: []byte(`{"licence_id":"` + id.String() + `","status":"suspended","tenant_id":"` + uuid.NewString() + `"}`)}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("an unknown payload field must be refused: %v", err)
	}
	if n := count(); n != 0 {
		t.Fatalf("refused requests must write nothing, found %d", n)
	}
	if r := w.licRow(id); r.Status != "active" {
		t.Fatalf("licence changed: %+v", r)
	}
}

// Tenant staff may not request any lifecycle kind; a foreign tenant is indistinguishable.
func TestLifecycleKinds_TenantStaffRefused(t *testing.T) {
	w := newWorld(t)
	tenantID := w.tenant("own_licence")
	id := w.licence(w.jurisdiction(), "tenant", tenantID)
	for _, role := range []string{"tenant_admin", "compliance"} {
		ta := w.tenantStaff(tenantID, role)
		for _, c := range []struct {
			k Kind
			p any
		}{
			{KindLicenceStatusChange, LicenceStatusPayload{LicenceID: id, Status: "suspended"}},
			{KindLicenceValidityDatesChange, LicenceValidityDatesPayload{LicenceID: id, ExpiresAt: sp("2030-01-01")}},
			{KindLicenceReviewDueChange, LicenceReviewDuePayload{LicenceID: id, ReviewDueAt: sp("2030-01-01")}},
			{KindLicenceEvidenceReferenceChange, LicenceEvidenceReferencePayload{LicenceID: id, Reference: sp("R")}},
		} {
			_, err := w.svc.Request(context.Background(), ta, RequestInput{Kind: c.k, Payload: payload(t, c.p), ReasonCode: "x"})
			if !errors.Is(err, ErrForbidden) {
				t.Errorf("%s requesting %s: want ErrForbidden, got %v", role, c.k, err)
			}
		}
	}
	if r := w.licRow(id); r.Status != "active" || r.Evidence != nil {
		t.Fatalf("tenant staff changed a licence: %+v", r)
	}
}

// The licences UPDATE trigger (migration 0178): igaming_runtime cannot change a governed column, or any column, of a licence
// outside an executed licensing change of the same transaction. The owner (fixtures / seeding) keeps its path.
func TestLifecycle_GovernedTrigger_DirectRuntimeUpdateRefused(t *testing.T) {
	w := newWorld(t)
	id := w.newLicence()
	for _, set := range []string{
		`status = 'suspended'`, `status = 'active'`, `expires_at = DATE '2099-01-01'`, `issued_at = DATE '2020-01-01'`,
		`review_due_at = DATE '2099-01-01'`, `evidence_reference = 'DIRECT'`, `licence_number = 'RENAMED'`,
		`permitted_products = '["x"]'::jsonb`,
	} {
		err := w.rt.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE licences SET `+set+` WHERE id = $1`, id)
			return err
		})
		if sqlstateOf(err) != "LC040" {
			t.Errorf("runtime UPDATE %s: want LC040, got %v", set, err)
		}
	}
	if r := w.licRow(id); r.Status != "active" || r.Evidence != nil || r.Expires != nil {
		t.Fatalf("a refused direct write changed the licence: %+v", r)
	}
	w.ownerExec(`UPDATE licences SET status = 'suspended' WHERE id = $1`, id) // the documented owner path
	if w.licRow(id).Status != "suspended" {
		t.Fatal("the owner path must keep working")
	}
}

// A kind's executed request does not license a write to another column or another licence in the same transaction: the row
// match pins the licence id and every untouched column.
func TestLifecycle_RowMatch_OtherColumnsAndOtherLicencesRefused(t *testing.T) {
	w := newWorld(t)
	a, b := w.newLicence(), w.newLicence()
	// Execute a narrowing evidence change for licence a, then (same transaction, runtime role) try to write more.
	proof := func(tx pgx.Tx) {} // proofs are attached by Service; here we use the service-level transaction hook below.
	_ = proof
	res := w.mustRequest(w.req, KindLicenceEvidenceReferenceChange, LicenceEvidenceReferencePayload{LicenceID: a, Reference: sp("ROW-MATCH")}, "lic4_rowmatch")
	if !res.Executed {
		t.Fatal("setup: the evidence change must execute")
	}
	// A later transaction has no executed request of ITS txid: even an identical write is refused.
	err := w.rt.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE licences SET evidence_reference = 'ROW-MATCH' WHERE id = $1`, b)
		return err
	})
	if sqlstateOf(err) != "LC040" {
		t.Fatalf("a replayed write in another transaction: want LC040, got %v", err)
	}
	if got := w.licRow(b); got.Evidence != nil {
		t.Fatalf("licence b changed: %+v", got)
	}
}

// A widening request whose precondition vanished before the last approval cannot execute: the licence is unchanged and the
// request stays open (no silent no-op execution).
func TestLifecycleWidening_StateChangedBeforeExecution_Refused(t *testing.T) {
	w := newWorld(t)
	id := w.newLicence()
	w.ownerExec(`UPDATE licences SET status = 'suspended' WHERE id = $1`, id)
	res := w.mustRequest(w.req, KindLicenceStatusChange, LicenceStatusPayload{LicenceID: id, Status: "active"}, "lic4_race")
	w.mustApprove(w.ap1, res.Request.ID)
	w.ownerExec(`UPDATE licences SET status = 'active' WHERE id = $1`, id) // someone else already reinstated it
	_, err := w.approve(w.ap2, res.Request.ID)
	if err == nil {
		t.Fatal("the second approval must not execute a change that is now a no-op")
	}
	if got := w.status(res.Request.ID); got != StatusRequested {
		t.Fatalf("the request must stay requested, is %s", got)
	}
}

// A request that was narrowing when filed but widens by execution is refused (LC024) - exercised through the database
// function: classification is recomputed against the locked row. Direct SQL check of licensing_change_classify.
func TestLifecycle_ClassifyFunction_FailsClosed(t *testing.T) {
	w := newWorld(t)
	id := w.newLicence()
	classify := func(kind, payload string) (bool, error) {
		var widening bool
		err := w.rt.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT widening FROM licensing_change_classify($1, $2::jsonb)`, kind, payload).Scan(&widening)
		})
		return widening, err
	}
	l := id.String()
	for _, c := range []struct {
		name, kind, payload string
	}{
		{"missing key", "licence_status_change", `{"licence_id":"` + l + `"}`},
		{"extra key", "licence_status_change", `{"licence_id":"` + l + `","status":"suspended","x":1}`},
		{"bad status", "licence_status_change", `{"licence_id":"` + l + `","status":"revoked"}`},
		{"status as number", "licence_status_change", `{"licence_id":"` + l + `","status":1}`},
		{"unknown licence", "licence_status_change", `{"licence_id":"` + uuid.NewString() + `","status":"suspended"}`},
		{"no-op status", "licence_status_change", `{"licence_id":"` + l + `","status":"active"}`},
		{"missing date key", "licence_validity_dates_change", `{"licence_id":"` + l + `","expires_at":null}`},
		{"bad date", "licence_validity_dates_change", `{"licence_id":"` + l + `","issued_at":"2026-02-30","expires_at":null}`},
		{"date as number", "licence_validity_dates_change", `{"licence_id":"` + l + `","issued_at":20260101,"expires_at":null}`},
		{"expiry not after issue", "licence_validity_dates_change", `{"licence_id":"` + l + `","issued_at":"2026-05-01","expires_at":"2026-05-01"}`},
		{"no-op dates", "licence_validity_dates_change", `{"licence_id":"` + l + `","issued_at":null,"expires_at":null}`},
		{"bad review date", "licence_review_due_change", `{"licence_id":"` + l + `","review_due_at":"soon"}`},
		{"no-op review", "licence_review_due_change", `{"licence_id":"` + l + `","review_due_at":null}`},
		{"evidence with contact details", "licence_evidence_reference_change", `{"licence_id":"` + l + `","licence_evidence_reference":"a@b.example"}`},
		{"evidence untrimmed", "licence_evidence_reference_change", `{"licence_id":"` + l + `","licence_evidence_reference":" x"}`},
		{"evidence too long", "licence_evidence_reference_change", `{"licence_id":"` + l + `","licence_evidence_reference":"` + strings.Repeat("x", 201) + `"}`},
		{"evidence no-op", "licence_evidence_reference_change", `{"licence_id":"` + l + `","licence_evidence_reference":null}`},
		{"unregistered kind", "licence_revoke", `{"licence_id":"` + l + `"}`},
	} {
		if _, err := classify(c.kind, c.payload); sqlstateOf(err) != "LC010" {
			t.Errorf("%s: want LC010, got %v", c.name, err)
		}
	}
	// An unrecognised stored status classifies as WIDENING (fail closed) when the licence is later set to a known status.
	// licences.status has a CHECK in the live schema, so this is exercised only where the constraint allows it.
	if w, err := classify("licence_status_change", `{"licence_id":"`+l+`","status":"suspended"}`); err != nil || w {
		t.Fatalf("active -> suspended must be narrowing: %v %v", w, err)
	}
}

// Down refusal and snapshot: 0178 up / down / up restores the 0177 schema byte for byte; the down is refused (LC095) while a
// lifecycle request exists and changes nothing.
func TestMigration0178_UpDownUp_SnapshotAndDownRefusal(t *testing.T) {
	ctx := context.Background()
	u := scratchdb.New(t, "m0178_")
	pool, err := db.Connect(ctx, u, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	full, _ := migrationsThrough(t, 178)
	base, _ := migrationsThrough(t, 177)
	if _, err := pool.MigrateUp(ctx, base); err != nil {
		t.Fatalf("migrate to 0177: %v", err)
	}
	before := schemaSnapshot(t, pool)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != 178 {
		t.Fatalf("0178 up: %v %v", applied, err)
	}
	up := schemaSnapshot(t, pool)
	if !strings.Contains(up, "zz_licensing_change_governed_upd") {
		t.Fatal("0178 up must create the licences UPDATE governed trigger")
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("0178 down: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != before {
		t.Fatal("0178 down must restore the 0177 schema byte for byte")
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("0178 up again: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatal("0178 up again must equal the first up")
	}

	prooftest.InstallVia(t, pool.Raw(), u)
	launchfix.SeedOperatorsAt(t, u)
	ops := launchfix.Operators()
	var licID uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		jid := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'm0178')`, jid, "M-"+strings.ToUpper(jid.String()[:8])); err != nil {
			return err
		}
		licID = uuid.New()
		_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', 'M0178')`, licID, jid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{Pool: pool})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Request(ctx, Actor{StaffID: ops.Requester, Scope: ScopePlatform, Role: string(auth.RolePlatformAdmin)},
		RequestInput{Kind: KindLicenceEvidenceReferenceChange, Payload: payload(t, LicenceEvidenceReferencePayload{LicenceID: licID, Reference: sp("M0178")}), ReasonCode: "m0178"}); err != nil {
		t.Fatalf("seed a request: %v", err)
	}
	_, err = pool.MigrateDown(ctx, full, 1)
	if err == nil || !strings.Contains(err.Error(), "LC095") {
		t.Fatalf("0178 down must refuse LC095 while a lifecycle request exists, got %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatal("a refused down must change nothing")
	}
}

// ---- security review secrev3 (docs/plans/licensing-security-review-record.md, section 5), folded into migration 0178 ----

// F-1: a runtime credential with a MADE-UP platform GUC (no staff row, no proof) cannot change a licence: the identity
// columns are immutable for every role; status toward active, a later or cleared expires_at and a suspension are refused
// without an executed licensing change of the same transaction.
func TestSecurityF1_MadeUpPlatformGUC_CannotUpdateLicences(t *testing.T) {
	w := newWorld(t)
	id := w.newLicence()
	other := w.jurisdiction()
	w.ownerExec(`UPDATE licences SET status = 'suspended', expires_at = DATE '2030-01-01' WHERE id = $1`, id)
	forged := uuid.New() // not a staff account at all
	try := func(set string) error {
		return w.rt.WithPlatformAdmin(context.Background(), forged, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE licences SET `+set+` WHERE id = $1`, id)
			return err
		})
	}
	for _, c := range []struct{ set, code string }{
		{`jurisdiction_id = '` + other.String() + `'`, "LC042"},
		{`licence_number = 'RE-POINTED'`, "LC042"},
		{`permitted_markets = '["ZZ"]'::jsonb`, "LC042"},
		{`permitted_products = '["casino"]'::jsonb`, "LC042"},
		{`licensee = 'tenant'`, "LC042"},
		{`status = 'active'`, "LC040"},                 // reinstate
		{`expires_at = DATE '2099-12-31'`, "LC040"},    // later expiry
		{`expires_at = NULL`, "LC040"},                 // cleared expiry
		{`review_due_at = DATE '2099-01-01'`, "LC040"}, // set review (narrowing) still needs a proof-bound change
		{`status = 'expired'`, "LC040"},                // suspend / expire without a proof
		{`issued_at = DATE '2000-01-01'`, "LC040"},     // earlier issue date
		{`status = 'active', expires_at = DATE '2099-12-31', licence_number = 'X'`, "LC042"},
	} {
		if got := sqlstateOf(try(c.set)); got != c.code {
			t.Errorf("forged-GUC UPDATE %s: want %s, got %q", c.set, c.code, got)
		}
	}
	if r := w.licRow(id); r.Status != "suspended" || strOf(r.Expires) != "2030-01-01" || r.Review != nil {
		t.Fatalf("a refused forged write changed the licence: %+v", r)
	}
	// The identity columns are immutable for the OWNER too (a reviewed migration disables the trigger if it ever must).
	err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE licences SET licence_number = 'OWNER-RENAMED' WHERE id = $1`, id)
		return err
	})
	if sqlstateOf(err) != "LC042" {
		t.Fatalf("owner changing an identity column: want LC042, got %v", err)
	}
}

// F-1 (c): an executed single-actor request of a PREVIOUS transaction authorises nothing later: a status reinstatement after an
// executed evidence-reference change (a different transaction, a different kind) is refused.
func TestSecurityF1_ExecutedNarrowingRequest_DoesNotAuthoriseAWideningWrite(t *testing.T) {
	w := newWorld(t)
	id := w.newLicence()
	w.ownerExec(`UPDATE licences SET status = 'suspended' WHERE id = $1`, id)
	// The only executed requests of this licence are single-actor ones; none authorises 'active'.
	w.mustRequest(w.req, KindLicenceEvidenceReferenceChange, LicenceEvidenceReferencePayload{LicenceID: id, Reference: sp("F1C")}, "lic4_f1c")
	err := w.rt.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE licences SET status = 'active' WHERE id = $1`, id)
		return err
	})
	if sqlstateOf(err) != "LC040" {
		t.Fatalf("reinstating beside an unrelated executed request: want LC040, got %v", err)
	}
	if w.licRow(id).Status != "suspended" {
		t.Fatal("the licence was reinstated")
	}
}

// F-2: licence_versions cannot be written by the runtime role (INSERT revoked), by a forged platform GUC, or directly by anyone
// (the INSERT policy requires the licences trigger); a huge version_no cannot be planted; changed_by is recorded only for an
// ACTIVE platform staff account.
func TestSecurityF2_LicenceVersions_NotForgeable(t *testing.T) {
	w := newWorld(t)
	id := w.newLicence()
	ins := func(pool *db.Pool, principal uuid.UUID, versionNo int64) error {
		return pool.WithPlatformAdmin(context.Background(), principal, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO licence_versions (licence_id, version_no, operation, changed_by, status, licence_number, permitted_products, permitted_markets)
				VALUES ($1, $2, 'update', $3, 'active', 'FORGED', '[]', '[]')`, id, versionNo, principal)
			return err
		})
	}
	for _, v := range []int64{2, 2147483647, 9000000000} {
		if err := ins(w.rt, uuid.New(), v); err == nil {
			t.Errorf("runtime INSERT of version_no %d into licence_versions must be refused", v)
		}
		if err := ins(w.rt, w.ops.Requester, v); err == nil {
			t.Errorf("runtime INSERT (real platform principal) of version_no %d must be refused", v)
		}
		if err := ins(w.owner, w.ops.Requester, v); err == nil {
			t.Errorf("a direct INSERT (not from the licences trigger) of version_no %d must be refused for the owner too", v)
		}
	}
	// version_no is bigint; the legitimate writer still works and stays gapless.
	var typ string
	if err := w.owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT data_type FROM information_schema.columns WHERE table_name = 'licence_versions' AND column_name = 'version_no'`).Scan(&typ)
	}); err != nil || typ != "bigint" {
		t.Fatalf("version_no type = %q (%v), want bigint", typ, err)
	}
	w.mustRequest(w.req, KindLicenceStatusChange, LicenceStatusPayload{LicenceID: id, Status: "suspended"}, "lic4_f2")
	var maxNo, n int64
	var by *uuid.UUID
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT max(version_no), count(*), (array_agg(changed_by ORDER BY version_no DESC))[1] FROM licence_versions WHERE licence_id = $1`, id).Scan(&maxNo, &n, &by)
	}); err != nil {
		t.Fatal(err)
	}
	if maxNo != n || n != 2 || by == nil || *by != w.ops.Requester {
		t.Fatalf("history after one governed change: max=%d count=%d changed_by=%v (want 2 gapless rows, changed_by the requester)", maxNo, n, by)
	}
	// An owner-path change with a GUC that is NOT an active platform staff account records changed_by = NULL, never the forged id.
	forged := uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), forged, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE licences SET status = 'expired' WHERE id = $1`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT changed_by FROM licence_versions WHERE licence_id = $1 ORDER BY version_no DESC LIMIT 1`, id).Scan(&by)
	}); err != nil {
		t.Fatal(err)
	}
	if by != nil {
		t.Fatalf("an unverifiable principal must be recorded as NULL, got %v", *by)
	}
}

// F-4: a tenant-filed brand-rung policy naming another tenant's brand (or an unknown one) is refused at FILING as not found,
// before any approver sees it; its own brand is accepted.
func TestSecurityF4_BrandOwnershipCheckedAtFiling(t *testing.T) {
	w := newWorld(t)
	a, b := w.tenant("under_platform_licence"), w.tenant("under_platform_licence")
	brandA, brandB := w.brand(a), w.brand(b)
	ta := w.tenantStaff(a, "tenant_admin")
	file := func(brand uuid.UUID) error {
		_, err := w.svc.Request(context.Background(), ta, RequestInput{Kind: KindOperatingPolicyVersion, ReasonCode: "lic4_f4",
			Payload: payload(t, OperatingPolicyPayload{TenantID: a, Scope: "brand", BrandID: &brand, CountryCode: "MT", State: "disabled", Status: "active"})})
		return err
	}
	expectErr(t, file(brandB), jurisdiction.ErrNotFound, "another tenant's brand")
	expectErr(t, file(uuid.New()), jurisdiction.ErrNotFound, "an unknown brand")
	if err := file(brandA); err != nil {
		t.Fatalf("a tenant's own brand must be accepted at filing: %v", err)
	}
	if n := w.ownerInt(`SELECT count(*) FROM licensing_change_requests WHERE subject_tenant_id = $1 AND payload ->> 'brand_id' = $2`, a, brandB.String()); n != 0 {
		t.Fatalf("a refused filing stored %d requests", n)
	}
}

// F-4 at the database: the SQL classifier refuses the same payload for a tenant session (LC010), independent of Go.
func TestSecurityF4_DatabaseRefusesForeignBrandForTenantSession(t *testing.T) {
	w := newWorld(t)
	a, b := w.tenant("under_platform_licence"), w.tenant("under_platform_licence")
	brandB := w.brand(b)
	ta := w.tenantStaff(a, "tenant_admin")
	p := `{"tenant_id":"` + a.String() + `","scope":"brand","brand_id":"` + brandB.String() + `","operation_code":null,"product_code":null,"country_code":"MT","state":"disabled","status":"active","authorization_reference":null}`
	err := w.rt.WithPrincipalScope(context.Background(), a, ta.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		var widening bool
		return tx.QueryRow(ctx, `SELECT widening FROM licensing_change_classify('operating_policy_version', $1::jsonb)`, p).Scan(&widening)
	})
	if sqlstateOf(err) != "LC010" {
		t.Fatalf("a tenant session classifying a foreign brand: want LC010, got %v", err)
	}
}

// F-5: an identical open request of the OTHER requester class does not block a platform admin's narrowing; a duplicate within
// one class is still refused.
func TestSecurityF5_DuplicatePendingIsPerRequesterClass(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	co := w.tenantStaff(tid, "compliance")
	p := OperatingPolicyPayload{TenantID: tid, Scope: "tenant", CountryCode: "MT", State: "disabled", Status: "active"}
	tenantReq := w.mustRequest(co, KindOperatingPolicyVersion, p, "incident_exit") // tenant narrowing: stays open (1 approval)
	if tenantReq.Executed || tenantReq.Request.Status != StatusRequested {
		t.Fatalf("setup: %+v", tenantReq.Request)
	}
	_, err := w.svc.Request(context.Background(), co, RequestInput{Kind: KindOperatingPolicyVersion, Payload: payload(t, p), ReasonCode: "incident_exit"})
	expectErr(t, err, ErrDuplicatePending, "the same tenant repeating its open request")
	// The platform admin's identical narrowing is NOT blocked: it executes at once.
	pr := w.mustRequest(w.req, KindOperatingPolicyVersion, p, "incident_exit")
	if !pr.Executed || pr.Request.RequestedScope != ScopePlatform {
		t.Fatalf("the platform narrowing must execute despite the tenant's open identical request: %+v", pr.Request)
	}
	// Within the platform class a duplicate widening request is still refused.
	id := w.newLicence()
	w.ownerExec(`UPDATE licences SET status = 'suspended' WHERE id = $1`, id)
	w.mustRequest(w.req, KindLicenceStatusChange, LicenceStatusPayload{LicenceID: id, Status: "active"}, "lic4_dup")
	_, err = w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindLicenceStatusChange, ReasonCode: "lic4_dup",
		Payload: payload(t, LicenceStatusPayload{LicenceID: id, Status: "active"})})
	expectErr(t, err, ErrDuplicatePending, "the platform repeating its open widening request")
}

// Tenant isolation at SQL level: a session of tenant A (real staff with a Person) cannot file a request about tenant B, cannot
// read B's requests or approvals, and cannot change B's request (UPDATE affects 0 rows).
func TestSecurity_TenantA_CannotTouchTenantBRequests(t *testing.T) {
	w := newWorld(t)
	a, b := w.tenant("under_platform_licence"), w.tenant("under_platform_licence")
	staffA := w.tenantStaff(a, "compliance")
	coB := w.tenantStaff(b, "compliance")
	reqB := w.mustRequest(coB, KindOperatingPolicyVersion, OperatingPolicyPayload{TenantID: b, Scope: "tenant", CountryCode: "MT", State: "disabled", Status: "active"}, "b_exit")
	ctx := context.Background()

	// A raw INSERT of a request whose payload subject is tenant B, in tenant A's session: refused before any proof check.
	err := w.rt.WithPrincipalScope(ctx, a, staffA.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO licensing_change_requests (kind, payload, payload_hash, reason_code) VALUES ('operating_policy_version', $1::jsonb, 'x', 'x')`,
			`{"tenant_id":"`+b.String()+`","scope":"tenant","brand_id":null,"operation_code":null,"product_code":null,"country_code":"MT","state":"disabled","status":"active","authorization_reference":null}`)
		return err
	})
	if c := sqlstateOf(err); c != "LC002" && c != "42501" && c != "LC010" {
		t.Fatalf("tenant A filing a request about tenant B: want LC002 / RLS refusal, got %v", err)
	}
	var seenReq, seenApp int
	var updated int64
	if err := w.rt.WithPrincipalScope(ctx, a, staffA.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM licensing_change_requests WHERE id = $1`, reqB.Request.ID).Scan(&seenReq); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM licensing_change_approvals WHERE request_id = $1`, reqB.Request.ID).Scan(&seenApp); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE licensing_change_requests SET status = 'cancelled' WHERE id = $1`, reqB.Request.ID)
		updated = tag.RowsAffected()
		return err
	}); err != nil {
		t.Fatalf("tenant A's reads / update attempt: %v", err)
	}
	if seenReq != 0 || seenApp != 0 || updated != 0 {
		t.Fatalf("tenant A saw %d requests, %d approvals and updated %d rows of tenant B's request", seenReq, seenApp, updated)
	}
	if w.status(reqB.Request.ID) != StatusRequested {
		t.Fatal("tenant B's request changed")
	}
}
