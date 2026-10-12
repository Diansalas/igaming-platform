//go:build integration

// ADR 0115 S1 (migration 0135) test world: ONE throwaway scratch database (internal/testsupport/
// scratchdb; no role, password or role attribute is touched - only this scratch database's object
// ACL, the brands_grant_pin pattern), migrated to head, with the runtime role igaming_runtime as the
// role of EVERY assertion. Requires TEST_DATABASE_URL, TEST_ADMIN_DATABASE_URL and
// TEST_RUNTIME_DATABASE_URL; a skip is NOT evidence.
package rg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

type s1World struct {
	ownerURL string
	owner    *db.Pool
	rt       *db.Pool
	ops      launchfix.OperatorIDs
	// samePersonPlatform is a second platform staff account of the requester's Person.
	samePersonPlatform uuid.UUID
}

// s1Fix is one isolated jurisdiction with two tenants licensed in it.
type s1Fix struct {
	jur                      string
	tenantA, tenantB         uuid.UUID
	brandA1, brandA2, brandB uuid.UUID
	staffA, staffB           uuid.UUID
	staffASelf               uuid.UUID // a tenant-A staff account of personA
	personA, personX         uuid.UUID
	acctA1, acctA2           uuid.UUID // personA, brands A1 / A2
	acctX                    uuid.UUID // personX, brand A1
	acctB                    uuid.UUID // personA, tenant B
}

func newS1World(t *testing.T) *s1World {
	t.Helper()
	rtBase := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if rtBase == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping ADR 0115 S1 runtime-role tests")
	}
	ctx := context.Background()
	scratch := scratchdb.New(t, "rg135_")
	owner, err := db.Connect(ctx, scratch, 8, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	su, _ := url.Parse(scratch)
	dbName := strings.TrimPrefix(su.Path, "/")
	for _, stmt := range []string{
		`GRANT CONNECT ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO igaming_runtime`,
		`GRANT USAGE ON SCHEMA public TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO igaming_runtime`,
	} {
		if _, err := owner.Raw().Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := owner.MigrateUp(ctx, "../../migrations"); err != nil {
		t.Fatalf("migrate scratch: %v", err)
	}
	prooftest.InstallVia(t, owner.Raw(), "rg135-"+dbName)
	launchfix.SeedOperatorsAt(t, scratch)
	ru, _ := url.Parse(rtBase)
	ru.Path = su.Path
	rt, err := db.Connect(ctx, ru.String(), 8, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	w := &s1World{ownerURL: scratch, owner: owner, rt: rt, ops: launchfix.Operators(), samePersonPlatform: uuid.New()}
	if err := owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			VALUES ($1, NULL, $2, '!x', 'platform_admin', $3)`, w.samePersonPlatform, "rg135-same-"+w.samePersonPlatform.String()+"@fixture.invalid", w.ops.Persons[0])
		return err
	}); err != nil {
		t.Fatalf("seed same-person platform staff: %v", err)
	}
	return w
}

func (w *s1World) newFix(t *testing.T) s1Fix {
	t.Helper()
	ctx := context.Background()
	f := s1Fix{jur: "RG-" + strings.ToUpper(uuid.NewString()[:8]), personA: uuid.New(), personX: uuid.New(),
		staffA: uuid.New(), staffB: uuid.New(), staffASelf: uuid.New(), acctA1: uuid.New(), acctA2: uuid.New(), acctX: uuid.New(), acctB: uuid.New()}
	staffPersonA, staffPersonB := uuid.New(), uuid.New()
	if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var jid, lic uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO jurisdictions (code, name) VALUES ($1, 'rg135 fixture') RETURNING id`, f.jur).Scan(&jid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO licences (jurisdiction_id, licensee, licence_number) VALUES ($1, 'platform', $2) RETURNING id`, jid, "RG135-"+f.jur).Scan(&lic); err != nil {
			return err
		}
		for _, p := range []uuid.UUID{f.personA, f.personX, staffPersonA, staffPersonB} {
			if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, p); err != nil {
				return err
			}
		}
		for i, tp := range []*uuid.UUID{&f.tenantA, &f.tenantB} {
			tn, err := launchfix.CreateTenant(ctx, tx, fmt.Sprintf("rg135 %d", i), "rg135-"+uuid.NewString()[:12], "under_platform_licence")
			if err != nil {
				return err
			}
			*tp = tn.ID
			if _, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lic, tn.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed jurisdiction / tenants: %v", err)
	}
	for _, s := range []struct {
		id, tenant, person uuid.UUID
	}{{f.staffA, f.tenantA, staffPersonA}, {f.staffB, f.tenantB, staffPersonB}, {f.staffASelf, f.tenantA, f.personA}} {
		if err := w.owner.WithTenant(ctx, s.tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
				VALUES ($1, $2, $3, '!x', 'tenant_admin', $4)`, s.id, s.tenant, "rg135-"+s.id.String()+"@fixture.invalid", s.person)
			return err
		}); err != nil {
			t.Fatalf("seed tenant staff: %v", err)
		}
	}
	for _, b := range []struct {
		tenant uuid.UUID
		brand  *uuid.UUID
		accts  [][2]uuid.UUID // account, person
	}{
		{f.tenantA, &f.brandA1, [][2]uuid.UUID{{f.acctA1, f.personA}, {f.acctX, f.personX}}},
		{f.tenantA, &f.brandA2, [][2]uuid.UUID{{f.acctA2, f.personA}}},
		{f.tenantB, &f.brandB, [][2]uuid.UUID{{f.acctB, f.personA}}},
	} {
		if err := w.owner.WithTenant(ctx, b.tenant, func(ctx context.Context, tx pgx.Tx) error {
			br, err := launchfix.CreateBrand(ctx, tx, b.tenant, "rg135", "rg135-b-"+uuid.NewString()[:12])
			if err != nil {
				return err
			}
			*b.brand = br.ID
			for _, a := range b.accts {
				if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
					VALUES ($1, $2, $3, $4, $5, 'x', 'active')`, a[0], b.tenant, br.ID, a[1], a[0].String()+"@rg135.invalid"); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("seed brands / accounts: %v", err)
		}
	}
	return f
}

// sess is the full set of session settings a transaction runs with (” = unset).
type sess struct {
	tenant, principal, platform, player string
	env                                 string
	iso                                 pgx.TxIsoLevel
}

func platformSess(staff uuid.UUID) sess { return sess{platform: staff.String()} }
func staffSess(tenant, staff uuid.UUID) sess {
	return sess{tenant: tenant.String(), principal: staff.String()}
}
func playerSess(tenant, acct uuid.UUID) sess {
	return sess{tenant: tenant.String(), player: acct.String()}
}

// do runs fn in ONE transaction on pool with exactly the session settings of s.
func do(pool *db.Pool, s sess, fn func(ctx context.Context, tx pgx.Tx) error) error {
	ctx := context.Background()
	tx, err := pool.Raw().BeginTx(ctx, pgx.TxOptions{IsoLevel: s.iso})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, kv := range [][2]string{{"app.tenant_id", s.tenant}, {"app.principal_id", s.principal},
		{"app.platform_admin_principal_id", s.platform}, {"app.player_account_id", s.player}, {"app.environment", s.env}} {
		if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// errRollback makes do() roll back a transaction whose work succeeded.
var errRollback = errors.New("rg135: rollback")

// num renders a decoded NUMERIC (nil -> "").
func num(v any) string {
	n, ok := v.(pgtype.Numeric)
	if !ok || !n.Valid {
		return ""
	}
	dv, err := n.Value()
	if err != nil {
		return ""
	}
	s, _ := dv.(string)
	return s
}

// durOf renders a decoded INTERVAL of fixed time.
func durOf(v any) time.Duration {
	iv, ok := v.(pgtype.Interval)
	if !ok || !iv.Valid {
		return -1
	}
	return time.Duration(iv.Microseconds)*time.Microsecond + time.Duration(iv.Days)*24*time.Hour
}

// toStrings renders a decoded text[].
func toStrings(v any) []string {
	var out []string
	if a, ok := v.([]any); ok {
		for _, x := range a {
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

func codeOf(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func wantCode(t *testing.T, what string, err error, code string) {
	t.Helper()
	if got := codeOf(err); got != code {
		t.Fatalf("%s: want SQLSTATE %s, got %q (%v)", what, code, got, err)
	}
}

func wantOK(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// floor writes a jurisdiction floor through the real four-eyes flow as the runtime role.
func (w *s1World) floor(t *testing.T, spec rgfix.Spec) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	wantOK(t, "floor", do(w.rt, sess{env: "production"}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = rgfix.InsertFloor(ctx, tx, spec, rgfix.DefaultSteps())
		return err
	}))
	return id
}

func (w *s1World) operator(tenant, staff uuid.UUID, spec rgfix.Spec) error {
	return do(w.rt, staffSess(tenant, staff), func(ctx context.Context, tx pgx.Tx) error {
		_, err := rgfix.InsertRow(ctx, tx, spec, nil)
		return err
	})
}

func strp(s string) *string { return &s }

func hex64() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// chg is one player_rg_limit_changes insert.
type chg struct {
	tenant, acct uuid.UUID
	scope, kind  string
	asset        *string
	period       *string
	value        *string
	assert       *string
	supersedes   *uuid.UUID
	source       string
	evidence     *string
	idem         string
	requestedAt  *time.Time
	effectiveAt  *time.Time
}

type chgRes struct {
	id                   uuid.UUID
	kind                 string
	requestedAt, effAt   time.Time
	requiresConfirmation bool
	value                *string
}

func insertChange(ctx context.Context, tx pgx.Tx, c chg) (chgRes, error) {
	if c.scope == "" {
		c.scope = "account"
	}
	if c.source == "" {
		c.source = "player_self_service"
	}
	if c.idem == "" {
		c.idem = uuid.NewString()
	}
	var r chgRes
	err := tx.QueryRow(ctx, `INSERT INTO player_rg_limit_changes
		(tenant_id, player_account_id, scope, control_kind, asset_code, period_kind, value, change_kind,
		 supersedes_change_id, source, reason_code, evidence_ref, idempotency_key, request_hash, requested_at, effective_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9, $10, 'rg135_test', $11, $12, $13, $14, $15)
		RETURNING id, change_kind, requested_at, effective_at, requires_confirmation, value::text`,
		c.tenant, c.acct, c.scope, c.kind, c.asset, c.period, c.value, c.assert, c.supersedes, c.source, c.evidence,
		c.idem, hex64(), c.requestedAt, c.effectiveAt,
	).Scan(&r.id, &r.kind, &r.requestedAt, &r.effAt, &r.requiresConfirmation, &r.value)
	return r, err
}

func (w *s1World) change(s sess, c chg) (chgRes, error) {
	var r chgRes
	err := do(w.rt, s, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = insertChange(ctx, tx, c)
		return err
	})
	return r, err
}

// realized reads the realization rule as the tenant's staff (runtime role).
func (w *s1World) realized(t *testing.T, f s1Fix, k LimitKey) Realized {
	t.Helper()
	var r Realized
	wantOK(t, "realized", do(w.rt, staffSess(k.TenantID, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = RealizedLimit(ctx, tx, k, nil)
		return err
	}))
	return r
}

// compose reads rg_limit_config_compose in session s (runtime role).
func (w *s1World) compose(t *testing.T, s sess, jur string, tenant, brand *uuid.UUID, kind string, asset *string) map[string]any {
	t.Helper()
	out := map[string]any{}
	wantOK(t, "compose", do(w.rt, s, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT * FROM rg_limit_config_compose($1, $2, $3, $4, $5, clock_timestamp())`, jur, tenant, brand, kind, asset)
		if err != nil {
			return err
		}
		defer rows.Close()
		if !rows.Next() {
			return errors.New("compose returned no row")
		}
		vals, err := rows.Values()
		if err != nil {
			return err
		}
		for i, fd := range rows.FieldDescriptions() {
			out[fd.Name] = vals[i]
		}
		return rows.Err()
	}))
	return out
}
