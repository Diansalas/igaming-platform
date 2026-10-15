package aml

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// Pool is the database handle the officer API needs (satisfied by *db.Pool).
type Pool interface {
	WithPrincipalScope(ctx context.Context, tenantID, principalID uuid.UUID, fn db.TxFunc) error
	WithPlatformActingInTenant(ctx context.Context, principalID, targetTenantID, requestID uuid.UUID, operation string, fn db.TxFunc) error
}

// Config wires a Service.
type Config struct {
	Pool Pool
	// Proofs signs the ADR 0110 actor proofs. nil means actorproof.Default(); with neither, every call fails closed
	// (ErrProofUnavailable).
	Proofs *actorproof.Issuer
	// LockTimeout is SET LOCAL lock_timeout of every officer transaction (default "5s").
	LockTimeout string
}

// Service is the officer-facing AML API (holds, cases, notes, four-eyes). It holds no per-request state. Every method
// opens the actor's own session, signs a proof for exactly the call it makes, and reaches an OWNER-owned SECURITY DEFINER
// function that re-authorises the actor in SQL (migration 0140 aml_authorize). Nothing here reads an AML table.
type Service struct {
	cfg Config
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Pool == nil {
		return nil, errors.New("aml: a pool is required")
	}
	if cfg.LockTimeout == "" {
		cfg.LockTimeout = "5s"
	}
	return &Service{cfg: cfg}, nil
}

// runActor opens the actor's session transaction: a tenant principal bound to its own tenant, or a platform principal
// acting in the (route-validated) target tenant. The acting setter is db.Pool.WithPlatformActingInTenant, which
// re-validates the platform_admin + in-force grant itself (migration 0112); the operation label is deliberately neutral
// because the "acting session opened" audit row is tenant-visible (no tipping-off).
func (s *Service) runActor(ctx context.Context, a Actor, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if err := a.validate(); err != nil {
		return err
	}
	wrapped := func(ctx context.Context, tx pgx.Tx) error {
		var iso string
		if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&iso); err != nil {
			return fmt.Errorf("aml: read isolation level: %w", err)
		}
		if iso != "read committed" {
			return fmt.Errorf("%w: officer transactions must be READ COMMITTED, got %s", ErrInvalidInput, iso)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, s.cfg.LockTimeout); err != nil {
			return fmt.Errorf("aml: set lock_timeout: %w", err)
		}
		// Request metadata for the append-only case trail (evidence only; the database validates the shape and ignores
		// anything unusable). It carries no authority.
		if _, err := tx.Exec(ctx, `SELECT set_config('app.aml_ip', $1, true), set_config('app.aml_request_id', $2, true)`, a.IPAddress, a.RequestID); err != nil {
			return fmt.Errorf("aml: set request metadata: %w", err)
		}
		return fn(ctx, tx)
	}
	var err error
	switch a.Scope {
	case ScopeTenant:
		err = s.cfg.Pool.WithPrincipalScope(ctx, a.TenantID, a.StaffID, wrapped)
	case ScopePlatformActing:
		err = s.cfg.Pool.WithPlatformActingInTenant(ctx, a.StaffID, a.TenantID, uuid.Nil, "compliance", wrapped)
	}
	return classify(err)
}

// attachProof signs and attaches the proof for the call about to be made. Operation names are the closed set of
// proof.go; the target is the row id (or "new" for a create); payloadHash is the Digest of exactly the arguments the SQL
// function hashes.
func (s *Service) attachProof(ctx context.Context, tx pgx.Tx, a Actor, op, target, payloadHash string) error {
	iss := s.cfg.Proofs
	if iss == nil {
		iss = actorproof.Default()
	}
	if iss == nil {
		return ErrProofUnavailable
	}
	return iss.Attach(ctx, tx, actorproof.Claims{
		Actor: a.StaffID, Scope: a.Scope, Tenant: a.TenantID, Operation: op, Target: target, PayloadHash: payloadHash,
	})
}

// The closed AML operation names a proof can bind (ADR 0116 s3.1). Scope tenant | platform_acting only.
const (
	OpProofHoldPlace      = "aml_hold:place"
	OpProofHoldLift       = actorproof.OpAMLHoldLift
	OpProofHoldRead       = "aml_hold:read"
	OpProofAlertRaise     = "aml_alert:raise"
	OpProofCaseOpen       = "aml_case:open"
	OpProofCaseRead       = "aml_case:read"
	OpProofCaseTransition = "aml_case:transition"
	OpProofCaseNote       = "aml_case:note"
	OpProofCaseFlagSAR    = "aml_case:flag_sar"
	OpProofCasePropose    = actorproof.OpAMLCasePropose
	OpProofCaseApprove    = actorproof.OpAMLCaseApprove
)

// proofOperations is the closed set, exported to the actorproof scope mirror test.
var proofOperations = map[string]bool{
	OpProofHoldPlace: true, OpProofHoldLift: true, OpProofHoldRead: true,
	OpProofAlertRaise: true, OpProofCaseOpen: true, OpProofCaseRead: true, OpProofCaseTransition: true, OpProofCaseNote: true,
	OpProofCaseFlagSAR: true, OpProofCasePropose: true, OpProofCaseApprove: true,
}

// ProofOperations lists the AML proof operation names (for tests that mirror the verifier's admission rule).
func ProofOperations() []string {
	out := make([]string, 0, len(proofOperations))
	for k := range proofOperations {
		out = append(out, k)
	}
	return out
}

func tenantStr(id uuid.UUID) *string { return actorproof.S(id.String()) }
