//go:build integration

// ADR 0116 migration 0154 (screening foundation + readiness reader, A7) on THROWAWAY scratch databases: up / down / up whole
// schema snapshot, the pre-flight (AM098), and the down refusal (AM099) while screening history exists.
// Requires TEST_DATABASE_URL and TEST_ADMIN_DATABASE_URL (scratchdb); a skip is NOT evidence.
package aml_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

func TestMigration0154_UpDownUp_WholeSchema(t *testing.T) {
	ctx := context.Background()
	pool, _ := scratch(t, "m0154s_")
	if _, err := pool.MigrateUp(ctx, migDir(t, 151)); err != nil {
		t.Fatal(err)
	}
	base := snapshot(t, pool)
	full := migDir(t, 154)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("up: %v", err)
	}
	up := snapshot(t, pool)
	if up == base {
		t.Fatal("0154 changed nothing")
	}
	for _, want := range []string{"table:aml_screening_policies:rls=true:force=true", "table:aml_screening_results:rls=true:force=true", "function:aml_readiness(uuid,uuid[])"} {
		if !strings.Contains(up, want) {
			t.Errorf("the snapshot lacks %q", want)
		}
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	wantSame(t, "after down 0154", base, snapshot(t, pool))
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("second up: %v", err)
	}
	wantSame(t, "second up vs first up", up, snapshot(t, pool))
}

func TestMigration0154_Preflight(t *testing.T) {
	ctx := context.Background()
	pool, _ := scratch(t, "m0154p_")
	if _, err := pool.MigrateUp(ctx, migDir(t, 151)); err != nil {
		t.Fatal(err)
	}
	f := "0154_aml_screening_and_readiness.up.sql"
	before := snapshot(t, pool)
	preflightRefuses(t, pool, f, "AML screening table", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE TABLE aml_screening_results (id int)`)
		return err
	})
	preflightRefuses(t, pool, f, "AML screening function", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE FUNCTION aml_readiness() RETURNS int LANGUAGE sql AS 'SELECT 1'`)
		return err
	})
	wantSame(t, "a refused pre-flight changes nothing", before, snapshot(t, pool))
	// Without 0140-0142 it is refused.
	pool2, _ := scratch(t, "m0154q_")
	if _, err := pool2.MigrateUp(ctx, migDir(t, 139)); err != nil {
		t.Fatal(err)
	}
	preflightRefuses(t, pool2, f, "0140-0142", nil)
}

// Screening history is compliance history: the down refuses (AM099) and deletes nothing, for a result row and for a policy row.
func TestMigration0154Down_RefusesWhileScreeningHistoryExists(t *testing.T) {
	ctx := context.Background()
	pool, url := scratch(t, "m0154d_")
	full := migDir(t, 154)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	prooftest.InstallVia(t, pool.Raw(), t.Name())
	launchfix.SeedOperatorsAt(t, url)
	var tenant uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "m0154d", "m0154d-"+uuid.NewString(), "under_platform_licence")
		tenant = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var person uuid.UUID
	if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		b, err := launchfix.CreateBrand(ctx, tx, tenant, "b", "m0154d-b-"+uuid.NewString())
		if err != nil {
			return err
		}
		a, err := registerFixturePlayer(ctx, tx, b)
		person = a.PersonID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	jid := uuid.New()
	if err := pool.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'm0154d')`, jid, "MD-"+jid.String()[:12])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	q := amlfix.Bootstrap(t, pool, tenant, 2)
	svc := amlfix.Service(t, pool)
	refused := func(what string) {
		t.Helper()
		before := snapshot(t, pool)
		if _, err := pool.MigrateDown(ctx, full, 1); err == nil || !strings.Contains(fmt.Sprint(err), "AM099") {
			t.Fatalf("0154 down must refuse AM099 (%s): %v", what, err)
		}
		wantSame(t, "a refused down changes nothing", before, snapshot(t, pool))
	}
	// (a) a policy row alone.
	if _, err := svc.SetScreeningPolicy(ctx, q.Officers[0].Actor, aml.SetScreeningPolicyInput{JurisdictionID: jid, Lists: []aml.ListType{aml.ListSanctions},
		RescreenInterval: time.Hour, EffectiveFrom: time.Now().Add(-time.Hour), LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture"}); err != nil {
		t.Fatal(err)
	}
	refused("a policy row")
	// (b) a result row.
	if _, err := svc.RecordScreening(ctx, q.Officers[0].Actor, aml.RecordScreeningInput{PersonID: person, IdempotencyKey: "m0154d-1", Trigger: aml.TriggerManual,
		ProviderID: aml.MockProviderID, SubjectRef: strings.Repeat("a", 64), Lists: []aml.ListType{aml.ListSanctions}, Outcome: aml.OutcomeClear, ScreenedAt: time.Now(), Decision: aml.HoldNone}); err != nil {
		t.Fatal(err)
	}
	refused("a result row")
	if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_results`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("the result must survive the refused down, found %d", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
