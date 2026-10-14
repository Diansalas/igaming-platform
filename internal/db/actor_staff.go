package db

// ADR 0119 S4b, section 9.5: the scoped in-transaction staff re-assertion of
// the governed flows that move value or change authority (ADR 0082 Amendment
// A9). CheckActorStaffLive applies it to the rows the flow's own A8 staff FOR
// SHARE statement returned: the actor's row must be present, active, at the
// role the verified token names and (ADR 0119 S5 + S6, migration 0139) at the
// staff_version the token's `sv` claim names.
//
// ADR 0119 S5 + S6 (section 9.5(b), S4b-R3): SetActorStaffIDForCurrentTx is the
// ONLY setter of the app.actor_staff_id GUC (a static test pins it). Migration
// 0139's staff_actor_self_select / staff_actor_self_lock policy pair keys on it:
// inside a transaction scoped to ANOTHER tenant (a platform admin acting on a
// tenant staff member through db.WithTenant(target)) the actor's own platform
// row is otherwise hidden by the dual-scope policy, so its FOR SHARE
// re-assertion could not see it. The lock policy's WITH CHECK (false) means the
// GUC can never admit an actual UPDATE of the actor's row.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrActorStaffNotLive: the governed transaction's own actor is no longer a
// live staff member at the moment its A8 staff FOR SHARE lock was taken
// (suspended, role changed, staff_version changed, or the row is not
// visible). Nothing is written; the HTTP layer answers 401 session_revoked
// (ADR 0119 section 9.5).
var ErrActorStaffNotLive = errors.New("db: the acting staff member is no longer live")

// ActorStaffRow is the part of a staff_users row the re-assertion compares
// (ADR 0099 A-19 column discipline: id, role, status, staff_version).
type ActorStaffRow struct {
	ID           uuid.UUID
	Role         string
	Status       string
	StaffVersion int64
}

// ActorExpectation is what the verified token says about the actor: its role
// and its `sv` claim (ADR 0119 s7.1).
type ActorExpectation struct {
	Role         string
	StaffVersion int64
}

// CheckActorStaffLive returns ErrActorStaffNotLive unless rows contains the
// actor's row, active, at want.Role and want.StaffVersion. An empty role or a
// staff version below 1 never matches (every staff row starts at version 1,
// migration 0139).
func CheckActorStaffLive(rows []ActorStaffRow, actor uuid.UUID, want ActorExpectation) error {
	if actor == uuid.Nil || want.Role == "" || want.StaffVersion < 1 {
		return ErrActorStaffNotLive
	}
	for _, r := range rows {
		if r.ID != actor {
			continue
		}
		if r.Status == "active" && r.Role == want.Role && r.StaffVersion == want.StaffVersion {
			return nil
		}
		return ErrActorStaffNotLive
	}
	return ErrActorStaffNotLive
}

// ScanActorStaffRows reads every row of an A8 staff FOR SHARE statement (so
// every row lock is taken) and returns the id, role, status and staff_version
// of each row, located by column NAME, so the statement keeps its own select
// list (it must include id, role, status and staff_version).
func ScanActorStaffRows(rows pgx.Rows, err error) ([]ActorStaffRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx := map[string]int{}
	for n, f := range rows.FieldDescriptions() {
		idx[f.Name] = n
	}
	iID, okID := idx["id"]
	iRole, okRole := idx["role"]
	iStatus, okStatus := idx["status"]
	iVersion, okVersion := idx["staff_version"]
	if !okID || !okRole || !okStatus || !okVersion {
		return nil, fmt.Errorf("db: actor staff rows: the statement must select id, role, status and staff_version")
	}
	var out []ActorStaffRow
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		var r ActorStaffRow
		switch v := vals[iID].(type) {
		case [16]byte:
			r.ID = uuid.UUID(v)
		case uuid.UUID:
			r.ID = v
		default:
			return nil, fmt.Errorf("db: actor staff rows: id has type %T", vals[iID])
		}
		r.Role, _ = vals[iRole].(string)
		r.Status, _ = vals[iStatus].(string)
		r.StaffVersion, _ = vals[iVersion].(int64)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetActorStaffIDForCurrentTx sets app.actor_staff_id, for the rest of the
// current transaction only, to the governed transaction's own actor - the
// verified token subject, never a client-supplied value. It is the ONLY setter
// of that GUC (TestStatic_ActorStaffIDSingleSetter): migration 0139's
// staff_actor_self_* policy pair makes exactly that one staff row visible and
// FOR SHARE lockable (never updatable) inside a transaction scoped to another
// tenant, for the ADR 0119 section 9.5 re-assertion.
func SetActorStaffIDForCurrentTx(ctx context.Context, tx pgx.Tx, actor uuid.UUID) error {
	if actor == uuid.Nil {
		return fmt.Errorf("db: SetActorStaffIDForCurrentTx called with a nil actor")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_staff_id', $1, true)`, actor.String()); err != nil {
		return fmt.Errorf("db: set actor staff id: %w", err)
	}
	return nil
}
