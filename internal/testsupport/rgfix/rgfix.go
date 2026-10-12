//go:build integration

// Package rgfix is the TEST-ONLY RG configuration fixture of ADR 0115 section 6 (ledger-finance
// condition): it seeds a permissive SYNTHETIC-NOT-LEGAL jurisdiction floor for every control kind
// of every jurisdiction in the database (enabled=false, mandatory=false, ceiling none, a non-zero
// increase cooldown, every other field explicit), so that once S4 enforces "no floor in force =
// rg_config_missing" the existing money-path integration tests keep their semantics. Fail-closed
// behaviour is proven by dedicated tests that do NOT call it. S1 itself enforces nothing.
//
// The values are NOT legal values. Every row carries attestation_ref 'synthetic', which the
// database refuses (RL008) unless the owner-written database_environment anchor AND the session's
// app.environment are development / test; EnsureTestEnvironment records 'test' on a database whose
// anchor is still unset and refuses any database anchored otherwise. Floors are written through
// the REAL four-eyes flow (a change request, an approval by a different Person, the apply), each
// step with a signed actor proof from the per-process test key (prooftest); nothing is disabled.
//
// This package carries the `integration` build tag and lives under internal/testsupport, so it is
// never compiled into an application binary; internal/tenant/launch_fixture_pin_test.go pins that
// no non-test package imports it and CI checks the build tag. It must only ever be pointed at a
// synthetic development / CI database (CLAUDE.md "Environment safety").
package rgfix

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// ReasonCode is the reason recorded on every fixture row.
const ReasonCode = "rgfix_synthetic_not_legal"

// Spec is the content of one rg_limit_configs row, keyed by column name (nil = SQL NULL). It is the
// single source of both the payload hash (rg_limit_config_payload_hash) and the INSERT.
type Spec map[string]any

// contentColumns are the caller-supplied columns of rg_limit_configs (created_* are forced by the
// database; id defaults).
var contentColumns = []string{
	"jurisdiction_code", "tenant_id", "brand_id", "control_kind", "asset_code", "effective_from",
	"enabled", "mandatory", "ceiling", "ceiling_none", "allowed_periods", "calendar_tz", "week_start",
	"increase_cooldown", "increase_requires_reconfirmation", "scope", "count_bonus_funded",
	"loss_computation", "loss_may_go_negative", "loss_may_go_negative_none", "post_session_break",
	"post_session_break_none", "concurrent_chain_aggregation", "concurrent_chain_aggregation_none",
	"pending_intent_ttl", "pending_intent_ttl_none", "allowed_durations", "allowed_durations_any",
	"allowed_intervals", "reason_code", "attestation_ref",
}

// Clone returns a copy with overrides applied.
func (s Spec) Clone(over Spec) Spec {
	out := Spec{}
	for k, v := range s {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func (s Spec) json() ([]byte, error) {
	m := map[string]any{}
	for _, c := range contentColumns {
		m[c] = s[c]
	}
	return json.Marshal(m)
}

// PermissiveFloor is the SYNTHETIC-NOT-LEGAL permissive floor of ADR 0115 section 6 for one control
// kind (all assets; effective_from must be set by the caller).
func PermissiveFloor(jurisdiction, kind string) Spec {
	s := Spec{
		"jurisdiction_code": jurisdiction, "control_kind": kind, "enabled": false, "mandatory": false,
		"reason_code": ReasonCode, "attestation_ref": "synthetic",
	}
	limit := Spec{"ceiling_none": true, "increase_cooldown": "24:00:00", "increase_requires_reconfirmation": false, "scope": "account"}
	amount := Spec{"allowed_periods": []string{"rolling_24h", "rolling_7d", "rolling_30d", "calendar_day", "calendar_week", "calendar_month"},
		"calendar_tz": "UTC", "week_start": 1}
	switch kind {
	case "deposit":
		s = s.Clone(limit).Clone(amount).Clone(Spec{"pending_intent_ttl": "01:00:00", "pending_intent_ttl_none": false})
	case "loss":
		s = s.Clone(limit).Clone(amount).Clone(Spec{"count_bonus_funded": false, "loss_computation": "net_of_wins",
			"loss_may_go_negative": true, "loss_may_go_negative_none": false})
	case "wager":
		s = s.Clone(limit).Clone(amount).Clone(Spec{"count_bonus_funded": false})
	case "session_duration":
		s = s.Clone(limit).Clone(Spec{"post_session_break_none": true, "concurrent_chain_aggregation": false, "concurrent_chain_aggregation_none": false})
	case "reality_check_interval":
		s = s.Clone(Spec{"scope": "account", "allowed_intervals": []string{"00:15:00", "00:30:00", "01:00:00"}})
	case "time_out":
		s = s.Clone(Spec{"scope": "platform_person", "allowed_durations_any": true})
	default:
		panic("rgfix: unknown control kind " + kind)
	}
	return s
}

// ControlKinds is every kind rg_limit_configs admits.
var ControlKinds = []string{"deposit", "loss", "wager", "session_duration", "reality_check_interval", "time_out"}

// SetPlatform makes tx a plain platform-principal session (app.tenant_id cleared).
func SetPlatform(ctx context.Context, tx pgx.Tx, staff uuid.UUID) error {
	for _, kv := range [][2]string{{"app.tenant_id", ""}, {"app.principal_id", ""}, {"app.player_account_id", ""},
		{"app.platform_admin_principal_id", staff.String()}} {
		if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// SetEnvironmentGUC sets the session's app.environment (transaction-local).
func SetEnvironmentGUC(ctx context.Context, tx pgx.Tx, env string) error {
	_, err := tx.Exec(ctx, `SELECT set_config('app.environment', $1, true)`, env)
	return err
}

// PayloadHash is rg_limit_config_payload_hash of the spec, computed by the database function.
func PayloadHash(ctx context.Context, tx pgx.Tx, s Spec) (string, error) {
	b, err := s.json()
	if err != nil {
		return "", err
	}
	var h string
	err = tx.QueryRow(ctx, `SELECT rg_limit_config_payload_hash(jsonb_populate_record(NULL::rg_limit_configs, $1::jsonb))`, b).Scan(&h)
	return h, err
}

// InsertRow inserts the spec as one rg_limit_configs row in tx's CURRENT session (no proof is
// attached here) and returns its id.
func InsertRow(ctx context.Context, tx pgx.Tx, s Spec, changeRequestID *uuid.UUID) (uuid.UUID, error) {
	b, err := s.json()
	if err != nil {
		return uuid.Nil, err
	}
	cols := ""
	sel := ""
	for i, c := range contentColumns {
		if i > 0 {
			cols += ", "
			sel += ", "
		}
		cols += c
		if c == "effective_from" {
			sel += "COALESCE(r.effective_from, clock_timestamp())"
		} else {
			sel += "r." + c
		}
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO rg_limit_configs (`+cols+`, change_request_id)
		SELECT `+sel+`, $2 FROM jsonb_populate_record(NULL::rg_limit_configs, $1::jsonb) AS r RETURNING id`, b, changeRequestID).Scan(&id)
	return id, err
}

// FloorSteps lets a test run (or skip) the steps of the floor four-eyes flow individually.
type FloorSteps struct {
	Requester, Approver, Applier uuid.UUID
}

// DefaultSteps uses the launchfix fixture operators (distinct Persons).
func DefaultSteps() FloorSteps {
	o := launchfix.Operators()
	return FloorSteps{Requester: o.Requester, Approver: o.Approver1, Applier: o.Requester}
}

// RequestFloor files a change request for the spec as st.Requester (signed proof) and returns its
// id and the payload hash.
func RequestFloor(ctx context.Context, tx pgx.Tx, s Spec, st FloorSteps) (uuid.UUID, string, error) {
	if err := SetPlatform(ctx, tx, st.Requester); err != nil {
		return uuid.Nil, "", err
	}
	hash, err := PayloadHash(ctx, tx, s)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("rgfix: payload hash: %w", err)
	}
	req, _ := json.Marshal(map[string]any{"jurisdiction_code": s["jurisdiction_code"], "control_kind": s["control_kind"],
		"asset_code": s["asset_code"], "payload_hash": hash, "reason_code": s["reason_code"]})
	var digest string
	if err := tx.QueryRow(ctx, `SELECT rg_limit_config_request_digest(jsonb_populate_record(NULL::rg_limit_config_change_requests, $1::jsonb))`, req).Scan(&digest); err != nil {
		return uuid.Nil, "", err
	}
	if err := prooftest.AttachForSession(ctx, tx, actorproof.OpRGConfigRequest, actorproof.TargetNew, digest); err != nil {
		return uuid.Nil, "", err
	}
	proposed, err := s.json()
	if err != nil {
		return uuid.Nil, "", err
	}
	var id uuid.UUID
	var stored string
	err = tx.QueryRow(ctx, `INSERT INTO rg_limit_config_change_requests (jurisdiction_code, control_kind, asset_code, proposed_config, reason_code)
		VALUES ($1, $2, $3, $4::jsonb, $5) RETURNING id, payload_hash`, s["jurisdiction_code"], s["control_kind"], s["asset_code"], proposed, s["reason_code"]).Scan(&id, &stored)
	if err == nil && stored != hash {
		return uuid.Nil, "", fmt.Errorf("rgfix: the database derived payload hash %s, the fixture computed %s", stored, hash)
	}
	return id, hash, err
}

// Decide approves (or rejects / cancels) the request as staff (signed proof).
func Decide(ctx context.Context, tx pgx.Tx, reqID uuid.UUID, hash, status string, staff uuid.UUID) error {
	if err := SetPlatform(ctx, tx, staff); err != nil {
		return err
	}
	op := map[string]string{"approved": actorproof.OpRGConfigApprove, "rejected": actorproof.OpRGConfigReject, "cancelled": actorproof.OpRGConfigCancel}[status]
	if err := prooftest.AttachForSession(ctx, tx, op, reqID.String(), hash); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE rg_limit_config_change_requests SET status = $2 WHERE id = $1`, reqID, status)
	return err
}

// Apply inserts the floor row as staff with the apply proof bound to (request, hash).
func Apply(ctx context.Context, tx pgx.Tx, s Spec, reqID uuid.UUID, hash string, staff uuid.UUID) (uuid.UUID, error) {
	if err := SetPlatform(ctx, tx, staff); err != nil {
		return uuid.Nil, err
	}
	if err := prooftest.AttachForSession(ctx, tx, actorproof.OpRGConfigApply, reqID.String(), hash); err != nil {
		return uuid.Nil, err
	}
	return InsertRow(ctx, tx, s, &reqID)
}

// InsertFloor runs the complete four-eyes flow for the spec in tx (an OWNER or runtime connection
// whose actor-proof key is provisioned). effective_from defaults to the database clock.
func InsertFloor(ctx context.Context, tx pgx.Tx, s Spec, st FloorSteps) (uuid.UUID, error) {
	if s["effective_from"] == nil {
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return uuid.Nil, err
		}
		s = s.Clone(Spec{"effective_from": now.UTC().Format(time.RFC3339Nano)})
	}
	reqID, hash, err := RequestFloor(ctx, tx, s, st)
	if err != nil {
		return uuid.Nil, fmt.Errorf("rgfix: request: %w", err)
	}
	if err := Decide(ctx, tx, reqID, hash, "approved", st.Approver); err != nil {
		return uuid.Nil, fmt.Errorf("rgfix: approve: %w", err)
	}
	id, err := Apply(ctx, tx, s, reqID, hash, st.Applier)
	if err != nil {
		return uuid.Nil, fmt.Errorf("rgfix: apply: %w", err)
	}
	return id, nil
}

// EnsureTestEnvironment records database_environment = 'test' on a database whose anchor is unset
// (as the TABLE OWNER: ownerURL must be the owner role) and fails the test if the database is
// anchored as anything but development / test. It never changes an existing anchor.
func EnsureTestEnvironment(tb testing.TB, ownerURL string) {
	tb.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		tb.Fatalf("rgfix: connect owner: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, `INSERT INTO database_environment (environment) VALUES ('test') ON CONFLICT (singleton) DO NOTHING`); err != nil {
		tb.Fatalf("rgfix: record the test environment anchor: %v", err)
	}
	var env string
	if err := conn.QueryRow(ctx, `SELECT rg_database_environment()`).Scan(&env); err != nil {
		tb.Fatalf("rgfix: read the environment anchor: %v", err)
	}
	if env != "test" && env != "development" {
		tb.Fatalf("rgfix: refusing to seed synthetic RG configuration into a database anchored as %q", env)
	}
}

// SeedPermissiveFloorsAt seeds the permissive synthetic floor for every control kind of every
// jurisdiction at the owner URL that has no floor in force for that kind (all assets). Idempotent.
func SeedPermissiveFloorsAt(tb testing.TB, ownerURL string) {
	tb.Helper()
	EnsureTestEnvironment(tb, ownerURL)
	launchfix.SeedOperatorsAt(tb, ownerURL)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		tb.Fatalf("rgfix: connect owner: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := prooftest.EnsureKey(ctx, conn); err != nil {
		tb.Fatalf("rgfix: provision the actor-proof key: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := SetEnvironmentGUC(ctx, tx, "test"); err != nil {
		tb.Fatal(err)
	}
	st := DefaultSteps()
	if err := SetPlatform(ctx, tx, st.Requester); err != nil {
		tb.Fatal(err)
	}
	rows, err := tx.Query(ctx, `SELECT j.code, k.kind FROM jurisdictions j CROSS JOIN unnest($1::text[]) AS k(kind)
		WHERE NOT (rg_limit_config_compose(j.code, NULL, NULL, k.kind, NULL, clock_timestamp())).found
		ORDER BY j.code, k.kind`, ControlKinds)
	if err != nil {
		tb.Fatalf("rgfix: list missing floors: %v", err)
	}
	var missing [][2]string
	for rows.Next() {
		var j, k string
		if err := rows.Scan(&j, &k); err != nil {
			tb.Fatal(err)
		}
		missing = append(missing, [2]string{j, k})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		tb.Fatal(err)
	}
	for _, m := range missing {
		if _, err := InsertFloor(ctx, tx, PermissiveFloor(m[0], m[1]), st); err != nil {
			tb.Fatalf("rgfix: seed %s/%s: %v", m[0], m[1], err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		tb.Fatalf("rgfix: commit: %v", err)
	}
}

// SeedPermissiveFloors is SeedPermissiveFloorsAt against TEST_DATABASE_URL.
func SeedPermissiveFloors(tb testing.TB) {
	tb.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		tb.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	SeedPermissiveFloorsAt(tb, u)
}
