//go:build integration

// Migration 0179 integration tests (lic6): licence_versions attribution from the authorising request (L-1), the "applied in the
// wrong order" widening (L-2a), INSERT ... ON CONFLICT (L-2b), guard trigger ordering (I-1), no DELETE on licences (I-2), the
// licence_create match at creation (I-3), read-only read views (I-5), and the migration round trip.
package licensingchange

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// rawFileAndExecute files a single-actor (narrowing / neutral) licensing change by staff and executes it in the SAME transaction,
// with real signed proofs, entirely in SQL (no Service). It returns the request id. The caller applies the row write itself.
func rawFileAndExecute(t *testing.T, ctx context.Context, tx pgx.Tx, forger prooftest.Forger, staff uuid.UUID, kind, body, reason string) uuid.UUID {
	t.Helper()
	var hash string
	if err := tx.QueryRow(ctx, `SELECT licensing_change_payload_hash($1, $2::jsonb, $3)`, kind, body, reason).Scan(&hash); err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := setProof(ctx, tx, forger.Valid(t, staff, "platform", uuid.Nil, actorproof.OpLicensingChangeRequest, "new", hash)); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO licensing_change_requests (kind, payload, payload_hash, reason_code) VALUES ($1, $2::jsonb, $3, $4) RETURNING id`,
		kind, body, hash, reason).Scan(&id); err != nil {
		t.Fatalf("file %s: %v", kind, err)
	}
	if err := setProof(ctx, tx, forger.Valid(t, staff, "platform", uuid.Nil, actorproof.OpLicensingChangeExecute, id.String(), hash)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE licensing_change_requests SET status = 'executed' WHERE id = $1`, id); err != nil {
		t.Fatalf("execute %s: %v", kind, err)
	}
	return id
}

func (w *world) lastVersion(id uuid.UUID) (changedBy, requestID *uuid.UUID, n int) {
	w.t.Helper()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM licence_versions WHERE licence_id = $1`, id).Scan(&n); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT changed_by, change_request_id FROM licence_versions WHERE licence_id = $1 ORDER BY version_no DESC LIMIT 1`, id).Scan(&changedBy, &requestID)
	}); err != nil {
		w.t.Fatal(err)
	}
	return
}

// L-1: changed_by / change_request_id come from the executed request that authorised the write, whatever the session GUC says.
func TestHardeningL1_VersionAttributionFromTheAuthorisingRequest(t *testing.T) {
	w := newWorld(t)
	good := prooftest.ProcessForger(t, w.ownerURL)
	id := w.newLicence()
	// Executed as staff A (the requester); the session GUC is then switched to staff B before the row write.
	var reqID uuid.UUID
	err := w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		reqID = rawFileAndExecute(t, ctx, tx, good, w.ops.Requester, "licence_evidence_reference_change",
			`{"licence_id":"`+id.String()+`","licence_evidence_reference":"L1-REF"}`, "lic6_l1")
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, w.ops.Approver1.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE licences SET evidence_reference = 'L1-REF' WHERE id = $1`, id)
		return err
	})
	mustNotErr(t, err, "execute as A, write with the GUC of B")
	by, rq, n := w.lastVersion(id)
	if n != 2 || by == nil || *by != w.ops.Requester || rq == nil || *rq != reqID {
		t.Fatalf("version attribution: changed_by=%v change_request_id=%v (n=%d), want %s / %s", by, rq, n, w.ops.Requester, reqID)
	}
	// Through the service the same: the executor and the request.
	res := w.mustRequest(w.req, KindLicenceStatusChange, LicenceStatusPayload{LicenceID: id, Status: "suspended"}, "lic6_l1b")
	by, rq, _ = w.lastVersion(id)
	if by == nil || *by != w.req.StaffID || rq == nil || *rq != res.Request.ID {
		t.Fatalf("service path attribution: %v / %v, want %s / %s", by, rq, w.req.StaffID, res.Request.ID)
	}
	// Owner path (no request): NULL / NULL even with a platform GUC naming a real staff account.
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE licences SET status = 'expired' WHERE id = $1`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if by, rq, _ = w.lastVersion(id); by != nil || rq != nil {
		t.Fatalf("an owner-path write has no request: %v / %v", by, rq)
	}
}

// L-2a: two narrowing date changes filed and executed in one transaction, applied A then B, where B (filed against the original
// row) now WIDENS the row: refused (LC040), because B is not a widening request.
func TestHardeningL2a_NarrowingRequestsAppliedInTheWrongOrder_Refused(t *testing.T) {
	w := newWorld(t)
	good := prooftest.ProcessForger(t, w.ownerURL)
	id := w.newLicence()
	w.ownerExec(`UPDATE licences SET expires_at = DATE '2031-01-01' WHERE id = $1`, id)
	err := w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		a := `{"licence_id":"` + id.String() + `","issued_at":null,"expires_at":"2029-01-01"}`
		b := `{"licence_id":"` + id.String() + `","issued_at":null,"expires_at":"2030-01-01"}`
		rawFileAndExecute(t, ctx, tx, good, w.ops.Requester, "licence_validity_dates_change", a, "lic6_l2a_a")
		rawFileAndExecute(t, ctx, tx, good, w.ops.Requester, "licence_validity_dates_change", b, "lic6_l2a_b")
		if _, err := tx.Exec(ctx, `UPDATE licences SET expires_at = DATE '2029-01-01' WHERE id = $1`, id); err != nil {
			t.Errorf("applying A (2031 -> 2029, narrowing) must pass: %v", err)
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE licences SET expires_at = DATE '2030-01-01' WHERE id = $1`, id) // 2029 -> 2030 widens
		return err
	})
	if pgCode(err) != "LC040" {
		t.Fatalf("applying B after A (a widening row change carried by a narrowing request) must be LC040, got %v", err)
	}
	if r := w.licRow(id); strOf(r.Expires) != "2031-01-01" {
		t.Fatalf("the refused transaction must change nothing: %+v", r)
	}
}

// L-2b: INSERT ... ON CONFLICT (id) DO UPDATE cannot be used to rewrite a licence: the BEFORE INSERT guard fires first.
func TestHardeningL2b_InsertOnConflictDoUpdate_Refused(t *testing.T) {
	w := newWorld(t)
	jid := w.jurisdiction()
	id := w.licence(jid, "platform", uuid.Nil)
	w.ownerExec(`UPDATE licences SET status = 'suspended' WHERE id = $1`, id)
	err := w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', 'X')
			ON CONFLICT (id) DO UPDATE SET status = 'active'`, id, jid)
		return err
	})
	if pgCode(err) != "LC040" {
		t.Fatalf("INSERT ... ON CONFLICT DO UPDATE on licences must be LC040, got %v", err)
	}
	if w.licRow(id).Status != "suspended" {
		t.Fatal("the licence was reinstated")
	}
}

// I-1: on every governed table the zz_ governance guards are the LAST BEFORE row triggers (PostgreSQL fires triggers by name), so
// they judge the row after every other BEFORE trigger (for example licences_touch_updated_at) has had its say.
func TestHardeningI1_GovernanceGuardsFireLast(t *testing.T) {
	w := newWorld(t)
	type trg struct{ table, name string }
	var all []trg
	if err := w.owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT c.relname::text, t.tgname::text FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			 WHERE NOT t.tgisinternal AND c.relnamespace = 'public'::regnamespace
			   AND (t.tgtype & 1) = 1 AND (t.tgtype & 2) = 2 /* ROW, BEFORE */
			 ORDER BY c.relname, t.tgname COLLATE "C"`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x trg
			if err := rows.Scan(&x.table, &x.name); err != nil {
				return err
			}
			all = append(all, x)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	byTable := map[string][]string{}
	for _, x := range all {
		byTable[x.table] = append(byTable[x.table], x.name)
	}
	for _, table := range []string{"licences", "tenants", "tenant_jurisdiction_configs", "licence_country_ceilings", "operating_country_policies"} {
		names := byTable[table]
		if len(names) == 0 {
			t.Fatalf("%s has no BEFORE row triggers at all", table)
		}
		last := names[len(names)-1]
		if !strings.HasPrefix(last, "zz_licensing_change_governed") {
			t.Errorf("%s: the last BEFORE row trigger is %q, want a zz_licensing_change_governed* guard (all: %v)", table, last, names)
		}
		for _, n := range names[:len(names)-1] {
			if strings.HasPrefix(n, "zz_") && !strings.HasPrefix(n, "zz_licensing_change_governed") {
				t.Errorf("%s: trigger %q sorts among the guards; it must sort before them", table, n)
			}
		}
	}
	// licences carries BOTH guards (INSERT and UPDATE) and they are the last two.
	n := byTable["licences"]
	if len(n) < 2 || !strings.HasPrefix(n[len(n)-2], "zz_licensing_change_governed") {
		t.Errorf("licences: both zz_ guards must be the last two BEFORE row triggers, got %v", n)
	}
	// Every other governance trigger family of the licensing tables sorts before any zz_: licence_versions has only deny triggers.
	for _, name := range byTable["licence_versions"] {
		if strings.HasPrefix(name, "zz_") {
			t.Errorf("licence_versions: unexpected zz_ trigger %q", name)
		}
	}
}

// I-2: the runtime role cannot DELETE a licence (privilege revoked), and a platform-admin session deleting one is refused.
func TestHardeningI2_NoDeleteOnLicences(t *testing.T) {
	w := newWorld(t)
	id := w.newLicence()
	var has bool
	if err := w.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT has_table_privilege(current_user, 'licences', 'DELETE')`).Scan(&has)
	}); err != nil || has {
		t.Fatalf("igaming_runtime must not hold DELETE on licences: has=%v err=%v", has, err)
	}
	err := w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM licences WHERE id = $1`, id)
		return err
	})
	if pgCode(err) != "42501" {
		t.Fatalf("DELETE on licences as the runtime role: want 42501, got %v", err)
	}
	if w.ownerInt(`SELECT count(*) FROM licences WHERE id = $1`, id) != 1 {
		t.Fatal("the licence was deleted")
	}
}

// I-3: the licence_create INSERT match pins every lifecycle column to its creation value, review_due_at and evidence_reference
// included. Checked on the match function itself (a licence_create request needs two approvals to execute).
func TestHardeningI3_LicenceCreateMatchRequiresNullLifecycleColumns(t *testing.T) {
	w := newWorld(t)
	jid := w.jurisdiction()
	p := map[string]any{"jurisdiction_id": jid.String(), "licensee": "platform", "licence_number": "I3", "permitted_products": []string{}, "permitted_markets": []string{}}
	pj, _ := json.Marshal(p)
	row := func(extra map[string]any) string {
		r := map[string]any{"jurisdiction_id": jid.String(), "licensee": "platform", "licence_number": "I3", "permitted_products": []string{}, "permitted_markets": []string{},
			"status": "active", "issued_at": nil, "expires_at": nil, "review_due_at": nil, "evidence_reference": nil}
		for k, v := range extra {
			r[k] = v
		}
		b, _ := json.Marshal(r)
		return string(b)
	}
	match := func(newRow string) bool {
		var ok bool
		if err := w.rt.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT licensing_change_row_matches('licence_create', $1::jsonb, 'r', NULL, 'licences', 'INSERT', $2::jsonb, NULL)`, string(pj), newRow).Scan(&ok)
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !match(row(nil)) {
		t.Fatal("the creation shape must match")
	}
	for name, extra := range map[string]map[string]any{
		"review_due_at set":        {"review_due_at": "2030-01-01"},
		"evidence_reference set":   {"evidence_reference": "SMUGGLED"},
		"issued_at set":            {"issued_at": "2020-01-01"},
		"expires_at set":           {"expires_at": "2099-01-01"},
		"status not active":        {"status": "suspended"},
		"different licence number": {"licence_number": "OTHER"},
	} {
		if match(row(extra)) {
			t.Errorf("%s must not match licence_create", name)
		}
	}
}

// I-5: the platform read views leave their transaction read-only: a write after the call is refused (25006).
func TestHardeningI5_ReadViewsAreReadOnly(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	id := w.newLicence()
	for name, call := range map[string]func(ctx context.Context, tx pgx.Tx) error{
		"policies": func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := operatingmarket.ListTenantPolicyVersionsPage(ctx, tx, tid, "", false, 10, 0)
			return err
		},
		"ceilings": func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := operatingmarket.ListLicenceCeilingVersionsPage(ctx, tx, id, "", false, 10, 0)
			return err
		},
		"configs": func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := jurisdiction.ListTenantJurisdictionConfigs(ctx, tx, tid, 10, 0)
			return err
		},
	} {
		err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			if err := call(ctx, tx); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE tenants SET name = name WHERE id = $1`, tid)
			return err
		})
		if pgCode(err) != "25006" {
			t.Errorf("%s: a write after the read view must be refused (25006), got %v", name, err)
		}
	}
}

// 0179 up / down / up restores the 0178 schema byte for byte, and the down is refused (LC095) while a version row carries a
// change_request_id.
func TestMigration0179_UpDownUp_SnapshotAndDownRefusal(t *testing.T) {
	ctx := context.Background()
	u := scratchdb.New(t, "m0179_")
	pool, err := db.Connect(ctx, u, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	full, _ := migrationsThrough(t, 179)
	base, _ := migrationsThrough(t, 178)
	if _, err := pool.MigrateUp(ctx, base); err != nil {
		t.Fatalf("migrate to 0178: %v", err)
	}
	before := schemaSnapshot(t, pool)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != 179 {
		t.Fatalf("0179 up: %v %v", applied, err)
	}
	up := schemaSnapshot(t, pool)
	if !strings.Contains(up, "column:licence_versions:change_request_id") {
		t.Fatal("0179 up must add licence_versions.change_request_id")
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("0179 down: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != before {
		t.Fatalf("0179 down must restore the 0178 schema byte for byte; %s", firstDiff(before, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("0179 up again: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatal("0179 up again must equal the first up")
	}

	prooftest.InstallVia(t, pool.Raw(), u)
	launchfix.SeedOperatorsAt(t, u)
	ops := launchfix.Operators()
	var licID uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		jid := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'm0179')`, jid, "M-"+strings.ToUpper(jid.String()[:8])); err != nil {
			return err
		}
		licID = uuid.New()
		_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', 'M0179')`, licID, jid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{Pool: pool})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Request(ctx, Actor{StaffID: ops.Requester, Scope: ScopePlatform, Role: string(auth.RolePlatformAdmin)},
		RequestInput{Kind: KindLicenceEvidenceReferenceChange, Payload: payload(t, LicenceEvidenceReferencePayload{LicenceID: licID, Reference: sp("M0179")}), ReasonCode: "m0179"}); err != nil {
		t.Fatalf("seed a governed change: %v", err)
	}
	_, err = pool.MigrateDown(ctx, full, 1)
	if err == nil || !strings.Contains(err.Error(), "LC095") {
		t.Fatalf("0179 down must refuse LC095 while a version carries a change_request_id, got %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatal("a refused down must change nothing")
	}
}

func firstDiff(a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	inA := map[string]bool{}
	for _, l := range la {
		inA[l] = true
	}
	inB := map[string]bool{}
	for _, l := range lb {
		inB[l] = true
	}
	var out []string
	for _, l := range la {
		if !inB[l] && len(out) < 4 {
			out = append(out, "only before: "+l)
		}
	}
	for _, l := range lb {
		if !inA[l] && len(out) < 8 {
			out = append(out, "only after: "+l)
		}
	}
	return strings.Join(out, " | ")
}
