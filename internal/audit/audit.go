// Package audit is the platform's append-only security/administrative
// event log. See docs/decisions/0013-audit-log-immutability-and-dual-
// scope-rls.md: immutability is enforced by a database trigger
// (audit_log_immutable), not by convention, and this package never
// issues an UPDATE or DELETE against the table.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ActorType identifies what kind of principal performed an action.
type ActorType string

const (
	ActorPlayer  ActorType = "player"
	ActorStaff   ActorType = "staff"
	ActorService ActorType = "service"
	ActorSystem  ActorType = "system"
)

// Outcome is the result of the audited action.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
	OutcomeDenied  Outcome = "denied"
)

// Entry is one audit record. TenantID is the zero uuid.UUID{} for
// platform-level events (see the dual-scope RLS pattern) - callers pass
// uuid.Nil explicitly rather than leaving it ambiguous.
type Entry struct {
	TenantID   uuid.UUID // uuid.Nil for platform-level events
	ActorType  ActorType
	ActorID    uuid.UUID // uuid.Nil only valid when ActorType == ActorSystem
	Action     string
	TargetType string
	TargetID   string
	Outcome    Outcome
	IPAddress  string
	UserAgent  string
	RequestID  string
	// Metadata is structured extra detail. Never put secrets or
	// authentication material here (passwords, tokens, token hashes,
	// signing keys) - this is a code-review-enforced invariant, not a
	// mechanically checked one; see CLAUDE.md's audit rules.
	Metadata map[string]any
}

// Record writes an audit entry using tx - the SAME transaction as the
// action being audited, so the two commit or roll back together. This is
// deliberate: an action that "succeeded" with no audit record, or an
// audit record for an action that never actually committed, are both
// compliance gaps CLAUDE.md and the Stage 2 instructions ask this
// package to make structurally impossible, not just unlikely.
//
// tx must already be scoped correctly for entry.TenantID: use
// db.Pool.WithTenant for a tenant-scoped entry, db.Pool.WithoutTenant for
// a platform-level one (TenantID == uuid.Nil) - Record itself does not
// set the tenant context, matching every other tenant-scoped write in
// this codebase.
func Record(ctx context.Context, tx pgx.Tx, entry Entry) error {
	if entry.ActorType == ActorSystem && entry.ActorID != uuid.Nil {
		return fmt.Errorf("audit: actor_id must be empty for actor_type 'system'")
	}
	if entry.ActorType != ActorSystem && entry.ActorID == uuid.Nil {
		return fmt.Errorf("audit: actor_id is required for actor_type %q", entry.ActorType)
	}

	metadata := entry.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("audit: marshal metadata: %w", err)
	}

	var tenantID *uuid.UUID
	if entry.TenantID != uuid.Nil {
		tenantID = &entry.TenantID
	}
	var actorID *uuid.UUID
	if entry.ActorID != uuid.Nil {
		actorID = &entry.ActorID
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO audit_log
			(tenant_id, actor_type, actor_id, action, target_type, target_id, outcome, ip_address, user_agent, request_id, metadata)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, NULLIF($8, '')::inet, NULLIF($9, ''), NULLIF($10, ''), $11)`,
		tenantID, entry.ActorType, actorID, entry.Action, entry.TargetType, entry.TargetID,
		entry.Outcome, entry.IPAddress, entry.UserAgent, entry.RequestID, metadataJSON,
	)
	if err != nil {
		return fmt.Errorf("audit: insert entry: %w", err)
	}
	return nil
}
