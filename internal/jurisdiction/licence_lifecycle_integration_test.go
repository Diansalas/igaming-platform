//go:build integration

package jurisdiction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
)

func lcActor(reason string) ActorContext {
	return ActorContext{ActorID: uuid.New(), IPAddress: "127.0.0.1", UserAgent: "test", RequestID: "req-" + uuid.NewString()[:8], ReasonCode: reason}
}

// newLifecycleLicence inserts a fresh platform licence through the real CreateLicence.
func newLifecycleLicence(t *testing.T, pool *db.Pool, f fixture) Licence {
	t.Helper()
	var l Licence
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		l, err = CreateLicence(ctx, tx, CreateLicenceParams{JurisdictionID: f.jurisdictionID, Licensee: "platform",
			LicenceNumber: "LC-" + uuid.NewString()[:8], Actor: lcActor("test_seed")})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func lcDo(t *testing.T, pool *db.Pool, f fixture, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	return pool.WithPlatformAdmin(context.Background(), f.platformAdmin, fn)
}

func lcVersions(t *testing.T, pool *db.Pool, f fixture, id uuid.UUID) []LicenceVersion {
	t.Helper()
	var vs []LicenceVersion
	if err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		vs, err = ListLicenceVersions(ctx, tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return vs
}

func lcAudit(t *testing.T, pool *db.Pool, action string, id uuid.UUID) []map[string]any {
	t.Helper()
	var out []map[string]any
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT metadata FROM audit_log WHERE tenant_id IS NULL AND action = $1 AND target_id = $2 ORDER BY created_at`, action, id.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				return err
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLicenceLifecycle_StatusChange_AuditedVersionedAndClassified(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	l := newLifecycleLicence(t, pool, f)

	var res LicenceChange
	if err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = SetLicenceStatus(ctx, tx, l.ID, "suspended", lcActor("regulator_suspension"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if res.Kind != ChangeNarrowing || res.Licence.Status != "suspended" {
		t.Fatalf("got %+v", res)
	}

	a := lcAudit(t, pool, "jurisdiction_registry.licence_status_set", l.ID)
	if len(a) != 1 || a[0]["reason_code"] != "regulator_suspension" || a[0]["change_kind"] != "narrowing" {
		t.Fatalf("audit = %v", a)
	}
	before, after := a[0]["before"].(map[string]any), a[0]["after"].(map[string]any)
	if before["status"] != "active" || after["status"] != "suspended" {
		t.Fatalf("before/after = %v / %v", before, after)
	}

	vs := lcVersions(t, pool, f, l.ID)
	if len(vs) != 2 || vs[0].Operation != "insert" || vs[1].Operation != "update" || vs[1].Status != "suspended" || vs[1].ChangedBy == nil || *vs[1].ChangedBy != f.platformAdmin {
		t.Fatalf("versions = %+v", vs)
	}

	// Reinstating is WIDENING (the class slice 2 routes through an approval).
	if err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = SetLicenceStatus(ctx, tx, l.ID, "active", lcActor("regulator_reinstated"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if res.Kind != ChangeWidening {
		t.Fatalf("reinstate kind = %q", res.Kind)
	}

	// No-op and out-of-vocabulary values are refused and write nothing.
	for _, st := range []string{"active", "revoked"} {
		err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
			_, err := SetLicenceStatus(ctx, tx, l.ID, st, lcActor("x"))
			return err
		})
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("status %q: want ErrInvalidInput, got %v", st, err)
		}
	}
	if n := len(lcVersions(t, pool, f, l.ID)); n != 3 {
		t.Fatalf("a refused change must not add a version; have %d", n)
	}
	// A missing reason code is refused.
	err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetLicenceStatus(ctx, tx, l.ID, "suspended", lcActor("  "))
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing reason: %v", err)
	}
}

func TestLicenceLifecycle_DatesReviewAndEvidence_VersionedEachTime(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	l := newLifecycleLicence(t, pool, f)
	run := func(fn func(ctx context.Context, tx pgx.Tx) (LicenceChange, error)) (LicenceChange, error) {
		var res LicenceChange
		err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = fn(ctx, tx)
			return err
		})
		return res, err
	}
	res, err := run(func(ctx context.Context, tx pgx.Tx) (LicenceChange, error) {
		return SetLicenceValidityDates(ctx, tx, l.ID, d("2026-01-01"), d("2027-01-01"), lcActor("licence_issued"))
	})
	if err != nil || res.Kind != ChangeNarrowing || dateString(res.Licence.ExpiresAt) != "2027-01-01" {
		t.Fatalf("dates: %+v %v", res, err)
	}
	if _, err = run(func(ctx context.Context, tx pgx.Tx) (LicenceChange, error) {
		return SetLicenceValidityDates(ctx, tx, l.ID, d("2026-01-01"), d("2026-01-01"), lcActor("bad"))
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expires<=issued must be refused: %v", err)
	}
	res, err = run(func(ctx context.Context, tx pgx.Tx) (LicenceChange, error) {
		return SetLicenceValidityDates(ctx, tx, l.ID, d("2026-01-01"), d("2028-01-01"), lcActor("licence_renewed"))
	})
	if err != nil || res.Kind != ChangeWidening {
		t.Fatalf("extend: %+v %v", res, err)
	}
	if res, err = run(func(ctx context.Context, tx pgx.Tx) (LicenceChange, error) {
		return SetLicenceReviewDue(ctx, tx, l.ID, d("2027-06-30"), lcActor("review_scheduled"))
	}); err != nil || dateString(res.Licence.ReviewDueAt) != "2027-06-30" {
		t.Fatalf("review: %+v %v", res, err)
	}
	ref := "REG-LETTER-2026-0042"
	if res, err = run(func(ctx context.Context, tx pgx.Tx) (LicenceChange, error) {
		return SetLicenceEvidenceReference(ctx, tx, l.ID, &ref, lcActor("evidence_recorded"))
	}); err != nil || res.Licence.EvidenceReference != ref || res.Kind != ChangeNeutral {
		t.Fatalf("evidence: %+v %v", res, err)
	}
	bad := "mail me@example.com"
	if _, err = run(func(ctx context.Context, tx pgx.Tx) (LicenceChange, error) {
		return SetLicenceEvidenceReference(ctx, tx, l.ID, &bad, lcActor("x"))
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("contact details must be refused: %v", err)
	}

	vs := lcVersions(t, pool, f, l.ID)
	if len(vs) != 5 { // insert + 4 successful changes
		t.Fatalf("want 5 versions, got %d: %+v", len(vs), vs)
	}
	last := vs[len(vs)-1]
	if last.EvidenceReference != ref || dateString(last.ReviewDueAt) != "2027-06-30" || dateString(last.ExpiresAt) != "2028-01-01" {
		t.Fatalf("last version = %+v", last)
	}
	for i, v := range vs {
		if v.VersionNo != i+1 {
			t.Fatalf("version numbers must be gapless: %+v", vs)
		}
	}
	for _, action := range []string{"licence_validity_dates_set", "licence_review_due_set", "licence_evidence_reference_set"} {
		if len(lcAudit(t, pool, "jurisdiction_registry."+action, l.ID)) == 0 {
			t.Fatalf("no audit row for %s", action)
		}
	}
	// The evidence value is stored; clearing is a separate audited change.
	if res, err = run(func(ctx context.Context, tx pgx.Tx) (LicenceChange, error) {
		return SetLicenceEvidenceReference(ctx, tx, l.ID, nil, lcActor("evidence_withdrawn"))
	}); err != nil || res.Licence.EvidenceReference != "" {
		t.Fatalf("clear: %+v %v", res, err)
	}
}

func TestLicenceLifecycle_RequiresPlatformScope_AndRLSBackstop(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	l := newLifecycleLicence(t, pool, f)

	// Service layer: a tenant-scoped (and a scopeless) transaction is refused before any SQL.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetLicenceStatus(ctx, tx, l.ID, "suspended", lcActor("x"))
		return err
	})
	if err == nil {
		t.Fatal("tenant-scoped SetLicenceStatus must be refused")
	}
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetLicenceEvidenceReference(ctx, tx, l.ID, strPtr("x"), lcActor("x"))
		return err
	})
	if err == nil {
		t.Fatal("scopeless SetLicenceEvidenceReference must be refused")
	}

	// Database layer (independent of Go): a tenant-scoped raw UPDATE changes nothing.
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE licences SET status = 'suspended', evidence_reference = 'x' WHERE id = $1`, f.licenceID)
		if err == nil && tag.RowsAffected() != 0 {
			t.Errorf("tenant-scoped raw UPDATE changed %d rows", tag.RowsAffected())
		}
		return nil
	})
	if vs := lcVersions(t, pool, f, f.licenceID); len(vs) > 0 && vs[len(vs)-1].Status != "active" {
		t.Fatalf("licence changed by a tenant session: %+v", vs)
	}
	// A tenant session cannot read the history either.
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM licence_versions`).Scan(&n); err != nil {
			t.Errorf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("tenant session sees %d licence_versions rows", n)
		}
		return nil
	})
}

func TestLicenceVersions_AppendOnly_EvenForPlatformAdmin(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	l := newLifecycleLicence(t, pool, f)
	// UPDATE and DELETE: there is no policy for either, so even a platform admin changes ZERO rows (a deny trigger is
	// the second layer); TRUNCATE (not subject to RLS) is refused by its deny trigger.
	for _, stmt := range []string{
		`UPDATE licence_versions SET status = 'expired' WHERE licence_id = $1`,
		`DELETE FROM licence_versions WHERE licence_id = $1`,
	} {
		var affected int64
		err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, stmt, l.ID)
			affected = tag.RowsAffected()
			return err
		})
		if err == nil && affected != 0 {
			t.Fatalf("%q changed %d rows", stmt, affected)
		}
	}
	err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE licence_versions`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("TRUNCATE must be refused as append-only, got %v", err)
	}
	vs := lcVersions(t, pool, f, l.ID)
	if len(vs) != 1 || vs[0].Status != "active" {
		t.Fatalf("history must be intact: %+v", vs)
	}
}

func TestLicenceVersions_RawChangeBySQLIsStillVersioned(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	l := newLifecycleLicence(t, pool, f)
	if err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE licences SET status = 'expired', expires_at = DATE '2020-01-01' WHERE id = $1`, l.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	vs := lcVersions(t, pool, f, l.ID)
	if len(vs) != 2 || vs[1].Status != "expired" || dateString(vs[1].ExpiresAt) != "2020-01-01" {
		t.Fatalf("versions = %+v", vs)
	}
	// An UPDATE that changes no tracked column writes no version.
	if err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE licences SET status = status WHERE id = $1`, l.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(lcVersions(t, pool, f, l.ID)); n != 2 {
		t.Fatalf("no-op update wrote a version: %d", n)
	}
}

// --- sweep ---

type raised struct {
	kind  alerting.Kind
	id    uuid.UUID
	attrs map[string]alerting.AttrValue
}

func captureRaises(t *testing.T) *[]raised {
	t.Helper()
	var got []raised
	old := raiseLicenceAlert
	t.Cleanup(func() { raiseLicenceAlert = old })
	raiseLicenceAlert = func(_ context.Context, _ *db.Pool, kind alerting.Kind, id uuid.UUID, attrs map[string]alerting.AttrValue) error {
		got = append(got, raised{kind, id, attrs})
		return nil
	}
	return &got
}

// sweepTenant creates a tenant bound to a fresh platform licence with one brand and returns the ids.
func sweepTenant(t *testing.T, pool *db.Pool, f fixture, l Licence) (tenantID, brandID uuid.UUID) {
	t.Helper()
	tenantID, brandID = uuid.New(), uuid.New()
	if err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model, status, licence_id) VALUES ($1, $2, 'Sweep', 'under_platform_licence', 'active', $3)`,
			tenantID, "sw-"+tenantID.String()[:8], l.ID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, name, slug) VALUES ($1, $2, 'B', $3)`, brandID, tenantID, "b-"+brandID.String()[:8])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return tenantID, brandID
}

func TestLicenceSweep_RaisesExpiredAndExpiring_WithAffectedIds_AndChangesNothing(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	expired := newLifecycleLicence(t, pool, f)
	expiring := newLifecycleLicence(t, pool, f)
	healthy := newLifecycleLicence(t, pool, f)
	unbound := newLifecycleLicence(t, pool, f)
	set := func(id uuid.UUID, sql string, args ...any) {
		if err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, append([]any{id}, args...)...)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	set(expired.ID, `UPDATE licences SET expires_at = DATE '2026-10-19' WHERE id = $1`) // boundary: today
	set(expiring.ID, `UPDATE licences SET review_due_at = DATE '2026-11-10' WHERE id = $1`)
	set(healthy.ID, `UPDATE licences SET expires_at = DATE '2028-01-01', review_due_at = DATE '2027-06-01' WHERE id = $1`)
	set(unbound.ID, `UPDATE licences SET status = 'expired' WHERE id = $1`)

	tExp1, bExp1 := sweepTenant(t, pool, f, expired)
	tExp2, _ := sweepTenant(t, pool, f, expired) // a licence shared by two tenants -> ONE alert
	tSoon, bSoon := sweepTenant(t, pool, f, expiring)
	sweepTenant(t, pool, f, healthy)

	got := captureRaises(t)
	res, err := RunLicenceSweepOnce(context.Background(), pool, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.TenantErrors != 0 || res.RaiseErrors != 0 {
		t.Fatalf("result %+v", res)
	}
	byID := map[uuid.UUID]raised{}
	for _, r := range *got {
		if _, dup := byID[r.id]; dup {
			t.Fatalf("licence %s raised twice in one pass", r.id)
		}
		byID[r.id] = r
	}
	e, ok := byID[expired.ID]
	if !ok || e.kind != alerting.KindLicenceExpired || e.attrs["trigger"] != TriggerExpiresAtReached || e.attrs["tenant_count"] != 2 {
		t.Fatalf("expired alert = %+v", e)
	}
	ids := e.attrs["tenant_ids"].(string)
	if !strings.Contains(ids, tExp1.String()) || !strings.Contains(ids, tExp2.String()) || !strings.Contains(e.attrs["brand_ids"].(string), bExp1.String()) {
		t.Fatalf("affected ids missing: %v", e.attrs)
	}
	s, ok := byID[expiring.ID]
	if !ok || s.kind != alerting.KindLicenceExpiring || s.attrs["trigger"] != TriggerReviewDue ||
		s.attrs["tenant_ids"] != tSoon.String() || s.attrs["brand_ids"] != bSoon.String() {
		t.Fatalf("expiring alert = %+v", s)
	}
	if _, ok := byID[healthy.ID]; ok {
		t.Fatal("a healthy licence must not alert")
	}
	if _, ok := byID[unbound.ID]; ok {
		t.Fatal("a licence bound to no tenant affects nobody and is not raised")
	}

	// Read-only: no tenant, brand or licence status changed.
	var tenantStatus, brandStatus, licStatus string
	_ = lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
		_ = tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tExp1).Scan(&tenantStatus)
		_ = tx.QueryRow(ctx, `SELECT status FROM licences WHERE id = $1`, expired.ID).Scan(&licStatus)
		return nil
	})
	_ = pool.WithTenant(context.Background(), tExp1, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1`, bExp1).Scan(&brandStatus)
	})
	if tenantStatus != "active" || licStatus != "active" || brandStatus == "suspended" || brandStatus == "closed" {
		t.Fatalf("the sweep must change nothing: tenant=%s licence=%s brand=%s", tenantStatus, licStatus, brandStatus)
	}
}

// The sweep end to end with the REAL alert write (alert_dispatcher identity): one open P1 alert per licence, the
// affected tenant and brand ids in its attributes, and a second pass attaches an occurrence instead of a second alert.
func TestLicenceSweep_RealAlertWrite_DedupsPerLicenceAcrossPasses(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	l := newLifecycleLicence(t, pool, f)
	if err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetLicenceStatus(ctx, tx, l.ID, "expired", lcActor("regulator_revocation"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tenantID, brandID := sweepTenant(t, pool, f, l)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		res, err := RunLicenceSweepOnce(context.Background(), pool, nil, now)
		if err != nil || res.RaiseErrors != 0 {
			t.Fatalf("pass %d: %+v %v", i, res, err)
		}
	}
	var n int
	var kind, sev string
	var attrs []byte
	if err := lcDo(t, pool, f, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE discriminator = $1`, "licence:"+l.ID.String()).Scan(&n); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT kind, severity, attributes FROM alerts WHERE discriminator = $1`, "licence:"+l.ID.String()).Scan(&kind, &sev, &attrs)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 || kind != "licence.expired" || sev != "p1" {
		t.Fatalf("alerts=%d kind=%s severity=%s", n, kind, sev)
	}
	var a map[string]any
	if err := json.Unmarshal(attrs, &a); err != nil {
		t.Fatal(err)
	}
	if a["trigger"] != TriggerStatusExpired || a["tenant_ids"] != tenantID.String() || a["brand_ids"] != brandID.String() || a["licence_id"] != l.ID.String() {
		t.Fatalf("attributes = %v", a)
	}
	for k := range a {
		switch k {
		case "licence_id", "trigger", "expires_at", "review_due_at", "tenant_count", "brand_count", "tenant_ids", "brand_ids", "truncated":
		default:
			t.Fatalf("unexpected attribute %q", k)
		}
	}
}
