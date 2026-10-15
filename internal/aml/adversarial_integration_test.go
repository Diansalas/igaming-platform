//go:build integration

package aml_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

// THREAT-MODEL-ARBITRARY-SQL-1 (ADR 0110): these tests run RAW SQL as igaming_runtime with arbitrary GUCs set. The runtime role
// must reach no AML row, and must reach no AML write path without a valid, single-use, correctly bound proof, even when it
// forges the exact GUC shape of a real officer.

var amlTables = []string{"aml_holds", "aml_hold_lifts", "aml_enforcement_decisions", "aml_alerts", "aml_cases", "aml_case_alerts", "aml_case_notes", "aml_case_events",
	"aml_screening_policies", "aml_screening_results"} // migration 0154 (A7) added the last two

func TestRuntime_NoAccessToAMLTables_EvenWithForgedOfficerGUCs(t *testing.T) {
	w := newWorld(t, "own_licence")
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	officer := q.Officers[0].StaffID
	svc := amlfix.Service(t, w.RT)
	p := w.player()
	hold := q.PlaceHold(t, svc, p.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	if _, err := svc.OpenCase(bg, q.Officers[0].Actor, p.PersonID, aml.CaseKindManual, "t", nil); err != nil {
		t.Fatal(err)
	}
	shapes := map[string]func(ctx context.Context, tx pgx.Tx) error{
		"plain tenant session": func(ctx context.Context, tx pgx.Tx) error { return nil },
		"forged officer shape": func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT set_config('app.principal_id', $1, true), set_config('app.aml_access', 'on', true)`, officer.String())
			return err
		},
	}
	for name, setup := range shapes {
		for _, tbl := range amlTables {
			for _, stmt := range []string{
				`SELECT count(*) FROM ` + tbl,
				`INSERT INTO ` + tbl + ` DEFAULT VALUES`,
				`UPDATE ` + tbl + ` SET tenant_id = tenant_id`,
				`DELETE FROM ` + tbl,
				`TRUNCATE ` + tbl,
			} {
				err := w.RT.WithTenant(bg, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
					if err := setup(ctx, tx); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, stmt)
					return err
				})
				if pgCode(err) != "42501" {
					t.Errorf("%s: %q as the runtime role must be permission denied (42501), got %v", name, stmt, err)
				}
			}
		}
	}
	// The same statements as the OWNER are bound by the owner-only tenant policy: no GUC, no rows, even for the owner.
	var n int
	if err := w.Owner.WithoutTenant(bg, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM aml_holds) + (SELECT count(*) FROM aml_cases)`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("FORCE RLS: the owner without the session tenant must see no AML row: n=%d err=%v", n, err)
	}
	_ = hold
}

// rawCall runs one officer-API call as raw SQL in the officer's real session shape with the given proof token (or none).
func rawCall(w *amlWorld, officer uuid.UUID, token string, sql string, args ...any) error {
	return w.RT.WithPrincipalScope(bg, w.Tenant, officer, func(ctx context.Context, tx pgx.Tx) error {
		if token != "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, token); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

func TestRuntime_OfficerAPI_RequiresValidSingleUseBoundProof(t *testing.T) {
	w := newWorld(t, "own_licence")
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	o0, o1 := q.Officers[0].StaffID, q.Officers[1].StaffID
	p := w.player()
	// A real staff member with NO grant: a perfectly valid proof for themselves must not help.
	plain, _ := amlfix.NewStaff(t, w.Owner, w.Tenant)

	const sql = `SELECT aml_alert_raise($1, $2, 'low', 't')`
	hash := actorproof.Digest(actorproof.S(w.Tenant.String()), actorproof.S(p.PersonID.String()), actorproof.S("low"), actorproof.S("t"))
	f := prooftest.ProcessForger(t, ownerURL(t))
	valid := func(actor uuid.UUID, scope string, tenant uuid.UUID, op, target, payload string) string {
		return f.Valid(t, actor, scope, tenant, op, target, payload)
	}
	good := func() string {
		return valid(o0, "tenant", w.Tenant, "aml_alert:raise", "new", hash)
	}
	now := time.Now().Unix()
	other := uuid.New()

	cases := []struct {
		name  string
		actor uuid.UUID
		token string
		want  string
	}{
		{"no proof at all", o0, "", "AP001"},
		{"malformed proof", o0, "v1|x|y", "AP001"},
		{"proof signed for ANOTHER officer", o0, valid(o1, "tenant", w.Tenant, "aml_alert:raise", "new", hash), "AP004"},
		{"proof for another operation", o0, valid(o0, "tenant", w.Tenant, "aml_case:open", "new", hash), "AP004"},
		{"proof for another target", o0, valid(o0, "tenant", w.Tenant, "aml_alert:raise", other.String(), hash), "AP004"},
		{"proof for other arguments (payload hash)", o0, valid(o0, "tenant", w.Tenant, "aml_alert:raise", "new", strings.Repeat("a", 64)), "AP004"},
		{"proof for another tenant", o0, valid(o0, "tenant", other, "aml_alert:raise", "new", hash), "AP004"},
		{"proof claiming the acting scope", o0, valid(o0, "platform_acting", w.Tenant, "aml_alert:raise", "new", hash), "AP004"},
		{"expired proof", o0, f.Token(t, o0, "tenant", w.Tenant, "aml_alert:raise", "new", hash, now-100, now-70), "AP003"},
		{"lifetime above 60s", o0, f.Token(t, o0, "tenant", w.Tenant, "aml_alert:raise", "new", hash, now, now+120), "AP003"},
		{"wrong key (forged MAC)", o0, prooftest.Forger{KID: f.KID, Key: []byte("0123456789abcdef0123456789abcdef")}.Valid(t, o0, "tenant", w.Tenant, "aml_alert:raise", "new", hash), "AP002"},
		{"unknown key id", o0, prooftest.Forger{KID: "nokid", Key: f.Key}.Valid(t, o0, "tenant", w.Tenant, "aml_alert:raise", "new", hash), "AP002"},
		{"valid proof but NO aml_officer grant", plain, valid(plain, "tenant", w.Tenant, "aml_alert:raise", "new", hash), "AM001"},
		{"forged session of a non-existent staff member", uuid.New(), "", "CG001"},
	}
	for _, c := range cases {
		err := rawCall(w, c.actor, c.token, sql, w.Tenant, p.PersonID)
		if pgCode(err) != c.want {
			t.Errorf("%s: want %s, got %v", c.name, c.want, err)
		}
	}
	// Positive control: the same call with exactly the right proof succeeds, so none of the above failed for another reason.
	tok := good()
	if err := rawCall(w, o0, tok, sql, w.Tenant, p.PersonID); err != nil {
		t.Fatalf("a correctly bound proof must work: %v", err)
	}
	// Replay: the SAME proof a second time is refused (single use, serialised by the nonce key).
	if err := rawCall(w, o0, tok, sql, w.Tenant, p.PersonID); pgCode(err) != "AP005" {
		t.Fatalf("a replayed proof must be AP005, got %v", err)
	}
	// A call for the wrong tenant argument with a proof bound to the right session is refused before any write.
	hashOther := actorproof.Digest(actorproof.S(other.String()), actorproof.S(p.PersonID.String()), actorproof.S("low"), actorproof.S("t"))
	if err := rawCall(w, o0, valid(o0, "tenant", w.Tenant, "aml_alert:raise", "new", hashOther), sql, other, p.PersonID); pgCode(err) != "AM002" {
		t.Fatalf("an argument tenant other than the session tenant must be AM002, got %v", err)
	}
}

// A proof is consumed by the transaction that uses it: a failed call does not burn it (the nonce insert rolls back), and it
// cannot be spent on a DIFFERENT call (the binding includes the arguments).
func TestRuntime_Proof_NotBurnedByFailedCall_NotTransferable(t *testing.T) {
	w := newWorld(t, "own_licence")
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	o0 := q.Officers[0].StaffID
	p, p2 := w.player(), w.player()
	f := prooftest.ProcessForger(t, ownerURL(t))
	const sql = `SELECT aml_alert_raise($1, $2, 'low', 't')`
	hash := func(person uuid.UUID) string {
		return actorproof.Digest(actorproof.S(w.Tenant.String()), actorproof.S(person.String()), actorproof.S("low"), actorproof.S("t"))
	}
	tok := f.Valid(t, o0, "tenant", w.Tenant, "aml_alert:raise", "new", hash(p.PersonID))
	// Spend it on the OTHER person's arguments: refused (AP004), nothing written.
	if err := rawCall(w, o0, tok, sql, w.Tenant, p2.PersonID); pgCode(err) != "AP004" {
		t.Fatalf("a proof cannot be spent on other arguments: %v", err)
	}
	// It is still unspent: the call it was bound to succeeds.
	if err := rawCall(w, o0, tok, sql, w.Tenant, p.PersonID); err != nil {
		t.Fatalf("a refused attempt must not burn the proof: %v", err)
	}
}

// The enforcement read refuses a tenant other than the session's (AM026): never a silently filtered `false`.
func TestRuntime_HoldRead_SessionTenantBound_AndDisclosesOwnTenantOnly(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	p := w.player()
	other := newWorld(t, "under_platform_licence")
	err := w.RT.WithTenant(bg, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var b bool
		return tx.QueryRow(ctx, `SELECT aml_hold_active($1, $2, $3, 'deposit')`, other.Tenant, p.PersonID, p.AccountID).Scan(&b)
	})
	if pgCode(err) != "AM026" {
		t.Fatalf("a foreign tenant argument must be AM026: %v", err)
	}
	err = w.RT.WithoutTenant(bg, func(ctx context.Context, tx pgx.Tx) error {
		var b bool
		return tx.QueryRow(ctx, `SELECT aml_hold_active($1, $2, $3, 'deposit')`, w.Tenant, p.PersonID, p.AccountID).Scan(&b)
	})
	if pgCode(err) != "AM026" {
		t.Fatalf("a session with no tenant must be AM026 (fail closed), got %v", err)
	}
	for _, bad := range []string{"", "sell", "DEPOSIT", "deposit "} {
		err = w.RT.WithTenant(bg, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			var b bool
			return tx.QueryRow(ctx, `SELECT aml_hold_active($1, $2, $3, $4)`, w.Tenant, p.PersonID, p.AccountID, bad).Scan(&b)
		})
		if pgCode(err) != "AM004" {
			t.Fatalf("operation %q must be AM004: %v", bad, err)
		}
	}
	// aml_record_enforcement_decision: tenant bound, pairing checked, closed vocabularies (CHECKs).
	rec := func(tenant, acct, person uuid.UUID, op, kyc, hold, pub string, allowed bool) error {
		return w.RT.WithTenant(bg, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			return tx.QueryRow(ctx, `SELECT aml_record_enforcement_decision($1,$2,$3,$4,$5,$6,$7,$8,NULL,$9)`,
				tenant, acct, person, op, kyc, hold, pub, allowed, uuid.New()).Scan(&id)
		})
	}
	if err := rec(other.Tenant, p.AccountID, p.PersonID, "deposit", "passed", "eval_error", "review", false); pgCode(err) != "AM026" {
		t.Fatalf("foreign tenant: %v", err)
	}
	if err := rec(w.Tenant, p.AccountID, uuid.New(), "deposit", "passed", "eval_error", "review", false); pgCode(err) != "AM007" {
		t.Fatalf("account/person mismatch: %v", err)
	}
	if err := rec(w.Tenant, p.AccountID, p.PersonID, "deposit", "passed", "eval_error", "review", false); err != nil {
		t.Fatalf("a well-formed record: %v", err)
	}
	if err := rec(w.Tenant, p.AccountID, p.PersonID, "deposit", "passed", "eval_error", "review", true); err == nil {
		t.Fatal("an ALLOWED record with a hold outcome must violate the CHECK")
	}
	if err := rec(w.Tenant, p.AccountID, p.PersonID, "bogus", "passed", "eval_error", "review", false); err == nil {
		t.Fatal("an unknown operation must violate the CHECK")
	}
}

// Append-only, binding on the OWNER too: UPDATE, DELETE and TRUNCATE are refused on every AML table.
func TestAppendOnly_BindsTheOwner(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	c.Q.PlaceHold(t, c.Svc, c.P.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	id := c.open(t)
	_, _ = c.Svc.RaiseAlert(bg, c.A(), c.P.PersonID, aml.SeverityLow, "t")
	if _, err := c.Svc.AddNote(bg, c.A(), id, 1, "an ordinary note"); err != nil {
		t.Fatal(err)
	}
	a1, err := c.Svc.RaiseAlert(bg, c.A(), c.P.PersonID, aml.SeverityLow, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.OpenCase(bg, c.A(), c.P.PersonID, aml.CaseKindAlert, "t", []uuid.UUID{a1}); err != nil {
		t.Fatal(err)
	}
	// An enforcement decision row exists too.
	if err := c.RT.WithTenant(bg, c.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var x uuid.UUID
		return tx.QueryRow(ctx, `SELECT aml_record_enforcement_decision($1,$2,$3,'deposit','passed','eval_error','review',false,NULL,$4)`,
			c.Tenant, c.P.AccountID, c.P.PersonID, uuid.New()).Scan(&x)
	}); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range amlTables {
		for _, stmt := range []string{`UPDATE ` + tbl + ` SET tenant_id = tenant_id`, `DELETE FROM ` + tbl} {
			err := c.Owner.WithTenant(bg, c.Tenant, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, stmt)
				return err
			})
			if tbl == "aml_hold_lifts" {
				continue // no row yet; the TRUNCATE leg below still applies
			}
			if pgCode(err) != "AM005" {
				t.Errorf("%q must be AM005 for the owner, got %v", stmt, err)
			}
		}
		err := c.Owner.WithTenant(bg, c.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `TRUNCATE `+tbl+` CASCADE`)
			return err
		})
		if pgCode(err) != "AM005" {
			t.Errorf("TRUNCATE %s must be AM005 for the owner, got %v", tbl, err)
		}
	}
}
