//go:build loadtest

package main

import (
	"bytes"
	"context"
	"fmt"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5"
)

// The tables the seed writes through the OWNER path. Their user triggers (actor-proof guards, launch guards, the registration
// enqueue trigger) and FORCE ROW LEVEL SECURITY are switched off ONLY on the private scratch database for the bulk load and are
// restored (and verified) before the seed returns. The data written is shaped exactly as the production functions would leave it
// (the check constraints stay in force).
var seedTables = []string{"tenants", "brands", "persons", "player_accounts", "aml_screening_policies", "aml_screening_trigger_policies",
	"aml_screening_requests", "aml_screening_results", "aml_holds", "aml_enforcement_decisions", "kyc_verifications", "jurisdictions"}

const tenantSeedSQL = `
CREATE TEMP TABLE seed_s ON COMMIT DROP AS
SELECT g,
       md5('{{.Seed}}:p:{{.Tn}}:'||g)::uuid AS pid,
       md5('{{.Seed}}:a:{{.Tn}}:'||g)::uuid AS aid,
       ((('x'||substr(md5('{{.Seed}}:b:{{.Tn}}:'||g),1,8))::bit(32)::bigint)::float8/4294967296.0) AS u,
       ((('x'||substr(md5('{{.Seed}}:c:{{.Tn}}:'||g),1,8))::bit(32)::bigint)::float8/4294967296.0) AS u2,
       (g % {{.J}}) AS jx
  FROM generate_series(1,{{.P}}) g;
ALTER TABLE seed_s ADD COLUMN bucket text;
UPDATE seed_s SET bucket = CASE
  WHEN u < {{.C1}} THEN 'clear' WHEN u < {{.C2}} THEN 'stale' WHEN u < {{.C3}} THEN 'legacy'
  WHEN u < {{.C4}} THEN 'needs_officer' WHEN u < {{.C5}} THEN 'held' WHEN u < {{.C6}} THEN 'pending' ELSE 'none' END;
ANALYZE seed_s;

INSERT INTO tenants (id,name,slug,licensing_model,status) VALUES ('{{.TID}}','capa tenant {{.Tn}}','capa-{{.Seed}}-{{.Tn}}','under_platform_licence','active');
INSERT INTO brands (id,tenant_id,name,slug,status) VALUES ('{{.BID}}','{{.TID}}','capa brand','capa-b-{{.Seed}}-{{.Tn}}','active');
INSERT INTO persons (id) SELECT pid FROM seed_s;
INSERT INTO player_accounts (id,tenant_id,brand_id,person_id,email,password_hash,status,created_at,updated_at)
  SELECT aid,'{{.TID}}','{{.BID}}',pid,'p'||g||'@capa.invalid','!x','active', now()-interval '90 days', now()-interval '90 days' FROM seed_s;

-- screening policy versions (V per jurisdiction): version k took effect at now - (span+1d)*(V-k)/V
INSERT INTO aml_screening_policies (id,tenant_id,licensing_jurisdiction_id,lists,rescreen_interval_seconds,backfill_grace_seconds,effective_from,
       legal_review_reference,reason_code,created_by,created_by_person_id,created_by_scope,created_at)
  SELECT md5('{{.Seed}}:sp:{{.Tn}}:'||j.jx||':'||k)::uuid,'{{.TID}}',j.id,{{.Lists}},{{.I}},3600,
         now() - make_interval(secs => ({{.SpanSecs}}+86400)::float8*({{.V}}-k)/{{.V}}),
         'CAPA SYNTHETIC - NOT LEGAL','capa_fixture',md5('{{.Seed}}:staff')::uuid,md5('{{.Seed}}:staffp')::uuid,'platform_acting',
         now() - make_interval(secs => ({{.SpanSecs}}+86400)::float8*({{.V}}-k)/{{.V}})
    FROM capa_jur j CROSS JOIN generate_series(0,{{.V}}-1) k;
INSERT INTO aml_screening_trigger_policies (id,tenant_id,licensing_jurisdiction_id,events,gate_withdrawal,gate_alert_after_seconds,effective_from,
       legal_review_reference,reason_code,created_by,created_by_person_id,created_by_scope,created_at)
  SELECT md5('{{.Seed}}:tp:{{.Tn}}:'||j.jx||':'||k)::uuid,'{{.TID}}',j.id,ARRAY['registration','kyc_approval','scheduled'],true,{{.Alert}},
         now() - make_interval(secs => {{.SpanSecs}}::float8 - ({{.SpanSecs}}::float8*0.5)*k/{{.TV}}),
         'CAPA SYNTHETIC - NOT LEGAL','capa_fixture',md5('{{.Seed}}:staff')::uuid,md5('{{.Seed}}:staffp')::uuid,'platform_acting',
         now() - make_interval(secs => {{.SpanSecs}}::float8 - ({{.SpanSecs}}::float8*0.5)*k/{{.TV}})
    FROM capa_jur j CROSS JOIN generate_series(0,{{.TV}}-1) k;

-- done / needs_officer requests with full provenance. age (seconds): clear < I/2; stale 1.2I..3I; needs_officer up to I/2.
CREATE TEMP TABLE seed_r ON COMMIT DROP AS
SELECT s.g, s.pid, s.jx, s.bucket,
       CASE s.bucket WHEN 'clear' THEN s.u2*{{.I}}*0.5 WHEN 'held' THEN s.u2*{{.I}}*0.5 WHEN 'stale' THEN {{.I}}*(1.2+1.8*s.u2) ELSE s.u2*{{.I}}*0.5 + 60 END AS age
  FROM seed_s s WHERE s.bucket IN ('clear','held','stale','needs_officer');
ALTER TABLE seed_r ADD COLUMN k int;
UPDATE seed_r SET k = GREATEST(0, LEAST({{.V}}-1, floor({{.V}} - age*{{.V}}/({{.SpanSecs}}+86400))::int));
INSERT INTO aml_screening_requests (id,tenant_id,person_id,licensing_jurisdiction_id,trigger,idempotency_key,status,attempts,next_attempt_at,
       outcome,match_lists,provider_id,provider_reference,subject_ref,screened_at,completed_at,match_fingerprint,created_at,updated_at,
       screened_policy_id,screened_lists,screened_interval_seconds,claimed_at,claim_policy_id)
  SELECT md5('{{.Seed}}:rq:{{.Tn}}:'||r.g)::uuid,'{{.TID}}',r.pid,j.id,'scheduled','sched:'||r.g||':'||floor(extract(epoch from now())/{{.I}})::bigint||':seed',
         CASE WHEN r.bucket='needs_officer' THEN 'needs_officer' ELSE 'done' END, 1, now()-make_interval(secs=>r.age+10),
         CASE WHEN r.bucket='needs_officer' THEN 'potential_match' ELSE 'clear' END,
         CASE WHEN r.bucket='needs_officer' THEN ARRAY['pep'] END,'mock-screening','mock-'||r.g, md5(r.pid::text)||md5(r.g::text),
         now()-make_interval(secs=>r.age+2), now()-make_interval(secs=>r.age),
         CASE WHEN r.bucket='needs_officer' THEN md5('f'||r.g)||md5('g'||r.g) END,
         now()-make_interval(secs=>r.age+10), now()-make_interval(secs=>r.age),
         md5('{{.Seed}}:sp:{{.Tn}}:'||r.jx||':'||r.k)::uuid, {{.Lists}}, {{.I}}, now()-make_interval(secs=>r.age+3),
         md5('{{.Seed}}:sp:{{.Tn}}:'||r.jx||':'||r.k)::uuid
    FROM seed_r r JOIN capa_jur j ON j.jx = r.jx;
-- pending (due) requests
INSERT INTO aml_screening_requests (id,tenant_id,person_id,licensing_jurisdiction_id,trigger,idempotency_key,status,attempts,next_attempt_at)
  SELECT md5('{{.Seed}}:pq:{{.Tn}}:'||s.g)::uuid,'{{.TID}}',s.pid,j.id,'scheduled','sched:'||s.g||':pend:'||'{{.Seed}}','pending',0, now()-interval '1 minute'
    FROM seed_s s JOIN capa_jur j ON j.jx=s.jx WHERE s.bucket='pending';
-- legacy results: a clear recorded WITHOUT provenance (never covers)
INSERT INTO aml_screening_results (id,tenant_id,owner_scope,person_id,idempotency_key,trigger,provider_id,subject_ref,lists,outcome,screened_at,decision,
       payload_hash,recorded_by,recorded_by_person_id,recorded_by_scope,recorded_at)
  SELECT md5('{{.Seed}}:rs:{{.Tn}}:'||s.g)::uuid,'{{.TID}}','platform',s.pid,'legacy:'||s.g||':{{.Seed}}','manual','mock-screening',md5(s.pid::text)||md5(s.g::text),
         ARRAY['sanctions','pep'],'clear',now()-make_interval(secs=>s.u2*{{.I}}*0.5),'none',md5('h'||s.g)||md5('i'||s.g),
         md5('{{.Seed}}:staff')::uuid,md5('{{.Seed}}:staffp')::uuid,'platform_acting',now()-make_interval(secs=>s.u2*{{.I}}*0.5)
    FROM seed_s s WHERE s.bucket='legacy';
-- holds
INSERT INTO aml_holds (id,tenant_id,owner_scope,person_id,hold_scope,reason_class,reason_code,placed_by,placed_by_person_id,placed_by_scope)
  SELECT md5('{{.Seed}}:h:{{.Tn}}:'||s.g)::uuid,'{{.TID}}','platform',s.pid,'withdrawal_and_payout','manual','capa_fixture',md5('{{.Seed}}:staff')::uuid,md5('{{.Seed}}:staffp')::uuid,'platform_acting'
    FROM seed_s s WHERE s.bucket='held';
-- parked payout decisions in the last 7 days for a fraction of the persons the gate denies
INSERT INTO aml_enforcement_decisions (id,tenant_id,player_account_id,person_id,operation,kyc_outcome,hold_outcome,public_outcome,allowed,correlation_id,decided_at)
  SELECT md5('{{.Seed}}:d:{{.Tn}}:'||s.g||':'||n)::uuid,'{{.TID}}',s.aid,s.pid,'withdrawal_payout','passed','held','review',false,md5('{{.Seed}}:cr:'||s.g||':'||n)::uuid,
         now()-make_interval(secs => (s.u2*600000)*(n+1)/2.0)
    FROM seed_s s CROSS JOIN generate_series(0,1) n WHERE s.bucket IN ('stale','legacy','none') AND s.u2 < {{.DF}};
`

type tenantSeedArgs struct {
	Seed, Tn, J, P, V, TV  int64
	I, SpanSecs, Alert     int64
	C1, C2, C3, C4, C5, C6 float64
	DF                     float64
	TID, BID               string
	Lists                  string
}

// openOwner connects as the migration-owner role to the private database.
func openOwner(ctx context.Context, db string) (*pgx.Conn, error) {
	u, err := ownerURL(db)
	if err != nil {
		return nil, err
	}
	c, err := pgx.Connect(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("connect owner (%s): %v", db, errKind(err))
	}
	return c, nil
}

// errKind hides connection-string details from error text.
func errKind(err error) string { return fmt.Sprintf("%T", err) }

func md5uuid(ctx context.Context, c *pgx.Conn, s string) (string, error) {
	var id string
	err := c.QueryRow(ctx, `SELECT md5($1)::uuid::text`, s).Scan(&id)
	return id, err
}

// seedDB loads the scenario into the (already migrated) private database through the owner path.
func seedDB(ctx context.Context, db string, sc *Scenario) (map[string]any, error) {
	t0 := time.Now()
	c, err := openOwner(ctx, db)
	if err != nil {
		return nil, err
	}
	defer c.Close(ctx)
	// the environment anchor: only a development / staging / test database accepts worker results (same as rgfix.EnsureTestEnvironment).
	if _, err := c.Exec(ctx, `INSERT INTO database_environment (environment) VALUES ('test') ON CONFLICT (singleton) DO NOTHING`); err != nil {
		return nil, fmt.Errorf("environment anchor: %w", err)
	}
	var env string
	if err := c.QueryRow(ctx, `SELECT rg_database_environment()`).Scan(&env); err != nil || env != "test" {
		return nil, fmt.Errorf("environment anchor is %q: refusing to seed", env)
	}
	// Only a database that is EMPTY of tenants may be seeded.
	var n int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&n); err != nil || n != 0 {
		return nil, fmt.Errorf("database already has %d tenants", n)
	}
	for _, t := range seedTables {
		if _, err := c.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER USER`, t)); err != nil {
			return nil, fmt.Errorf("disable triggers on %s: %w", t, err)
		}
		if _, err := c.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s NO FORCE ROW LEVEL SECURITY`, t)); err != nil {
			return nil, fmt.Errorf("no force rls on %s: %w", t, err)
		}
	}
	// jurisdictions (shared by all tenants)
	for j := 0; j < sc.Jurisdictions; j++ {
		if _, err := c.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES (md5($1)::uuid, $2, 'capa synthetic')`,
			fmt.Sprintf("%d:jur:%d", sc.Seed, j), fmt.Sprintf("CAPA-%d-%d", sc.Seed, j)); err != nil {
			return nil, fmt.Errorf("jurisdiction: %w", err)
		}
	}
	cum := func(keys ...string) float64 {
		s := 0.0
		for _, k := range keys {
			s += sc.Mix[k]
		}
		return s
	}
	tmpl := template.Must(template.New("seed").Parse(tenantSeedSQL))
	span := int64(sc.GateFirstDaysAgo) * 86400
	lists := "ARRAY['sanctions','pep']"
	if intExtra(sc, "initial_lists_single", 0) == 1 {
		lists = "ARRAY['sanctions']"
	}
	tn := 0
	totalPersons := 0
	for _, g := range sc.Groups {
		for i := 0; i < g.Count; i++ {
			tn++
			tid, _ := md5uuid(ctx, c, fmt.Sprintf("%d:t:%d", sc.Seed, tn))
			bid, _ := md5uuid(ctx, c, fmt.Sprintf("%d:bd:%d", sc.Seed, tn))
			a := tenantSeedArgs{Seed: sc.Seed, Tn: int64(tn), J: int64(sc.Jurisdictions), P: int64(g.Persons), V: int64(sc.PolicyVersions), TV: int64(sc.TriggerVersions),
				I: sc.IntervalSeconds, SpanSecs: span, Alert: sc.GateAlertAfterSeconds, DF: sc.DecisionFraction, TID: tid, BID: bid, Lists: lists,
				C1: cum("clear"), C2: cum("clear", "stale"), C3: cum("clear", "stale", "legacy"), C4: cum("clear", "stale", "legacy", "needs_officer"),
				C5: cum("clear", "stale", "legacy", "needs_officer", "held"), C6: cum("clear", "stale", "legacy", "needs_officer", "held", "pending")}
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, a); err != nil {
				return nil, err
			}
			tx, err := c.Begin(ctx)
			if err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `CREATE TEMP TABLE capa_jur ON COMMIT DROP AS SELECT id, row_number() OVER (ORDER BY code) - 1 AS jx FROM jurisdictions WHERE code LIKE $1`, fmt.Sprintf("CAPA-%d-%%", sc.Seed)); err != nil {
				tx.Rollback(ctx)
				return nil, err
			}
			if _, err := tx.Exec(ctx, buf.String()); err != nil {
				tx.Rollback(ctx)
				return nil, fmt.Errorf("seed tenant %d: %w", tn, err)
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, err
			}
			totalPersons += g.Persons
		}
	}
	counts := map[string]any{}
	for _, t := range []string{"tenants", "persons", "player_accounts", "aml_screening_policies", "aml_screening_trigger_policies", "aml_screening_requests", "aml_screening_results", "aml_holds", "aml_enforcement_decisions"} {
		var cnt int64
		if err := c.QueryRow(ctx, "SELECT count(*) FROM "+t).Scan(&cnt); err != nil {
			return nil, err
		}
		counts[t] = cnt
	}
	// restore triggers and FORCE RLS, then verify against the catalog
	for _, t := range seedTables {
		if _, err := c.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ENABLE TRIGGER USER`, t)); err != nil {
			return nil, err
		}
		if _, err := c.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s FORCE ROW LEVEL SECURITY`, t)); err != nil {
			return nil, err
		}
	}
	var badTrig, badRLS int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM pg_trigger t JOIN pg_class r ON r.oid=t.tgrelid WHERE NOT t.tgisinternal AND t.tgenabled <> 'O' AND r.relnamespace='public'::regnamespace`).Scan(&badTrig); err != nil {
		return nil, err
	}
	if err := c.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relnamespace='public'::regnamespace AND relkind='r' AND relrowsecurity AND NOT relforcerowsecurity AND relname = ANY($1)`, seedTables).Scan(&badRLS); err != nil {
		return nil, err
	}
	if badTrig != 0 || badRLS != 0 {
		return nil, fmt.Errorf("seed left %d disabled triggers and %d unforced RLS tables", badTrig, badRLS)
	}
	for _, t := range seedTables {
		if _, err := c.Exec(ctx, "VACUUM (ANALYZE) "+t); err != nil {
			return nil, err
		}
	}
	counts["seed_seconds"] = time.Since(t0).Seconds()
	counts["persons_seeded"] = totalPersons
	return counts, nil
}
