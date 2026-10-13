package db

// ADR 0119 S4b, section 9.5: the scoped in-transaction staff re-assertion of
// the governed flows that move value or change authority (ADR 0082 Amendment
// A9). CheckActorStaffLive applies it to the rows the flow's own A8 staff FOR
// SHARE statement returned: the actor's row must be present, active and at the
// role the verified token names (staff_version joins with migration 0139; every
// staff member is at version 0 until then).
//
// The app.actor_staff_id GUC and the staff_actor_self_* policy pair of section
// 9.5(b) are NOT built: every flow re-asserted in S4b already sees the actor's
// own row (ADR 0119 S4b-R3 / D13). They land with the first flow that needs
// them.

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrActorStaffNotLive: the governed transaction's own actor is no longer a
// live staff member at the moment its A8 staff FOR SHARE lock was taken
// (suspended, role changed, or the row is not visible). Nothing is written;
// the HTTP layer answers 401 session_revoked (ADR 0119 section 9.5).
var ErrActorStaffNotLive = errors.New("db: the acting staff member is no longer live")

// ActorStaffRow is the part of a staff_users row the re-assertion compares
// (ADR 0099 A-19 column discipline: id, role, status; staff_version from 0139).
type ActorStaffRow struct {
	ID     uuid.UUID
	Role   string
	Status string
}

// CheckActorStaffLive returns ErrActorStaffNotLive unless rows contains the
// actor's row, active, at expectedRole. An empty expectedRole never matches.
func CheckActorStaffLive(rows []ActorStaffRow, actor uuid.UUID, expectedRole string) error {
	if actor == uuid.Nil || expectedRole == "" {
		return ErrActorStaffNotLive
	}
	for _, r := range rows {
		if r.ID != actor {
			continue
		}
		if r.Status == "active" && r.Role == expectedRole {
			return nil
		}
		return ErrActorStaffNotLive
	}
	return ErrActorStaffNotLive
}

// ScanActorStaffRows reads every row of an A8 staff FOR SHARE statement (so
// every row lock is taken) and returns the id, role and status of each row,
// located by column NAME, so the statement keeps its own select list (it must
// include id, role and status).
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
	if !okID || !okRole || !okStatus {
		return nil, fmt.Errorf("db: actor staff rows: the statement must select id, role and status")
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
		out = append(out, r)
	}
	return out, rows.Err()
}
