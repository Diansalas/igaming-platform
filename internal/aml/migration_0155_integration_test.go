//go:build integration

// ADR 0116 A7 round 3 (SR7-10) migration 0155 on THROWAWAY scratch databases: up / down / up whole-schema snapshot against the
// migration immediately before it, the pre-flights (AM098), and the down refusal (AM099) while sanctions_potential history exists.
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
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

const m0155 = 155

func TestMigration0155_UpDownUp_WholeSchema(t *testing.T) {
	ctx := context.Background()
	pool, _ := scratch(t, "m0155s_")
	if _, err := pool.MigrateUp(ctx, migDir(t, versionBefore(t, m0155))); err != nil {
		t.Fatal(err)
	}
	base := snapshot(t, pool)
	full := migDir(t, m0155)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("up: %v", err)
	}
	up := snapshot(t, pool)
	if up == base {
		t.Fatal("0155 changed nothing")
	}
	for _, want := range []string{"sanctions_potential", "aml_screening_results_check10", "aml_holds_reason_class_check"} {
		if !strings.Contains(up, want) {
			t.Errorf("the up snapshot lacks %q", want)
		}
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	wantSame(t, "after down 0155", base, snapshot(t, pool))
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("second up: %v", err)
	}
	wantSame(t, "second up vs first up", up, snapshot(t, pool))
}

// seedWorld builds a platform-licensed tenant with a configured jurisdiction, two officers and one player, on a scratch pool
// already migrated to the schema the caller chose.
func seedWorld(t *testing.T, pool *db.Pool, url, name string) (tenant, person, jid uuid.UUID, q *amlfix.Quorum, svc *aml.Service) {
	t.Helper()
	ctx := context.Background()
	prooftest.InstallVia(t, pool.Raw(), t.Name())
	launchfix.SeedOperatorsAt(t, url)
	if err := pool.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, name, name+"-"+uuid.NewString(), "under_platform_licence")
		tenant = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		b, err := launchfix.CreateBrand(ctx, tx, tenant, "b", name+"-b-"+uuid.NewString())
		if err != nil {
			return err
		}
		a, err := registerFixturePlayer(ctx, tx, b)
		person = a.PersonID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	jid = uuid.New()
	if err := pool.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, $3)`, jid, "MD-"+jid.String()[:12], name)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, effective_from)
			VALUES ($1, $2, 'k', 'a', 'r', now() - interval '1 hour')`, tenant, jid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return tenant, person, jid, amlfix.Bootstrap(t, pool, tenant, 2), amlfix.Service(t, pool)
}

func recordPotentialSanctions(t *testing.T, svc *aml.Service, a aml.Actor, person uuid.UUID, key string) error {
	t.Helper()
	_, err := svc.RecordScreening(context.Background(), a, aml.RecordScreeningInput{PersonID: person, IdempotencyKey: key, Trigger: aml.TriggerManual,
		ProviderID: aml.MockProviderID, SubjectRef: strings.Repeat("a", 64), Lists: []aml.ListType{aml.ListSanctions}, Outcome: aml.OutcomePotentialMatch,
		MatchLists: []aml.ListType{aml.ListSanctions}, ScreenedAt: time.Now(), Decision: aml.HoldAllValueMovement, HoldReasonCode: "sanctions_potential_match"})
	return err
}

func TestMigration0155_Preflight(t *testing.T) {
	ctx := context.Background()
	f := "0155_aml_sanctions_potential_class.up.sql"
	// Without 0154 it is refused.
	pool0, _ := scratch(t, "m0155q_")
	if _, err := pool0.MigrateUp(ctx, migDir(t, 139)); err != nil {
		t.Fatal(err)
	}
	preflightRefuses(t, pool0, f, "0141, 0142 and 0154", nil)

	// A potential sanctions result already recorded under the old manual class (0154 behaviour) is refused, nothing changes.
	pool, url := scratch(t, "m0155p_")
	if _, err := pool.MigrateUp(ctx, migDir(t, versionBefore(t, m0155))); err != nil {
		t.Fatal(err)
	}
	_, person, jid, q, svc := seedWorld(t, pool, url, "m0155p")
	if _, err := svc.SetScreeningPolicy(ctx, q.Officers[0].Actor, aml.SetScreeningPolicyInput{JurisdictionID: jid, Lists: []aml.ListType{aml.ListSanctions},
		RescreenInterval: time.Hour, EffectiveFrom: time.Now().Add(-time.Hour), LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := recordPotentialSanctions(t, svc, q.Officers[0].Actor, person, "m0155p-1"); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, pool)
	preflightRefuses(t, pool, f, "potential sanctions result recorded under the old manual class", nil)
	wantSame(t, "a refused pre-flight changes nothing (FORCE RLS restored)", before, snapshot(t, pool))
}

// sanctions_potential history is compliance history: the down refuses (AM099) and deletes nothing.
func TestMigration0155Down_RefusesWhileSanctionsPotentialHistoryExists(t *testing.T) {
	ctx := context.Background()
	pool, url := scratch(t, "m0155d_")
	full := migDir(t, m0155)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	_, person, jid, q, svc := seedWorld(t, pool, url, "m0155d")
	if _, err := svc.SetScreeningPolicy(ctx, q.Officers[0].Actor, aml.SetScreeningPolicyInput{JurisdictionID: jid, Lists: []aml.ListType{aml.ListSanctions},
		RescreenInterval: time.Hour, EffectiveFrom: time.Now().Add(-time.Hour), LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := recordPotentialSanctions(t, svc, q.Officers[0].Actor, person, "m0155d-1"); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, pool)
	if _, err := pool.MigrateDown(ctx, full, 1); err == nil || !strings.Contains(fmt.Sprint(err), "AM099") {
		t.Fatalf("0155 down must refuse AM099 while a sanctions_potential hold exists: %v", err)
	}
	wantSame(t, "a refused down changes nothing", before, snapshot(t, pool))
	hs, err := svc.ListHolds(ctx, q.Officers[1].Actor, person)
	if err != nil || len(hs) != 1 || hs[0].ReasonClass != aml.ReasonSanctionsPotential {
		t.Fatalf("the hold must survive the refused down: %+v %v", hs, err)
	}
}
