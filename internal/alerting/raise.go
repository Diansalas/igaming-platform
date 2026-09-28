package alerting

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const maxDedupRaceRetries = 2

// insertOrAttachOccurrence implements ADR §5's mechanics exactly:
//
//  1. INSERT ... ON CONFLICT DO NOTHING RETURNING id.
//  2. If no row is returned, SELECT the non-resolved row by
//     (tenant_id, dedup_key) - reconstructed here as (subject_tenant_id,
//     kind, discriminator) since every Kind raisable through this API is
//     platform-owned (tenant_id is always NULL; ADR §4.3: no tenant-owned
//     Kind exists in PRH-2) - then INSERT the occurrence.
//  3. If a concurrent resolve races the SELECT (the row it found is
//     resolved by the time the occurrence INSERT runs, or vanishes from
//     the non-resolved set), retry at most twice.
func insertOrAttachOccurrence(ctx context.Context, tx pgx.Tx, a Alert, attrsJSON []byte) error {
	var subjectID any
	if a.SubjectTenantID != uuid.Nil {
		subjectID = a.SubjectTenantID
	}

	for attempt := 0; attempt <= maxDedupRaceRetries; attempt++ {
		var alertID uuid.UUID
		err := tx.QueryRow(ctx,
			`INSERT INTO alerts (subject_tenant_id, kind, discriminator, attributes)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT DO NOTHING
			 RETURNING id`,
			subjectID, string(a.Kind), a.Discriminator, attrsJSON,
		).Scan(&alertID)
		if err == nil {
			return insertOccurrence(ctx, tx, alertID)
		}
		if err != pgx.ErrNoRows {
			return fmt.Errorf("alerting: insert alert: %w", err)
		}

		// No row returned: an open/acked row with this dedup key already
		// exists. Find it and attach a new occurrence.
		found, ferr := findOpenAlert(ctx, tx, subjectID, a.Kind, a.Discriminator)
		if ferr != nil {
			return ferr
		}
		if found == uuid.Nil {
			// The row we conflicted with was concurrently resolved
			// between the INSERT and this SELECT - retry the whole
			// INSERT, which will now succeed (a resolved row no longer
			// participates in the partial unique index).
			continue
		}
		if occErr := insertOccurrence(ctx, tx, found); occErr != nil {
			// LF C-4: insertOccurrence always wraps a real error (or
			// succeeds) - it never returns a distinguishable "the row
			// vanished mid-flight" sentinel, so there is nothing to retry
			// on here. Propagate as-is; the outer InTx/RaiseGuarded
			// machinery classifies and swallows/retries at the right
			// layer already.
			return occErr
		}
		return nil
	}
	return fmt.Errorf("alerting: dedup race exceeded %d retries for kind %q", maxDedupRaceRetries, a.Kind)
}

func findOpenAlert(ctx context.Context, tx pgx.Tx, subjectID any, kind Kind, discriminator string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM alerts
		 WHERE tenant_id IS NULL
		   AND subject_tenant_id IS NOT DISTINCT FROM $1
		   AND kind = $2
		   AND discriminator = $3
		   AND state <> 'resolved'`,
		subjectID, string(kind), discriminator,
	).Scan(&id)
	if err == pgx.ErrNoRows {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("alerting: find open alert: %w", err)
	}
	return id, nil
}

func insertOccurrence(ctx context.Context, tx pgx.Tx, alertID uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO alert_occurrences (alert_id) VALUES ($1)`, alertID)
	if err != nil {
		return fmt.Errorf("alerting: insert occurrence: %w", err)
	}
	return nil
}

// Raise performs the strict, unguarded raise: Go validation, then the
// dedup-INSERT-or-attach-occurrence mechanics, with NO savepoint and NO
// SQLSTATE swallow. It is used only by RaiseDetached (ADR §5) - every
// in-transaction business call site uses RaiseGuarded instead.
func Raise(ctx context.Context, tx pgx.Tx, a Alert) error {
	v, err := a.validate()
	if err != nil {
		return err
	}
	attrsJSON, err := json.Marshal(v.attrs)
	if err != nil {
		return fmt.Errorf("alerting: marshal attributes: %w", err)
	}
	return insertOrAttachOccurrence(ctx, tx, a, attrsJSON)
}
