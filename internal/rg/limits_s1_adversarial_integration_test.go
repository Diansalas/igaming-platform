//go:build integration

// ADR 0115 S1 adversarial suite (migration 0135), every assertion as igaming_runtime on one scratch
// database: the environment anchor (a runtime session cannot forge a dev/test environment), floor
// four-eyes, no backdating, operator tighten-only / widen-only, read-time composition, the
// player-change trigger (server time, forged change_kind / effective_at, realization, cancel /
// confirm, staff rules, L0.4 serialisation, isolation guard), cross-tenant / RLS isolation,
// append-only, time_out, grants. Mutation-kill evidence:
// docs/plans/prh2-hardening-round/adr0115-s1-mutation-kill.txt.
package rg

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

const testAttestation = "rg135-test-attestation-not-legal"

// midFloor is a NON-synthetic test floor with mid values on every restrictive field, so both a
// loosening and a tightening exist for each (these are test values, not legal values).
func midFloor(jur, kind string) rgfix.Spec {
	s := rgfix.PermissiveFloor(jur, kind).Clone(rgfix.Spec{"attestation_ref": testAttestation, "reason_code": "rg135_test", "enabled": true})
	amount := rgfix.Spec{"asset_code": "EUR", "ceiling": "10000", "ceiling_none": false, "increase_cooldown": "02:00:00",
		"increase_requires_reconfirmation": true, "scope": "tenant_person", "mandatory": true,
		"allowed_periods": []string{"rolling_24h", "calendar_day"}}
	switch kind {
	case "deposit":
		return s.Clone(amount).Clone(rgfix.Spec{"pending_intent_ttl": "01:00:00", "pending_intent_ttl_none": false})
	case "wager":
		return s.Clone(amount).Clone(rgfix.Spec{"count_bonus_funded": true})
	case "loss":
		return s.Clone(amount).Clone(rgfix.Spec{"count_bonus_funded": true, "loss_may_go_negative": false, "loss_may_go_negative_none": false})
	case "session_duration":
		return s.Clone(rgfix.Spec{"ceiling": "3600", "ceiling_none": false, "increase_cooldown": "02:00:00", "increase_requires_reconfirmation": true,
			"scope": "tenant_person", "post_session_break": "01:00:00", "post_session_break_none": false,
			"concurrent_chain_aggregation": true, "concurrent_chain_aggregation_none": false})
	case "time_out":
		return s.Clone(rgfix.Spec{"allowed_durations_any": false, "allowed_durations": []string{"01:00:00", "24:00:00"}, "scope": "platform_person"})
	case "reality_check_interval":
		return s.Clone(rgfix.Spec{"allowed_intervals": []string{"00:30:00", "01:00:00"}})
	}
	panic(kind)
}

func TestRGLimitsS1(t *testing.T) {
	w := newS1World(t)
	ctx := context.Background()
	eur := strp("EUR")
	rolling := strp("rolling_24h")

	t.Run("EnvironmentAnchor_RuntimeCannotForgeDevOrTest", func(t *testing.T) {
		f := w.newFix(t)
		syn := rgfix.PermissiveFloor(f.jur, "wager")
		// No anchor row = production, whatever the session claims.
		var env string
		wantOK(t, "read anchor", do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT rg_database_environment()`).Scan(&env)
		}))
		if env != "production" {
			t.Fatalf("a database with no anchor must read as production, got %q", env)
		}
		for _, guc := range []string{"test", "development", ""} {
			err := do(w.rt, sess{env: guc}, func(ctx context.Context, tx pgx.Tx) error {
				_, err := rgfix.InsertFloor(ctx, tx, syn, rgfix.DefaultSteps())
				return err
			})
			wantCode(t, "synthetic floor with app.environment="+guc+" and no anchor", err, "RL008")
		}
		// The runtime role can never write the anchor (grant, RLS and the owner-only trigger).
		for _, stmt := range []string{
			`INSERT INTO database_environment (environment) VALUES ('test')`,
			`UPDATE database_environment SET environment = 'test'`,
			`DELETE FROM database_environment`,
			`TRUNCATE database_environment`,
		} {
			err := do(w.rt, sess{env: "test"}, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt); return err })
			wantCode(t, "runtime: "+stmt, err, "42501")
		}
		// Anchor 'staging' (owner, rolled back): still refused, even for the owner's own session.
		ownerTry := func(anchor, guc string) error {
			return do(w.owner, sess{env: guc}, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `INSERT INTO database_environment (environment) VALUES ($1)`, anchor); err != nil {
					return err
				}
				_, err := rgfix.InsertFloor(ctx, tx, syn, rgfix.DefaultSteps())
				if err != nil {
					return err
				}
				return errRollback
			})
		}
		wantCode(t, "anchor staging", ownerTry("staging", "test"), "RL008")
		wantCode(t, "anchor production", ownerTry("production", "test"), "RL008")
		wantCode(t, "anchor test, app.environment unset (missing = production)", ownerTry("test", ""), "RL008")
		wantCode(t, "anchor test, app.environment staging", ownerTry("test", "staging"), "RL008")
		if err := ownerTry("test", "test"); err != errRollback {
			t.Fatalf("anchor test + app.environment test must admit the synthetic row: %v", err)
		}
		// A near-synthetic attestation is refused even in a test environment.
		near := syn.Clone(rgfix.Spec{"attestation_ref": "Synthetic-v2"})
		err := do(w.owner, sess{env: "test"}, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO database_environment (environment) VALUES ('test')`); err != nil {
				return err
			}
			_, err := rgfix.InsertFloor(ctx, tx, near, rgfix.DefaultSteps())
			return err
		})
		wantCode(t, "an attestation resembling 'synthetic'", err, "RL008")
		// The anchor is write-once, even for the owner.
		err = do(w.owner, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO database_environment (environment) VALUES ('production')`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE database_environment SET environment = 'test'`)
			return err
		})
		wantCode(t, "owner UPDATE of the anchor", err, "RL009")
	})

	t.Run("FloorFourEyes", func(t *testing.T) {
		f := w.newFix(t)
		st := rgfix.DefaultSteps()
		spec := midFloor(f.jur, "wager").Clone(rgfix.Spec{"asset_code": "EUR"})
		now := func(tx pgx.Tx) string {
			var ts time.Time
			_ = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&ts)
			return ts.UTC().Format(time.RFC3339Nano)
		}
		// No request at all.
		err := do(w.rt, platformSess(st.Requester), func(ctx context.Context, tx pgx.Tx) error {
			_, err := rgfix.InsertRow(ctx, tx, spec.Clone(rgfix.Spec{"effective_from": now(tx)}), nil)
			return err
		})
		wantCode(t, "floor without a change request", err, "RL006")
		// Pending, rejected and cancelled requests are not usable; an approval by the requester's
		// own Person (a second staff account) is refused at the decision.
		for _, final := range []string{"", "rejected", "cancelled", "same_person"} {
			err := do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
				s := spec.Clone(rgfix.Spec{"effective_from": now(tx)})
				reqID, hash, err := rgfix.RequestFloor(ctx, tx, s, st)
				if err != nil {
					return err
				}
				switch final {
				case "rejected":
					if err := rgfix.Decide(ctx, tx, reqID, hash, "rejected", st.Approver); err != nil {
						return err
					}
				case "cancelled":
					if err := rgfix.Decide(ctx, tx, reqID, hash, "cancelled", st.Requester); err != nil {
						return err
					}
				case "same_person":
					return rgfix.Decide(ctx, tx, reqID, hash, "approved", w.samePersonPlatform)
				}
				_, err = rgfix.Apply(ctx, tx, s, reqID, hash, st.Applier)
				return err
			})
			want := "RL006"
			if final == "same_person" {
				want = "RL007"
			}
			wantCode(t, "floor with request state "+final, err, want)
		}
		// The request's payload_hash is derived by the database from the stored proposal: a caller
		// hash that differs, or a proposal outside the request's scope, is refused.
		for name, tc := range map[string]struct {
			proposal rgfix.Spec
			hash     string
		}{
			"forged payload_hash":   {spec, strings.Repeat("ab", 32)},
			"operator-row proposal": {spec.Clone(rgfix.Spec{"tenant_id": f.tenantA.String()}), ""},
			"other-kind proposal":   {spec.Clone(rgfix.Spec{"control_kind": "loss"}), ""},
			"other-reason proposal": {spec.Clone(rgfix.Spec{"reason_code": "different"}), ""},
			"other-asset proposal":  {spec.Clone(rgfix.Spec{"asset_code": "USD"}), ""},
		} {
			err := do(w.rt, platformSess(st.Requester), func(ctx context.Context, tx pgx.Tx) error {
				b, _ := json.Marshal(tc.proposal.Clone(rgfix.Spec{"effective_from": now(tx)}))
				var h any
				if tc.hash != "" {
					h = tc.hash
				}
				_, err := tx.Exec(ctx, `INSERT INTO rg_limit_config_change_requests (jurisdiction_code, control_kind, asset_code, proposed_config, payload_hash, reason_code)
					VALUES ($1, 'wager', 'EUR', $2::jsonb, $3, $4)`, f.jur, b, h, spec["reason_code"])
				return err
			})
			wantCode(t, name, err, "RL007")
		}
		// Filing and deciding without a signed proof are refused by the verifier (AP001).
		err = do(w.rt, platformSess(st.Requester), func(ctx context.Context, tx pgx.Tx) error {
			b, _ := json.Marshal(spec.Clone(rgfix.Spec{"effective_from": now(tx)}))
			_, err := tx.Exec(ctx, `INSERT INTO rg_limit_config_change_requests (jurisdiction_code, control_kind, asset_code, proposed_config, reason_code)
				VALUES ($1, 'wager', 'EUR', $2::jsonb, $3)`, f.jur, b, spec["reason_code"])
			return err
		})
		wantCode(t, "request without a proof", err, "AP001")
		err = do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			reqID, _, err := rgfix.RequestFloor(ctx, tx, spec.Clone(rgfix.Spec{"effective_from": now(tx)}), st)
			if err != nil {
				return err
			}
			if err := rgfix.SetPlatform(ctx, tx, st.Approver); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', '', true)`); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE rg_limit_config_change_requests SET status = 'approved' WHERE id = $1`, reqID)
			return err
		})
		wantCode(t, "approval without a proof", err, "AP001")
		// The approver deciding their own request by staff id is refused too.
		err = do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			reqID, hash, err := rgfix.RequestFloor(ctx, tx, spec.Clone(rgfix.Spec{"effective_from": now(tx)}), st)
			if err != nil {
				return err
			}
			return rgfix.Decide(ctx, tx, reqID, hash, "approved", st.Requester)
		})
		wantCode(t, "self-approval", err, "RL007")
		// Payload mismatch: approved for ceiling 10000, applied with 99999 (proof signed for the
		// approved hash) - the database recomputes the hash from the inserted row.
		err = do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			s := spec.Clone(rgfix.Spec{"effective_from": now(tx)})
			reqID, hash, err := rgfix.RequestFloor(ctx, tx, s, st)
			if err != nil {
				return err
			}
			if err := rgfix.Decide(ctx, tx, reqID, hash, "approved", st.Approver); err != nil {
				return err
			}
			_, err = rgfix.Apply(ctx, tx, s.Clone(rgfix.Spec{"ceiling": "99999"}), reqID, hash, st.Applier)
			return err
		})
		wantCode(t, "applied payload differs from the approved one", err, "RL007")
		// No proof attached at apply: the verifier refuses (AP001).
		err = do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			s := spec.Clone(rgfix.Spec{"effective_from": now(tx)})
			reqID, hash, err := rgfix.RequestFloor(ctx, tx, s, st)
			if err != nil {
				return err
			}
			if err := rgfix.Decide(ctx, tx, reqID, hash, "approved", st.Approver); err != nil {
				return err
			}
			if err := rgfix.SetPlatform(ctx, tx, st.Applier); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', '', true)`); err != nil {
				return err
			}
			_, err = rgfix.InsertRow(ctx, tx, s, &reqID)
			return err
		})
		wantCode(t, "apply without a proof", err, "AP001")
		// Happy path, then reuse of the consumed request.
		var reqUsed uuid.UUID
		err = do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			s := spec.Clone(rgfix.Spec{"effective_from": now(tx)})
			reqID, hash, err := rgfix.RequestFloor(ctx, tx, s, st)
			if err != nil {
				return err
			}
			if err := rgfix.Decide(ctx, tx, reqID, hash, "approved", st.Approver); err != nil {
				return err
			}
			reqUsed = reqID
			if _, err := rgfix.Apply(ctx, tx, s, reqID, hash, st.Applier); err != nil {
				return err
			}
			_, err = rgfix.Apply(ctx, tx, s, reqID, hash, st.Approver)
			return err
		})
		wantCode(t, "a request applied twice", err, "23505")
		_ = reqUsed
		wantOK(t, "happy path", do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := rgfix.InsertFloor(ctx, tx, spec, st)
			return err
		}))
		// A tenant principal can neither file a floor request nor write a floor row.
		err = do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO rg_limit_config_change_requests (jurisdiction_code, control_kind, payload_hash, reason_code)
				VALUES ($1, 'wager', repeat('a', 64), 'x')`, f.jur)
			return err
		})
		if c := codeOf(err); c != "42501" && c != "RL001" {
			t.Fatalf("tenant session filing a floor request: %v", err)
		}
		err = do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			_, err := rgfix.InsertRow(ctx, tx, spec.Clone(rgfix.Spec{"effective_from": now(tx)}), nil)
			return err
		})
		if c := codeOf(err); c != "42501" && c != "RL001" {
			t.Fatalf("tenant session writing a floor row: %v", err)
		}
	})

	t.Run("NoBackdating", func(t *testing.T) {
		f := w.newFix(t)
		past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
		err := do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := rgfix.InsertFloor(ctx, tx, midFloor(f.jur, "loss").Clone(rgfix.Spec{"effective_from": past}), rgfix.DefaultSteps())
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "RL003") {
			t.Fatalf("a backdated floor must be refused (RL003): %v", err)
		}
		w.floor(t, midFloor(f.jur, "loss"))
		op := midFloor(f.jur, "loss").Clone(rgfix.Spec{"tenant_id": f.tenantA.String(), "effective_from": past})
		wantCode(t, "a backdated operator row", w.operator(f.tenantA, f.staffA, op), "RL003")
		future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
		wantOK(t, "a future-dated operator row", w.operator(f.tenantA, f.staffA, op.Clone(rgfix.Spec{"effective_from": future})))
	})

	t.Run("OperatorTightenOnly_WidenOnly", func(t *testing.T) {
		f := w.newFix(t)
		for _, k := range rgfix.ControlKinds {
			if k == "wager" {
				continue
			}
			w.floor(t, midFloor(f.jur, k))
		}
		op := func(kind string, over rgfix.Spec) rgfix.Spec {
			return midFloor(f.jur, kind).Clone(rgfix.Spec{"tenant_id": f.tenantA.String(), "effective_from": nil}).Clone(over)
		}
		loosen := []struct {
			kind, field string
			over        rgfix.Spec
		}{
			{"deposit", "enabled", rgfix.Spec{"enabled": false}},
			{"deposit", "mandatory", rgfix.Spec{"mandatory": false}},
			{"deposit", "ceiling", rgfix.Spec{"ceiling": "10001"}},
			{"deposit", "ceiling", rgfix.Spec{"ceiling": nil, "ceiling_none": true}},
			{"deposit", "increase_cooldown", rgfix.Spec{"increase_cooldown": "01:59:59"}},
			{"deposit", "increase_requires_reconfirmation", rgfix.Spec{"increase_requires_reconfirmation": false}},
			{"deposit", "scope", rgfix.Spec{"scope": "account"}},
			{"deposit", "allowed_periods", rgfix.Spec{"allowed_periods": []string{"rolling_24h"}}},
			{"deposit", "allowed_periods", rgfix.Spec{"allowed_periods": []string{"rolling_24h", "rolling_7d"}}},
			{"deposit", "pending_intent_ttl", rgfix.Spec{"pending_intent_ttl": "00:59:59"}},
			{"loss", "count_bonus_funded", rgfix.Spec{"count_bonus_funded": false}},
			{"loss", "loss_may_go_negative", rgfix.Spec{"loss_may_go_negative": true}},
			{"session_duration", "post_session_break", rgfix.Spec{"post_session_break": "00:30:00"}},
			{"session_duration", "post_session_break", rgfix.Spec{"post_session_break": nil, "post_session_break_none": true}},
			{"session_duration", "concurrent_chain_aggregation", rgfix.Spec{"concurrent_chain_aggregation": false}},
			{"session_duration", "concurrent_chain_aggregation", rgfix.Spec{"concurrent_chain_aggregation": nil, "concurrent_chain_aggregation_none": true}},
			{"session_duration", "ceiling", rgfix.Spec{"ceiling": "3601"}},
			{"time_out", "allowed_durations", rgfix.Spec{"allowed_durations": []string{"01:00:00"}}},
			{"time_out", "scope", rgfix.Spec{"scope": "tenant_person"}},
			{"time_out", "enabled", rgfix.Spec{"enabled": false}},
			{"reality_check_interval", "allowed_intervals", rgfix.Spec{"allowed_intervals": []string{"00:30:00"}}},
		}
		for _, c := range loosen {
			err := w.operator(f.tenantA, f.staffA, op(c.kind, c.over))
			wantCode(t, c.kind+" loosening "+c.field, err, "RL004")
			if !strings.Contains(err.Error(), c.field) {
				t.Fatalf("%s: the refusal must name %s: %v", c.kind, c.field, err)
			}
		}
		tighten := []struct {
			kind string
			over rgfix.Spec
		}{
			{"deposit", rgfix.Spec{"ceiling": "9999", "increase_cooldown": "03:00:00", "allowed_periods": []string{"rolling_24h", "calendar_day", "rolling_7d"},
				"pending_intent_ttl": nil, "pending_intent_ttl_none": true}},
			{"loss", rgfix.Spec{"ceiling": "5000"}},
			{"session_duration", rgfix.Spec{"post_session_break": "02:00:00", "ceiling": "1800"}},
			{"time_out", rgfix.Spec{"allowed_durations": nil, "allowed_durations_any": true}},
			{"reality_check_interval", rgfix.Spec{"allowed_intervals": []string{"00:15:00", "00:30:00", "01:00:00"}}},
		}
		for _, c := range tighten {
			wantOK(t, c.kind+" tightening / widening", w.operator(f.tenantA, f.staffA, op(c.kind, c.over)))
		}
		// A brand row is bounded by its tenant's row too (deposit tenant row: ceiling 9999).
		brand := op("deposit", rgfix.Spec{"brand_id": f.brandA1.String(), "ceiling": "10000", "increase_cooldown": "03:00:00",
			"allowed_periods": []string{"rolling_24h", "calendar_day", "rolling_7d"}, "pending_intent_ttl": nil, "pending_intent_ttl_none": true})
		wantCode(t, "brand row looser than its tenant row", w.operator(f.tenantA, f.staffA, brand), "RL004")
		wantOK(t, "brand row within its tenant row", w.operator(f.tenantA, f.staffA, brand.Clone(rgfix.Spec{"ceiling": "9000"})))
		// Nothing to tighten: no wager floor in force.
		wantCode(t, "operator row with no floor", w.operator(f.tenantA, f.staffA, op("wager", nil)), "RL005")
		// Another tenant's principal cannot write tenant A's row.
		err := w.operator(f.tenantB, f.staffB, op("deposit", rgfix.Spec{"ceiling": "100"}))
		if c := codeOf(err); c != "42501" && c != "RL001" {
			t.Fatalf("cross-tenant operator row: %v", err)
		}
	})

	t.Run("ReadTimeComposition_StrictestUnionOr", func(t *testing.T) {
		f := w.newFix(t)
		// F1: permissive-ish floor; the operator row tightens F1; F2 (later) tightens beyond the
		// operator row in some fields and widens options in others. Composition must keep the
		// strictest value of every restrictive field, the UNION of option sets and OR of flags -
		// even though the operator row is now looser than F2 in places (no trigger re-checks it).
		f1 := midFloor(f.jur, "deposit").Clone(rgfix.Spec{"enabled": false, "mandatory": false, "ceiling": nil, "ceiling_none": true,
			"increase_cooldown": "01:00:00", "increase_requires_reconfirmation": false, "scope": "account",
			"allowed_periods": []string{"rolling_24h"}})
		w.floor(t, f1)
		opRow := f1.Clone(rgfix.Spec{"tenant_id": f.tenantA.String(), "effective_from": nil, "ceiling": "500", "ceiling_none": false,
			"allowed_periods": []string{"rolling_24h", "rolling_7d"}, "increase_cooldown": "05:00:00"})
		wantOK(t, "operator row", w.operator(f.tenantA, f.staffA, opRow))
		time.Sleep(10 * time.Millisecond)
		w.floor(t, f1.Clone(rgfix.Spec{"enabled": true, "mandatory": true, "ceiling": "300", "ceiling_none": false,
			"allowed_periods": []string{"rolling_24h", "calendar_day"}, "increase_requires_reconfirmation": true, "scope": "tenant_person"}))
		c := w.compose(t, staffSess(f.tenantA, f.staffA), f.jur, &f.tenantA, &f.brandA1, "deposit", strp("EUR"))
		if c["found"] != true || c["enabled"] != true || c["mandatory"] != true || c["increase_requires_reconfirmation"] != true || c["scope"] != "tenant_person" {
			t.Fatalf("OR / strictest flags: %v", c)
		}
		if got := num(c["ceiling"]); got != "300" || c["ceiling_none"] != false {
			t.Fatalf("ceiling must be MIN(300, 500): %v", c)
		}
		if got := strings.Join(toStrings(c["allowed_periods"]), ","); got != "calendar_day,rolling_24h,rolling_7d" {
			t.Fatalf("periods must be the UNION: %s", got)
		}
		if got := c["increase_cooldown"]; durOf(got) != 5*time.Hour {
			t.Fatalf("cooldown must be MAX: %v", got)
		}
		// A brand row binds only its own brand.
		wantOK(t, "brand A2 row", w.operator(f.tenantA, f.staffA, opRow.Clone(rgfix.Spec{"brand_id": f.brandA2.String(), "ceiling": "100",
			"enabled": true, "mandatory": true, "allowed_periods": []string{"rolling_24h", "rolling_7d", "calendar_day"},
			"increase_requires_reconfirmation": true, "scope": "tenant_person"})))
		if c := w.compose(t, staffSess(f.tenantA, f.staffA), f.jur, &f.tenantA, &f.brandA1, "deposit", strp("EUR")); num(c["ceiling"]) != "300" {
			t.Fatalf("brand A1 must not be bound by brand A2's row: %v", c)
		}
		if c := w.compose(t, staffSess(f.tenantA, f.staffA), f.jur, &f.tenantA, &f.brandA2, "deposit", strp("EUR")); num(c["ceiling"]) != "100" {
			t.Fatalf("brand A2 must be bound by its own row: %v", c)
		}
		// Direct reads: operator rows never leave their tenant; floor change requests are platform-only.
		var foreignRows, reqRows int
		wantOK(t, "tenant B direct read", do(w.rt, staffSess(f.tenantB, f.staffB), func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_configs WHERE tenant_id = $1`, f.tenantA).Scan(&foreignRows); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_config_change_requests`).Scan(&reqRows)
		}))
		if foreignRows != 0 || reqRows != 0 {
			t.Fatalf("tenant B read %d of tenant A's operator rows and %d floor change requests", foreignRows, reqRows)
		}
		var anon, plat int
		wantOK(t, "principal-less read", do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_config_change_requests`).Scan(&anon)
		}))
		wantOK(t, "platform read", do(w.rt, platformSess(w.ops.Requester), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_config_change_requests`).Scan(&plat)
		}))
		if anon != 0 || plat == 0 {
			t.Fatalf("floor change requests need a platform principal: principal-less=%d platform=%d", anon, plat)
		}
		// Tenant B sees only the floor (the operator row never leaves tenant A).
		cb := w.compose(t, staffSess(f.tenantB, f.staffB), f.jur, &f.tenantB, &f.brandB, "deposit", strp("EUR"))
		if num(cb["ceiling"]) != "300" || durOf(cb["increase_cooldown"]) != time.Hour {
			t.Fatalf("tenant B composition must not include tenant A's row: %v", cb)
		}
		// No floor in force for a kind = not found (rg_config_missing).
		if c := w.compose(t, staffSess(f.tenantA, f.staffA), f.jur, &f.tenantA, &f.brandA1, "loss", strp("EUR")); c["found"] != false {
			t.Fatalf("a kind with no floor must compose as not found: %v", c)
		}
	})

	t.Run("PlayerChanges_ServerTimeClassificationRealization", func(t *testing.T) {
		f := w.newFix(t)
		cool := midFloor(f.jur, "deposit").Clone(rgfix.Spec{"increase_cooldown": "00:00:02", "increase_requires_reconfirmation": false, "scope": "account", "mandatory": false})
		w.floor(t, cool)
		w.floor(t, midFloor(f.jur, "wager").Clone(rgfix.Spec{"asset_code": "EUR", "ceiling": "10000", "ceiling_none": false,
			"increase_cooldown": "00:00:01", "increase_requires_reconfirmation": true}))
		pa := playerSess(f.tenantA, f.acctA1)
		dep := func(v *string) chg {
			return chg{tenant: f.tenantA, acct: f.acctA1, kind: "deposit", asset: eur, period: rolling, value: v}
		}
		key := LimitKey{TenantID: f.tenantA, PlayerAccountID: f.acctA1, PersonID: f.personA, Scope: LimitScopeAccount, Kind: ControlDeposit,
			AssetCode: eur, Period: func() *PeriodKind { p := PeriodRolling24h; return &p }()}

		r, err := w.change(pa, dep(strp("1000")))
		wantOK(t, "initial", err)
		if r.kind != "initial" || !r.effAt.Equal(r.requestedAt) || r.requiresConfirmation {
			t.Fatalf("initial: %+v", r)
		}
		if time.Since(r.requestedAt) > time.Minute || time.Since(r.requestedAt) < -time.Minute {
			t.Fatalf("requested_at must be the database clock: %v", r.requestedAt)
		}
		// Boundary of the realization rule: realized AT its effective_at (<=), not before.
		at := r.effAt
		before := r.effAt.Add(-time.Microsecond)
		wantOK(t, "realized at the boundary", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			got, err := RealizedLimit(ctx, tx, key, &at)
			if err != nil {
				return err
			}
			if !got.HasRow || got.Value == nil || *got.Value != "1000" {
				t.Errorf("a row is realized at exactly its effective_at: %+v", got)
			}
			got, err = RealizedLimit(ctx, tx, key, &before)
			if err != nil {
				return err
			}
			if got.HasRow {
				t.Errorf("nothing is realized before the first effective_at: %+v", got)
			}
			return nil
		}))
		r, err = w.change(pa, dep(strp("500")))
		wantOK(t, "decrease", err)
		if r.kind != "decrease" || !r.effAt.Equal(r.requestedAt) {
			t.Fatalf("decrease: %+v", r)
		}
		// Forged classification / client time.
		_, err = w.change(pa, chg{tenant: f.tenantA, acct: f.acctA1, kind: "deposit", asset: eur, period: rolling, value: strp("900"), assert: strp("decrease")})
		wantCode(t, "an increase asserted as a decrease", err, "RL020")
		past := time.Now().Add(-time.Hour)
		c := dep(strp("400"))
		c.effectiveAt = &past
		_, err = w.change(pa, c)
		wantCode(t, "client effective_at", err, "RL020")
		c = dep(strp("400"))
		c.requestedAt = &past
		_, err = w.change(pa, c)
		wantCode(t, "client requested_at", err, "RL020")
		_, err = w.change(pa, dep(strp("500")))
		wantCode(t, "no change", err, "RL021")
		// Increase: effective only after the cooldown (exactly requested_at + 2 s).
		inc, err := w.change(pa, dep(strp("800")))
		wantOK(t, "increase", err)
		if inc.kind != "increase" || inc.effAt.Sub(inc.requestedAt) != 2*time.Second {
			t.Fatalf("increase: %+v", inc)
		}
		if got := w.realized(t, f, key); got.Value == nil || *got.Value != "500" {
			t.Fatalf("before the cooldown the limit stays 500: %+v", got)
		}
		// A decrease before the cooldown elapses supersedes the pending increase for ever.
		dec, err := w.change(pa, dep(strp("600")))
		wantOK(t, "600 vs 500 in force", err)
		if dec.kind != "increase" {
			t.Fatalf("600 vs 500 must classify as increase: %+v", dec)
		}
		_, err = w.change(pa, dep(strp("450")))
		wantOK(t, "decrease supersedes", err)
		time.Sleep(2200 * time.Millisecond)
		if got := w.realized(t, f, key); got.Value == nil || *got.Value != "450" {
			t.Fatalf("superseded increases are never realized: %+v", got)
		}
		// cancel_pending: an increase cancelled stays unrealized.
		inc, err = w.change(pa, dep(strp("700")))
		wantOK(t, "increase 2", err)
		_, err = w.change(pa, chg{tenant: f.tenantA, acct: f.acctA1, kind: "deposit", asset: eur, period: rolling, supersedes: &dec.id, assert: strp("cancel_pending")})
		wantCode(t, "cancel of a request that is not the latest", err, "RL022")
		cn, err := w.change(pa, chg{tenant: f.tenantA, acct: f.acctA1, kind: "deposit", asset: eur, period: rolling, supersedes: &inc.id, assert: strp("cancel_pending")})
		wantOK(t, "cancel_pending", err)
		if cn.kind != "cancel_pending" || cn.value == nil || *cn.value != "450" {
			t.Fatalf("cancel_pending carries the limit in force: %+v", cn)
		}
		time.Sleep(2200 * time.Millisecond)
		if got := w.realized(t, f, key); got.Value == nil || *got.Value != "450" {
			t.Fatalf("a cancelled increase is never realized: %+v", got)
		}
		// Two pending increases: the first is superseded by the second BEFORE its own effective_at,
		// so it is never realized even after its cooldown elapses (the next-request bound).
		_, err = w.change(pa, dep(strp("650")))
		wantOK(t, "pending increase A", err)
		time.Sleep(1000 * time.Millisecond)
		_, err = w.change(pa, dep(strp("680")))
		wantOK(t, "pending increase B", err)
		time.Sleep(1300 * time.Millisecond)
		if got := w.realized(t, f, key); got.Value == nil || *got.Value != "450" {
			t.Fatalf("an increase superseded before its effective_at is never realized: %+v", got)
		}
		// Removal after the cooldown, realized as "no limit".
		rm, err := w.change(pa, dep(nil))
		wantOK(t, "removal", err)
		if rm.kind != "removal" || rm.effAt.Sub(rm.requestedAt) != 2*time.Second {
			t.Fatalf("removal: %+v", rm)
		}
		time.Sleep(2200 * time.Millisecond)
		if got := w.realized(t, f, key); !got.HasRow || got.Value != nil {
			t.Fatalf("a realized removal means no limit: %+v", got)
		}
		_, err = w.change(pa, dep(nil))
		wantCode(t, "removing a removed limit", err, "RL021")
		// Go's preview calls the same SQL function and agrees.
		wantOK(t, "preview", do(w.rt, pa, func(ctx context.Context, tx pgx.Tx) error {
			cl, err := ClassifyLimitChange(ctx, tx, ClassifyParams{Key: key, BrandID: f.brandA1, JurisdictionCode: &f.jur, Value: strp("10"), Source: ChangeSourcePlayerSelfService})
			if err != nil {
				return err
			}
			if cl.ChangeKind != ChangeInitial {
				t.Errorf("preview after removal must be initial: %+v", cl)
			}
			return nil
		}))

		// Reconfirmation (wager): an increase is never realized itself; confirm only after the
		// cooldown and only from the player's own session.
		wg := func(v *string) chg {
			return chg{tenant: f.tenantA, acct: f.acctA1, kind: "wager", asset: eur, period: rolling, value: v}
		}
		wkey := key
		wkey.Kind = ControlWager
		_, err = w.change(pa, wg(strp("100")))
		wantOK(t, "wager initial", err)
		winc, err := w.change(pa, wg(strp("300")))
		wantOK(t, "wager increase", err)
		if !winc.requiresConfirmation {
			t.Fatalf("reconfirmation config must mark the increase: %+v", winc)
		}
		confirm := chg{tenant: f.tenantA, acct: f.acctA1, kind: "wager", asset: eur, period: rolling, supersedes: &winc.id, assert: strp("confirm")}
		_, err = w.change(pa, confirm)
		wantCode(t, "confirm before the cooldown", err, "RL022")
		time.Sleep(1200 * time.Millisecond)
		if got := w.realized(t, f, wkey); got.Value == nil || *got.Value != "100" {
			t.Fatalf("an unconfirmed increase is never realized: %+v", got)
		}
		sc := confirm
		sc.source, sc.evidence = "staff_on_player_request", strp("ticket-1")
		_, err = w.change(staffSess(f.tenantA, f.staffA), sc)
		if c := codeOf(err); c != "RL022" && c != "RL025" {
			t.Fatalf("a staff confirm must be refused: %v", err)
		}
		cf, err := w.change(pa, confirm)
		wantOK(t, "player confirm", err)
		if cf.kind != "confirm" || cf.value == nil || *cf.value != "300" {
			t.Fatalf("confirm carries the increased value: %+v", cf)
		}
		if got := w.realized(t, f, wkey); got.Value == nil || *got.Value != "300" {
			t.Fatalf("a confirmed increase is realized: %+v", got)
		}
	})

	t.Run("PlayerChanges_StaffScopeConfigAndIsolation", func(t *testing.T) {
		f := w.newFix(t)
		w.floor(t, midFloor(f.jur, "deposit").Clone(rgfix.Spec{"increase_cooldown": "01:00:00", "increase_requires_reconfirmation": false, "scope": "account", "mandatory": false}))
		ss := staffSess(f.tenantA, f.staffA)
		dep := func(acct uuid.UUID, v string) chg {
			return chg{tenant: f.tenantA, acct: acct, kind: "deposit", asset: eur, period: rolling, value: strp(v),
				source: "staff_on_player_request", evidence: strp("ticket-7")}
		}
		_, err := w.change(ss, dep(f.acctX, "100"))
		wantOK(t, "staff initial on player request", err)
		r, err := w.change(ss, dep(f.acctX, "200"))
		wantOK(t, "staff increase", err)
		if !r.requiresConfirmation {
			t.Fatalf("a staff-originated increase always needs the player's confirm (security condition 4): %+v", r)
		}
		noEv := dep(f.acctX, "50")
		noEv.evidence = nil
		_, err = w.change(ss, noEv)
		wantCode(t, "staff change without evidence", err, "23514")
		_, err = w.change(staffSess(f.tenantA, f.staffASelf), dep(f.acctA1, "100"))
		wantCode(t, "staff acting on their own Person", err, "RL025")
		// A player writes only their own account, in their own tenant.
		_, err = w.change(playerSess(f.tenantA, f.acctA1), chg{tenant: f.tenantA, acct: f.acctX, kind: "deposit", asset: eur, period: rolling, value: strp("5")})
		wantCode(t, "player writing another account", err, "RL025")
		_, err = w.change(playerSess(f.tenantA, f.acctA1), chg{tenant: f.tenantB, acct: f.acctA1, kind: "deposit", asset: eur, period: rolling, value: strp("5")})
		wantCode(t, "player naming another tenant", err, "RL025")
		_, err = w.change(staffSess(f.tenantB, f.staffB), chg{tenant: f.tenantA, acct: f.acctX, kind: "deposit", asset: eur, period: rolling,
			value: strp("5"), source: "staff_on_player_request", evidence: strp("t")})
		wantCode(t, "tenant B staff writing tenant A", err, "RL025")
		// Isolation guard.
		rr := playerSess(f.tenantA, f.acctA1)
		rr.iso = pgx.RepeatableRead
		_, err = w.change(rr, chg{tenant: f.tenantA, acct: f.acctA1, kind: "deposit", asset: eur, period: rolling, value: strp("5")})
		wantCode(t, "REPEATABLE READ", err, "RL024")
		// Increase / removal with no configuration in force; decrease / initial need none.
		_, err = w.change(playerSess(f.tenantA, f.acctA1), chg{tenant: f.tenantA, acct: f.acctA1, kind: "loss", asset: eur, period: rolling, value: strp("100")})
		wantOK(t, "initial loss limit without configuration (protective)", err)
		_, err = w.change(playerSess(f.tenantA, f.acctA1), chg{tenant: f.tenantA, acct: f.acctA1, kind: "loss", asset: eur, period: rolling, value: strp("50")})
		wantOK(t, "decrease without configuration (protective)", err)
		_, err = w.change(playerSess(f.tenantA, f.acctA1), chg{tenant: f.tenantA, acct: f.acctA1, kind: "loss", asset: eur, period: rolling, value: strp("80")})
		wantCode(t, "increase without configuration", err, "RL023")
		// tenant_person: a limit set from account A1 binds the Person's account A2 (same tenant).
		tp := func(acct uuid.UUID, v string) chg {
			return chg{tenant: f.tenantA, acct: acct, scope: "tenant_person", kind: "deposit", asset: eur, period: rolling, value: strp(v)}
		}
		_, err = w.change(playerSess(f.tenantA, f.acctA1), tp(f.acctA1, "1000"))
		wantOK(t, "tenant_person initial", err)
		r, err = w.change(playerSess(f.tenantA, f.acctA2), tp(f.acctA2, "1500"))
		wantOK(t, "tenant_person from the linked account", err)
		if r.kind != "increase" {
			t.Fatalf("a linked account's higher value is an INCREASE of the Person's limit, not an initial value: %+v", r)
		}
		// Idempotency key reuse.
		k := tp(f.acctA1, "900")
		k.idem = "rg135-idem-1"
		_, err = w.change(playerSess(f.tenantA, f.acctA1), k)
		wantOK(t, "first use of a key", err)
		k.value = strp("800")
		_, err = w.change(playerSess(f.tenantA, f.acctA1), k)
		wantCode(t, "idempotency key reuse", err, "23505")

		// Limit history never leaves its tenant; a player sees only their own rows.
		count := func(s sess) int {
			var n int
			wantOK(t, "count", do(w.rt, s, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM player_rg_limit_changes WHERE tenant_id = $1`, f.tenantA).Scan(&n)
			}))
			return n
		}
		all := count(staffSess(f.tenantA, f.staffA))
		if all != 7 {
			t.Fatalf("tenant A staff must see the tenant's history, saw %d", all)
		}
		if n := count(staffSess(f.tenantB, f.staffB)); n != 0 {
			t.Fatalf("tenant B saw %d of tenant A's rows", n)
		}
		if n := count(platformSess(w.ops.Requester)); n != 0 {
			t.Fatalf("a platform session saw %d rows (no platform-wide read)", n)
		}
		if n := count(playerSess(f.tenantA, f.acctX)); n == 0 || n >= all {
			t.Fatalf("player X must see exactly their own rows, saw %d of %d", n, all)
		}
		var foreign int
		wantOK(t, "player X foreign rows", do(w.rt, playerSess(f.tenantA, f.acctX), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM player_rg_limit_changes WHERE person_id <> $1`, f.personX).Scan(&foreign)
		}))
		if foreign != 0 {
			t.Fatalf("player X saw %d rows of another Person", foreign)
		}
		var linked int
		wantOK(t, "linked account sees tenant_person rows", do(w.rt, playerSess(f.tenantA, f.acctA2), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM player_rg_limit_changes WHERE scope = 'tenant_person' AND player_account_id = $1`, f.acctA1).Scan(&linked)
		}))
		if linked == 0 {
			t.Fatal("a linked account must see its Person's tenant_person rows written from the other account")
		}
	})

	t.Run("PlayerChanges_L04SerialisesClassification", func(t *testing.T) {
		f := w.newFix(t)
		w.floor(t, midFloor(f.jur, "deposit").Clone(rgfix.Spec{"increase_cooldown": "01:00:00", "increase_requires_reconfirmation": false, "scope": "account", "mandatory": false}))
		pa := playerSess(f.tenantA, f.acctA1)
		dep := func(v string) chg {
			return chg{tenant: f.tenantA, acct: f.acctA1, kind: "deposit", asset: eur, period: rolling, value: strp(v)}
		}
		_, err := w.change(pa, dep("1000"))
		wantOK(t, "initial", err)
		// tx1 decreases to 500 and holds; tx2 asks for 800 meanwhile. Without serialisation tx2 would
		// classify 800 against 1000 as an immediate DECREASE and loosen 500 -> 800 with no cooldown.
		hold := make(chan struct{})
		inserted := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = do(w.rt, pa, func(ctx context.Context, tx pgx.Tx) error {
				_, err := insertChange(ctx, tx, dep("500"))
				inserted <- err
				<-hold
				return err
			})
		}()
		if err := <-inserted; err != nil {
			t.Fatal(err)
		}
		res := make(chan chgRes, 1)
		errc := make(chan error, 1)
		go func() {
			r, err := w.change(pa, dep("800"))
			res <- r
			errc <- err
		}()
		// tx2 must be waiting on the advisory lock.
		deadline := time.Now().Add(5 * time.Second)
		for {
			var waiting int
			_ = w.owner.Raw().QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting)
			if waiting > 0 {
				break
			}
			if time.Now().After(deadline) {
				close(hold)
				wg.Wait()
				t.Fatal("the second change did not wait on L0.4")
			}
			time.Sleep(20 * time.Millisecond)
		}
		close(hold)
		wg.Wait()
		r, err := <-res, <-errc
		wantOK(t, "second change", err)
		if r.kind != "increase" {
			t.Fatalf("after the lock the second change must see 500 and classify 800 as an increase, got %+v", r)
		}
	})

	t.Run("AppendOnly", func(t *testing.T) {
		f := w.newFix(t)
		w.floor(t, midFloor(f.jur, "deposit"))
		_, err := w.change(playerSess(f.tenantA, f.acctA1), chg{tenant: f.tenantA, acct: f.acctA1, kind: "deposit", asset: eur, period: rolling, value: strp("10")})
		wantOK(t, "seed change", err)
		for _, stmt := range []string{
			`UPDATE rg_limit_configs SET reason_code = 'x'`, `DELETE FROM rg_limit_configs`, `TRUNCATE rg_limit_configs`,
			`UPDATE player_rg_limit_changes SET value = 1`, `DELETE FROM player_rg_limit_changes`, `TRUNCATE player_rg_limit_changes`,
			`DELETE FROM rg_limit_config_change_requests`, `TRUNCATE rg_limit_config_change_requests`,
		} {
			// The runtime role holds no UPDATE / DELETE / TRUNCATE privilege at all.
			err := do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, stmt)
				return err
			})
			wantCode(t, "runtime: "+stmt, err, "42501")
			if strings.HasPrefix(stmt, "TRUNCATE") {
				err = do(w.owner, platformSess(w.ops.Requester), func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, stmt+" CASCADE")
					return err
				})
				wantCode(t, "owner: "+stmt, err, "RL009")
			}
		}
		// The owner (FORCE RLS): there is no UPDATE / DELETE policy, so nothing is even visible to a
		// mutation; the deny triggers are the second, independent layer (exercised below with the
		// FORCE lifted inside a rolled-back transaction). Either way no row changes.
		for _, m := range []struct {
			s    sess
			stmt string
			arg  any
		}{
			{platformSess(w.ops.Requester), `DELETE FROM rg_limit_configs WHERE jurisdiction_code = $1`, f.jur},
			{platformSess(w.ops.Requester), `UPDATE rg_limit_configs SET reason_code = 'x' WHERE jurisdiction_code = $1`, f.jur},
			{staffSess(f.tenantA, f.staffA), `DELETE FROM player_rg_limit_changes WHERE tenant_id = $1`, f.tenantA},
			{staffSess(f.tenantA, f.staffA), `UPDATE player_rg_limit_changes SET reason_code = 'x' WHERE tenant_id = $1`, f.tenantA},
		} {
			var affected int64 = -1
			err := do(w.owner, m.s, func(ctx context.Context, tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, m.stmt, m.arg)
				affected = tag.RowsAffected()
				return err
			})
			if !(codeOf(err) == "RL009" || (err == nil && affected == 0)) {
				t.Fatalf("owner %s: must change nothing (err=%v affected=%d)", m.stmt, err, affected)
			}
			// With FORCE RLS lifted (owner bypass, rolled back): the deny trigger refuses.
			tbl := "rg_limit_configs"
			if strings.Contains(m.stmt, "player_rg_limit_changes") {
				tbl = "player_rg_limit_changes"
			}
			err = do(w.owner, m.s, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `ALTER TABLE `+tbl+` NO FORCE ROW LEVEL SECURITY`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, m.stmt, m.arg)
				return err
			})
			wantCode(t, "owner without FORCE RLS: "+m.stmt, err, "RL009")
		}
		err = do(w.owner, platformSess(w.ops.Requester), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE rg_limit_config_change_requests SET reason_code = 'x' WHERE jurisdiction_code = $1`, f.jur)
			return err
		})
		wantCode(t, "owner content UPDATE of a decided request", err, "RL010")
	})

	t.Run("TimeOutRestrictions", func(t *testing.T) {
		f := w.newFix(t)
		ins := func(s sess, tenant *uuid.UUID, brand *uuid.UUID, typ string, ends *time.Time) error {
			return do(w.rt, s, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO player_restrictions (person_id, tenant_id, brand_id, player_account_id, restriction_type,
					starts_at, ends_at, source, created_by_actor_type, created_by_actor_id)
					VALUES ($1, $2, $3, $4, $5, now(), $6, 'player_self_service', 'player', $4)`, f.personA, tenant, brand, f.acctA1, typ, ends)
				return err
			})
		}
		pa := playerSess(f.tenantA, f.acctA1)
		end := time.Now().Add(24 * time.Hour)
		wantOK(t, "platform_person time_out", ins(pa, nil, nil, "time_out", &end))
		wantOK(t, "tenant_person time_out pinned to the session tenant", ins(pa, &f.tenantA, nil, "time_out", &end))
		wantCode(t, "time_out naming another tenant", ins(pa, &f.tenantB, nil, "time_out", &end), "42501")
		wantCode(t, "brand-scoped player time_out", ins(pa, &f.tenantA, &f.brandA1, "time_out", &end), "42501")
		wantCode(t, "an indefinite time_out", ins(pa, nil, nil, "time_out", nil), "23514")
		wantCode(t, "a tenant-scoped player self_exclusion is still refused", ins(pa, &f.tenantA, nil, "self_exclusion", nil), "42501")
		wantOK(t, "player self_exclusion unchanged", ins(pa, nil, nil, "self_exclusion", nil))
		if c := codeOf(ins(pa, nil, nil, "cool_off", &end)); c != "42501" && c != "23514" {
			t.Fatalf("an unknown restriction type must be refused, got %q", c)
		}
		// The CHECK itself (as the owner, RLS satisfied by a staff-shaped platform row).
		err := do(w.owner, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO player_restrictions (person_id, restriction_type, ends_at, source, created_by_actor_type, created_by_actor_id)
				VALUES ($1, 'cool_off', now() + interval '1 hour', 'staff', 'staff', $2)`, f.personA, w.ops.Requester)
			return err
		})
		wantCode(t, "restriction_type CHECK", err, "23514")
	})

	t.Run("CatalogAndGrants", func(t *testing.T) {
		var n int
		wantOK(t, "catalog", do(w.owner, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM pg_index i WHERE i.indexrelid IN ('idx_ledger_entries_tenant_player_asset_created'::regclass, 'idx_deposit_intents_tenant_player_asset_created'::regclass) AND i.indisvalid)
				+ (SELECT count(*) FROM information_schema.columns WHERE table_name = 'casino_launch_sessions' AND column_name = 'auth_session_root_started_at' AND is_nullable = 'YES')
				+ (SELECT count(*) FROM pg_class WHERE relname IN ('rg_limit_configs', 'rg_limit_config_change_requests', 'player_rg_limit_changes') AND relrowsecurity AND relforcerowsecurity)`).Scan(&n)
		}))
		if n != 6 {
			t.Fatalf("catalog objects: %d of 6", n)
		}
		want := map[string]string{
			"database_environment":            "SELECT",
			"rg_limit_config_change_requests": "INSERT,SELECT",
			"rg_limit_configs":                "INSERT,SELECT",
			"player_rg_limit_changes":         "INSERT,SELECT",
		}
		for tbl, privs := range want {
			var got string
			wantOK(t, "privs", do(w.owner, sess{}, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT coalesce(string_agg(p, ',' ORDER BY p), '') FROM unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p
					WHERE has_table_privilege('igaming_runtime', 'public.'||$1, p)`, tbl).Scan(&got)
			}))
			if got != privs {
				t.Errorf("%s runtime privileges = %q, want %q", tbl, got, privs)
			}
		}
		var col bool
		wantOK(t, "column privs", do(w.owner, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT has_column_privilege('igaming_runtime', 'public.rg_limit_config_change_requests', 'status', 'UPDATE')
				AND NOT has_column_privilege('igaming_runtime', 'public.rg_limit_config_change_requests', 'payload_hash', 'UPDATE')`).Scan(&col)
		}))
		if !col {
			t.Fatal("the runtime role may UPDATE only rg_limit_config_change_requests.status")
		}
	})

	t.Run("RGFix_PermissiveSyntheticFloorsResolveOnlyInTest", func(t *testing.T) {
		f := w.newFix(t)
		rgfix.SeedPermissiveFloorsAt(t, w.ownerURL)
		inEnv := func(env string) sess {
			return sess{tenant: f.tenantA.String(), principal: f.staffA.String(), env: env}
		}
		for _, k := range rgfix.ControlKinds {
			if c := w.compose(t, inEnv("test"), f.jur, &f.tenantA, &f.brandA1, k, eur); c["found"] != true || c["enabled"] != false {
				t.Fatalf("%s: the permissive fixture floor must resolve in a test session: %v", k, c)
			}
			if c := w.compose(t, inEnv(""), f.jur, &f.tenantA, &f.brandA1, k, eur); c["found"] != false {
				t.Fatalf("%s: a synthetic floor must resolve as ABSENT without app.environment: %v", k, c)
			}
			if c := w.compose(t, inEnv("staging"), f.jur, &f.tenantA, &f.brandA1, k, eur); c["found"] != false {
				t.Fatalf("%s: a synthetic floor must resolve as ABSENT in staging: %v", k, c)
			}
		}
		rgfix.SeedPermissiveFloorsAt(t, w.ownerURL) // idempotent
		// An operator row over a synthetic floor: where the floor counts as absent, the operator row
		// alone never makes the configuration "found" (a floor must be in force).
		opTest := sess{tenant: f.tenantA.String(), principal: f.staffA.String(), env: "test"}
		wantOK(t, "operator row over the synthetic floor", do(w.rt, opTest, func(ctx context.Context, tx pgx.Tx) error {
			_, err := rgfix.InsertRow(ctx, tx, rgfix.PermissiveFloor(f.jur, "deposit").Clone(rgfix.Spec{"tenant_id": f.tenantA.String(),
				"attestation_ref": testAttestation, "enabled": true}), nil)
			return err
		}))
		if c := w.compose(t, staffSess(f.tenantA, f.staffA), f.jur, &f.tenantA, &f.brandA1, "deposit", eur); c["found"] != false {
			t.Fatalf("an operator row without a floor in force must not compose as found: %v", c)
		}
		if c := w.compose(t, opTest, f.jur, &f.tenantA, &f.brandA1, "deposit", eur); c["found"] != true || c["enabled"] != true {
			t.Fatalf("in a test session the floor and the operator row compose: %v", c)
		}
		var n int
		wantOK(t, "count", do(w.rt, sess{env: "test"}, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_configs WHERE jurisdiction_code = $1 AND attestation_ref = 'synthetic'`, f.jur).Scan(&n)
		}))
		if n != len(rgfix.ControlKinds) {
			t.Fatalf("one synthetic floor per kind, got %d", n)
		}

		// app.environment per connection: a pool built from a production Config (APP_ENV unset)
		// sends app.environment=production in every startup packet, and a DATABASE_URL claiming
		// test is discarded - so on this test-anchored database a production-configured process
		// still resolves the synthetic floors as ABSENT.
		u := w.rt.Raw().Config().ConnString()
		if strings.Contains(u, "?") {
			u += "&app.environment=test"
		} else {
			u += "?app.environment=test"
		}
		prodPool, err := db.ConnectWithRuntimeParams(ctx, u, 2, 10*time.Second, config.Config{}.DatabaseRuntimeParams())
		if err != nil {
			t.Fatal(err)
		}
		defer prodPool.Close()
		var env string
		var found bool
		wantOK(t, "production pool", prodPool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT current_setting('app.environment', true),
				(rg_limit_config_compose($1, NULL, NULL, 'deposit', NULL, clock_timestamp())).found`, f.jur).Scan(&env, &found)
		}))
		if env != "production" || found {
			t.Fatalf("a production-configured connection must carry app.environment=production and see no synthetic floor: env=%q found=%v", env, found)
		}
		// Startup check (security LOW): a production process refuses this test-anchored database
		// (read through the runtime role, as platform-api does).
		if _, err := db.VerifyEnvironmentAnchor(ctx, "production", prodPool); !errors.Is(err, db.ErrEnvironmentAnchorMismatch) {
			t.Fatalf("a production start on a test-anchored database must be refused, got %v", err)
		}
	})

	t.Run("SecurityConditions_C1SeqId_C2SessionTenant_C4TimeOutNarrowing_ASCII", func(t *testing.T) {
		f := w.newFix(t)
		w.floor(t, midFloor(f.jur, "deposit").Clone(rgfix.Spec{"increase_cooldown": "01:00:00", "increase_requires_reconfirmation": false, "scope": "account", "mandatory": false}))
		// C1: a player-supplied seq / id is replaced (no 23505 existence oracle across tenants).
		a, err := w.change(playerSess(f.tenantA, f.acctA1), chg{tenant: f.tenantA, acct: f.acctA1, kind: "deposit", asset: eur, period: rolling, value: strp("100")})
		wantOK(t, "tenant A row", err)
		var aSeq int64
		wantOK(t, "seq", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT seq FROM player_rg_limit_changes WHERE id = $1`, a.id).Scan(&aSeq)
		}))
		var bID uuid.UUID
		var bSeq int64
		err = do(w.rt, playerSess(f.tenantB, f.acctB), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO player_rg_limit_changes
				(seq, id, tenant_id, player_account_id, scope, control_kind, asset_code, period_kind, value, source, reason_code, idempotency_key, request_hash)
				VALUES ($1, $2, $3, $4, 'account', 'deposit', 'EUR', 'rolling_24h', 5, 'player_self_service', 'c1', 'c1-probe', $5)
				RETURNING id, seq`, aSeq, a.id, f.tenantB, f.acctB, hex64()).Scan(&bID, &bSeq)
		})
		wantOK(t, "a player supplying another tenant's seq / id", err)
		if bID == a.id || bSeq == aSeq {
			t.Fatalf("seq / id must be database values: got id=%v seq=%d (probe id=%v seq=%d)", bID, bSeq, a.id, aSeq)
		}
		// C2: the read primitives refuse a tenant other than the session's (RL026), never a silent answer.
		probes := []struct {
			name, q string
			args    []any
		}{
			{"compose", `SELECT (rg_limit_config_compose($1, $2, NULL, 'deposit', 'EUR', clock_timestamp())).found`, []any{f.jur, f.tenantA}},
			{"realized", `SELECT (rg_limit_realized($1, $2, $3, 'account', 'deposit', 'EUR', 'rolling_24h', clock_timestamp())).has_row`,
				[]any{f.tenantA, f.acctA1, f.personA}},
			{"classify", `SELECT (rg_limit_change_classify($1, $2, $3, NULL, $4, 'account', 'deposit', 'EUR', 'rolling_24h', 1, NULL, NULL, 'player_self_service', clock_timestamp())).change_kind IS NOT NULL`,
				[]any{f.tenantA, f.acctA1, f.personA, f.jur}},
			// The cancel / confirm path reads the history directly (no realized / compose call first).
			{"classify-supersede", `SELECT (rg_limit_change_classify($1, $2, $3, NULL, $4, 'account', 'deposit', 'EUR', 'rolling_24h', NULL, $5, 'cancel_pending', 'player_self_service', clock_timestamp())).change_kind IS NOT NULL`,
				[]any{f.tenantA, f.acctA1, f.personA, f.jur, a.id}},
		}
		for _, p := range probes {
			for sname, s := range map[string]sess{"tenant B staff": staffSess(f.tenantB, f.staffB), "platform": platformSess(w.ops.Requester), "tenant-less": {}} {
				err := do(w.rt, s, func(ctx context.Context, tx pgx.Tx) error {
					var b bool
					return tx.QueryRow(ctx, p.q, p.args...).Scan(&b)
				})
				wantCode(t, p.name+" for tenant A from a "+sname+" session", err, "RL026")
			}
		}
		var stillHasRow bool
		wantOK(t, "own-tenant realized", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT (rg_limit_realized($1, $2, $3, 'account', 'deposit', 'EUR', 'rolling_24h', clock_timestamp())).has_row`, f.tenantA, f.acctA1, f.personA).Scan(&stillHasRow)
		}))
		if !stillHasRow {
			t.Fatal("the own-tenant read must still see the realized row")
		}
		// C4: a platform-scope time_out of person X (accounts in tenant A only) is invisible to tenant
		// B staff, visible to tenant A staff and to X; tenant-scoped rows are unchanged.
		end := time.Now().Add(24 * time.Hour)
		wantOK(t, "player X platform time_out", do(w.rt, playerSess(f.tenantA, f.acctX), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO player_restrictions (person_id, player_account_id, restriction_type, ends_at, source, created_by_actor_type, created_by_actor_id)
				VALUES ($1, $2, 'time_out', $3, 'player_self_service', 'player', $2)`, f.personX, f.acctX, end)
			return err
		}))
		seen := func(s sess) int {
			var n int
			wantOK(t, "time_out count", do(w.rt, s, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM player_restrictions WHERE person_id = $1 AND restriction_type = 'time_out'`, f.personX).Scan(&n)
			}))
			return n
		}
		if n := seen(staffSess(f.tenantB, f.staffB)); n != 0 {
			t.Fatalf("tenant B staff saw %d of person X's platform time_out rows", n)
		}
		if n := seen(staffSess(f.tenantA, f.staffA)); n != 1 {
			t.Fatalf("tenant A staff must see person X's platform time_out, saw %d", n)
		}
		if n := seen(playerSess(f.tenantA, f.acctX)); n != 1 {
			t.Fatalf("player X must see their own time_out, saw %d", n)
		}
		if n := seen(platformSess(w.ops.Requester)); n != 1 {
			t.Fatalf("a platform session is not narrowed, saw %d", n)
		}
		// ASCII attestation (security LOW): a Cyrillic homoglyph of 'synthetic' is refused by the CHECK.
		cyr := midFloor(f.jur, "loss").Clone(rgfix.Spec{"attestation_ref": "ѕynthetic"})
		err = do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := rgfix.InsertFloor(ctx, tx, cyr, rgfix.DefaultSteps())
			return err
		})
		wantCode(t, "a non-ASCII attestation_ref", err, "23514")
	})
}
