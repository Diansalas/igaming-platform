//go:build integration

package licensingchange

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
)

// rtPlatform runs fn as igaming_runtime in a platform-admin session of staff.
func (w *world) rtPlatform(staff uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return w.rt.WithPlatformAdmin(context.Background(), staff, fn)
}

func setProof(ctx context.Context, tx pgx.Tx, tok string) error {
	_, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, tok)
	return err
}

// The database refuses a request, an approval and an execution without a valid signed proof, whatever the session GUCs
// claim (GUC impersonation by a stolen runtime credential).
func TestSQL_MissingOrForgedProof_Refused(t *testing.T) {
	w := newWorld(t)
	jid := w.jurisdiction()
	body := `{"jurisdiction_id":"` + jid.String() + `","licensee":"platform","licence_number":"LC-SQL","permitted_products":[],"permitted_markets":[]}`
	insert := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO licensing_change_requests (kind, payload, payload_hash, reason_code) VALUES ('licence_create', $1::jsonb, repeat('0', 64), 'x')`, body)
		return err
	}
	// No proof: AP001.
	if err := w.rtPlatform(w.ops.Requester, insert); pgCode(err) != "AP001" {
		t.Fatalf("a request without a proof must be AP001, got %v", err)
	}
	// A proof signed with a key the database does not hold: AP002.
	wrong := prooftest.Forger{KID: "lc-wrong", Key: []byte(strings.Repeat("w", 32))}
	err := w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var hash string
		if err := tx.QueryRow(ctx, `SELECT licensing_change_payload_hash('licence_create', $1::jsonb, 'x')`, body).Scan(&hash); err != nil {
			return err
		}
		if err := setProof(ctx, tx, wrong.Valid(t, w.ops.Requester, "platform", uuid.Nil, actorproof.OpLicensingChangeRequest, "new", hash)); err != nil {
			return err
		}
		return insert(ctx, tx)
	})
	if pgCode(err) != "AP002" {
		t.Fatalf("a forged proof must be AP002, got %v", err)
	}
	// A correctly keyed proof for a DIFFERENT payload hash (the payload was tampered after signing): AP004.
	good := prooftest.ProcessForger(t, w.ownerURL)
	err = w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if err := setProof(ctx, tx, good.Valid(t, w.ops.Requester, "platform", uuid.Nil, actorproof.OpLicensingChangeRequest, "new", strings.Repeat("ab", 32))); err != nil {
			return err
		}
		return insert(ctx, tx)
	})
	if pgCode(err) != "AP004" {
		t.Fatalf("a proof for another payload must be AP004, got %v", err)
	}
	// A proof bound to another actor (impersonating a different platform admin through the GUC): AP004.
	err = w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var hash string
		if err := tx.QueryRow(ctx, `SELECT licensing_change_payload_hash('licence_create', $1::jsonb, 'x')`, body).Scan(&hash); err != nil {
			return err
		}
		if err := setProof(ctx, tx, good.Valid(t, w.ops.Approver1, "platform", uuid.Nil, actorproof.OpLicensingChangeRequest, "new", hash)); err != nil {
			return err
		}
		return insert(ctx, tx)
	})
	if pgCode(err) != "AP004" {
		t.Fatalf("a proof for another actor must be AP004, got %v", err)
	}

	// Approval and execution without a proof, on a real open request.
	r := w.mustRequest(w.req, KindLicenceCreate, LicenceCreatePayload{JurisdictionID: jid, Licensee: "platform", LicenceNumber: "LC-SQL-" + uuid.NewString()[:6]}, "licence_granted").Request
	err = w.rtPlatform(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO licensing_change_approvals (request_id, reason_code) VALUES ($1, 'ok')`, r.ID)
		return err
	})
	if pgCode(err) != "AP001" {
		t.Fatalf("an approval without a proof must be AP001, got %v", err)
	}
	// The requester's own approval is refused by the database even with a valid proof (four-eyes, LC032).
	err = w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if err := setProof(ctx, tx, good.Valid(t, w.ops.Requester, "platform", uuid.Nil, actorproof.OpLicensingChangeApprove, r.ID.String(), r.PayloadHash)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO licensing_change_approvals (request_id, reason_code) VALUES ($1, 'ok')`, r.ID)
		return err
	})
	if pgCode(err) != "LC032" {
		t.Fatalf("a self-approval must be LC032 at the database, got %v", err)
	}
	// Executing (or marking approved) without the approvals: LC013 / LC020, never a write.
	err = w.rtPlatform(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE licensing_change_requests SET status = 'approved' WHERE id = $1`, r.ID)
		return err
	})
	if pgCode(err) != "LC020" {
		t.Fatalf("approved without approvals must be LC020, got %v", err)
	}
	err = w.rtPlatform(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE licensing_change_requests SET status = 'executed' WHERE id = $1`, r.ID)
		return err
	})
	if pgCode(err) != "LC013" {
		t.Fatalf("executed from requested on a two-person request must be LC013, got %v", err)
	}
	// Content is immutable (payload tampered after a request / approval): LC012, for the runtime role AND the owner.
	// The runtime role holds UPDATE on the state columns only (42501 on payload); the owner meets the guard (LC012).
	for name, want := range map[string]string{"runtime": "42501", "owner": "LC012"} {
		pool := w.rt
		if name == "owner" {
			pool = w.owner
		}
		err = pool.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE licensing_change_requests SET payload = jsonb_set(payload, '{licence_number}', '"TAMPERED"') WHERE id = $1`, r.ID)
			return err
		})
		if pgCode(err) != want {
			t.Fatalf("%s: a payload change must be %s, got %v", name, want, err)
		}
	}
	// Append-only: no DELETE (LC099) and no approval UPDATE.
	err = w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM licensing_change_requests WHERE id = $1`, r.ID)
		return err
	})
	if err == nil {
		t.Fatal("a request DELETE must be refused")
	}
}

// A single-actor (0-approval) change executes only by its requester, in the request's own transaction, with a proof.
func TestSQL_SingleActorExecution_RequesterSameTransactionOnly(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	good := prooftest.ProcessForger(t, w.ownerURL)
	body := `{"tenant_id":"` + tid.String() + `"}`
	// Request in one transaction (proof-bound), execution attempted in ANOTHER: LC021.
	var id uuid.UUID
	var hash string
	err := w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT licensing_change_payload_hash('tenant_licence_unbind', $1::jsonb, 'x')`, body).Scan(&hash); err != nil {
			return err
		}
		if err := setProof(ctx, tx, good.Valid(t, w.ops.Requester, "platform", uuid.Nil, actorproof.OpLicensingChangeRequest, "new", hash)); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO licensing_change_requests (kind, payload, payload_hash, reason_code) VALUES ('tenant_licence_unbind', $1::jsonb, $2, 'x') RETURNING id`, body, hash).Scan(&id)
	})
	mustNotErr(t, err, "single-actor request")
	err = w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if err := setProof(ctx, tx, good.Valid(t, w.ops.Requester, "platform", uuid.Nil, actorproof.OpLicensingChangeExecute, id.String(), hash)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE licensing_change_requests SET status = 'executed' WHERE id = $1`, id)
		return err
	})
	if pgCode(err) != "LC021" {
		t.Fatalf("a single-actor change executed in another transaction must be LC021, got %v", err)
	}
	// Executed by another platform admin: LC021 too.
	err = w.rtPlatform(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		if err := setProof(ctx, tx, good.Valid(t, w.ops.Approver1, "platform", uuid.Nil, actorproof.OpLicensingChangeExecute, id.String(), hash)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE licensing_change_requests SET status = 'executed' WHERE id = $1`, id)
		return err
	})
	if pgCode(err) != "LC021" {
		t.Fatalf("a single-actor change executed by someone else must be LC021, got %v", err)
	}
}

// Every governed table refuses a direct runtime write (LC040, or RLS for a tenant-scope configuration write), and an
// executed request of ANOTHER kind / payload in the same transaction does not admit it.
func TestSQL_GovernedTables_DirectRuntimeWriteRefused(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	jid := w.jurisdiction()
	lid := w.licence(jid, "platform", tid)
	other := w.licence(jid, "platform", uuid.Nil)
	cases := []struct {
		name   string
		tenant bool
		sql    string
		args   []any
		want   string
	}{
		{"licence_country_ceilings insert", false, `INSERT INTO licence_country_ceilings (licence_id, country_code, state, status, authorization_reference, reason_code, policy_version, created_by_actor_type, created_by_actor_id)
			VALUES ($1, 'MT', 'enabled', 'active', 'T', 'r', 'v', 'staff', $2)`, []any{lid, w.ops.Requester}, "LC040"},
		{"licences insert", false, `INSERT INTO licences (jurisdiction_id, licensee, licence_number) VALUES ($1, 'platform', 'LC-RAW')`, []any{jid}, "LC040"},
		{"tenants licence_id update", false, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, []any{other, tid}, "LC040"},
		{"tenants licence_id unbind", false, `UPDATE tenants SET licence_id = NULL WHERE id = $1`, []any{tid}, "LC040"},
		{"tenant_jurisdiction_configs platform insert", false, `INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id) VALUES ($1, $2, 'k')`, []any{tid, jid}, "LC040"},
		{"tenant_jurisdiction_configs tenant insert", true, `INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id) VALUES ($1, $2, 'k')`, []any{tid, jid}, "LC040"},
		{"operating_country_policies tenant insert", true, `INSERT INTO operating_country_policies (tenant_id, scope_kind, country_code, state, status, reason_code, policy_version, created_by_actor_type, created_by_actor_id)
			VALUES ($1, 'tenant', 'MT', 'disabled', 'active', 'r', 'v', 'staff', $2)`, []any{tid, w.ops.Requester}, "LC040"},
	}
	for _, c := range cases {
		var err error
		if c.tenant {
			err = w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, c.sql, c.args...)
				return err
			})
		} else {
			err = w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, c.sql, c.args...)
				return err
			})
		}
		if pgCode(err) != c.want {
			t.Errorf("%s as igaming_runtime: want SQLSTATE %s, got %v", c.name, c.want, err)
		}
	}
	// A configuration row (written through the executor) cannot be edited in place by the runtime role, from either scope.
	set := TenantJurisdictionConfigPayload{TenantID: tid, JurisdictionID: jid, Action: ConfigActionSet, KYCRulesetID: sp("k"),
		AllowedCurrencies: []string{"EUR"}, AllowedPaymentMethods: []string{}, GeoBlockList: []string{}}
	r := w.mustRequest(w.req, KindTenantJurisdictionConfigVersion, set, "configuration_initial").Request
	w.mustApprove(w.ap1, r.ID)
	w.mustApprove(w.ap2, r.ID)
	err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tenant_jurisdiction_configs SET kyc_ruleset_id = 'evil' WHERE tenant_id = $1`, tid)
		if err == nil && tag.RowsAffected() != 0 {
			t.Fatal("a tenant-scope runtime UPDATE must touch no row")
		}
		return err
	})
	mustNotErr(t, err, "tenant-scope update (RLS: zero rows)")
	err = w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenant_jurisdiction_configs SET kyc_ruleset_id = 'evil' WHERE tenant_id = $1`, tid)
		return err
	})
	if c := pgCode(err); c != "LC041" && c != "LC040" {
		t.Fatalf("a platform-scope in-place edit must be refused (LC041 / LC040), got %v", err)
	}
	// The tenant still READS its own configuration.
	if n := w.rtTenantInt(tid, `SELECT count(*) FROM tenant_jurisdiction_configs WHERE tenant_id = $1`, tid); n != 1 {
		t.Fatalf("the tenant reads its own configuration, got %d", n)
	}
}

func (w *world) rtTenantInt(tenantID uuid.UUID, sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}); err != nil {
		w.t.Fatalf("runtime tenant query: %v", err)
	}
	return n
}
