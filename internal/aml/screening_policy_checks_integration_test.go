//go:build integration

package aml_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The policy table's own CHECKs are the last line behind the definer function and the Go validation: even an owner-level insert
// (guard disabled for the transaction, so the CHECKs are what is tested) cannot write a policy with no list, an unknown list, a
// zero re-screen interval, a negative grace, a missing / blank / control-character legal review reference, a free-text reason or
// a non-platform author. TEST FIXTURE VALUES - NOT LEGAL THRESHOLDS.
func TestScreeningPolicies_ChecksInTheDatabase(t *testing.T) {
	ctx := context.Background()
	w, q, _ := platformWorld(t)
	j := w.jurisdiction()
	errRollback := errors.New("rolled back on purpose")
	type row struct {
		lists           string
		rescreen, grace int64
		legal, reason   string
		authorScope     string
	}
	good := row{"{pep}", 3600, 0, "TEST FIXTURE REF", "fixture", "platform_acting"}
	insert := func(r row) error {
		err := w.Owner.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `ALTER TABLE aml_screening_policies DISABLE TRIGGER aml_screening_policies_10_guard`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO aml_screening_policies (tenant_id, licensing_jurisdiction_id, lists, rescreen_interval_seconds, backfill_grace_seconds, effective_from,
					legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
				VALUES ($1, $2, $3::text[], $4, $5, now(), $6, $7, $8, $9, $10)`,
				w.Tenant, j, r.lists, r.rescreen, r.grace, r.legal, r.reason, q.Officers[0].StaffID, q.Officers[0].PersonID, r.authorScope); err != nil {
				return err
			}
			return errRollback
		})
		if errors.Is(err, errRollback) {
			return nil
		}
		return err
	}
	if err := insert(good); err != nil {
		t.Fatalf("control: a lawful policy shape must insert: %v", err)
	}
	with := func(f func(r *row)) row { r := good; f(&r); return r }
	for name, r := range map[string]row{
		"no list":                   with(func(r *row) { r.lists = "{}" }),
		"three lists":               with(func(r *row) { r.lists = "{sanctions,pep,adverse}" }),
		"an unknown list":           with(func(r *row) { r.lists = "{watch}" }),
		"zero re-screen interval":   with(func(r *row) { r.rescreen = 0 }),
		"negative re-screen":        with(func(r *row) { r.rescreen = -5 }),
		"negative backfill grace":   with(func(r *row) { r.grace = -1 }),
		"empty legal reference":     with(func(r *row) { r.legal = "" }),
		"over-long legal reference": with(func(r *row) { r.legal = strings.Repeat("x", 201) }),
		"control char in the ref":   with(func(r *row) { r.legal = "a\u0007b" }),
		"free-text reason":          with(func(r *row) { r.reason = "Not A Token" }),
		"empty reason":              with(func(r *row) { r.reason = "" }),
		"a tenant-scope author":     with(func(r *row) { r.authorScope = "tenant" }),
	} {
		if err := insert(r); pgCode(err) != "23514" {
			t.Errorf("%s: want a CHECK violation, got %v", name, err)
		}
	}
}
