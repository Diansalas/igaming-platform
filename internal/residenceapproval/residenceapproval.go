// Package residenceapproval is the residence determination maker-checker (lic10; audit G8; owner question Q21; migration 0181).
//
// Today a single reviewer's residence determination becomes market evidence at once (internal/kyc.ReadMarketResidenceEvidence).
// This package makes that CONFIGURABLE per tenant, without a code release:
//
//   - mode none              today's behaviour (the DEFAULT of every tenant; an absent policy is none);
//   - mode into_open_market  a determination into a market the brand serves needs a second, distinct Person;
//   - mode all               every determination needs a second, distinct Person.
//
// A determination that needs approval and has none is NOT market evidence (pending fails closed). The mode in force is
// SNAPSHOTTED on each determination (migration 0181), so a later change of mode applies to FUTURE determinations only.
//
// Governance of the policy itself (all DB-enforced, internal/residenceapproval only issues the calls):
//
//   - a change to a STRICTER mode is active at once (one actor; it only tightens);
//   - a change to a LESS strict mode is filed PENDING and takes effect only after two approvals by two distinct Persons, neither
//     the requester's (the "stricter needs 0 approvals" rule is a table CHECK);
//   - every write re-resolves the actor in SQL (financial_actor_session) and verifies a signed actor proof (ADR 0110) bound to
//     the exact arguments; the runtime role has no write privilege on any table.
//
// This is NOT a licensing_change kind: the licensing change registry is replaced as a whole by each migration that adds a kind
// (licensing_change_classify / licensing_change_row_matches), and the parallel operating-bases work (migration 0180) changes the
// same two functions. A tenant-scoped configuration with its own four-eyes tables avoids a blind merge of two replacements of
// one security function. The approval counts and the rule are the same as the licensing change primitive's (2 approvals, distinct
// Persons, requester excluded), and the same ADR 0110 proof discipline applies.
//
// LABELS: IMPLEMENTED against the local database (integration tests are for the owner to run, see the lic10 record). No legal
// value is shipped: the mode, who approves and when are the operator's configuration.
package residenceapproval

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// Mode is a residence approval mode.
type Mode string

const (
	ModeNone           Mode = "none"
	ModeIntoOpenMarket Mode = "into_open_market"
	ModeAll            Mode = "all"
)

// Valid reports whether m is one of the closed modes.
func (m Mode) Valid() bool { return m == ModeNone || m == ModeIntoOpenMarket || m == ModeAll }

// Rank orders the modes by strictness (none < into_open_market < all); -1 for an invalid mode.
func (m Mode) Rank() int {
	switch m {
	case ModeNone:
		return 0
	case ModeIntoOpenMarket:
		return 1
	case ModeAll:
		return 2
	}
	return -1
}

// Proof operation names (migration 0181; actorproof constants).
const (
	OpPolicySet            = actorproof.OpResidencePolicySet
	OpPolicyApprove        = actorproof.OpResidencePolicyApprove
	OpDeterminationApprove = actorproof.OpResidenceDeterminationApprove
)

// ScopeTenant is the only scope of this service.
const ScopeTenant = actorproof.ScopeTenant

var reasonCodeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

// ValidReasonCode reports whether s is a well-formed reason code (a code, never free text).
func ValidReasonCode(s string) bool { return reasonCodeRE.MatchString(s) }

// Sentinel errors. Database refusals (SQLSTATE class RD) are mapped to these.
var (
	ErrNotFound         = errors.New("residenceapproval: not found")
	ErrForbidden        = errors.New("residenceapproval: forbidden")
	ErrInvalidInput     = errors.New("residenceapproval: invalid input")
	ErrNotPending       = errors.New("residenceapproval: nothing pending, stale, or no change")
	ErrFourEyes         = errors.New("residenceapproval: four-eyes refused (the requester or determiner may not approve their own act)")
	ErrNoPerson         = errors.New("residenceapproval: the account is not linked to a Person")
	ErrProofUnavailable = errors.New("residenceapproval: no actor-proof issuer configured")
	ErrActorScope       = errors.New("residenceapproval: invalid actor")
	ErrDuplicate        = errors.New("residenceapproval: already recorded")
)

// Actor is the authenticated TENANT staff member (role compliance), resolved by the caller from the verified token (never from a
// request body); TenantID is the actor's own tenant. There is no platform_acting path (it would widen the ADR 0099 acting-session
// allow-list; a platform-licensed tenant is run through its own compliance accounts).
type Actor struct {
	StaffID   uuid.UUID
	Scope     string
	TenantID  uuid.UUID
	IPAddress string
	UserAgent string
	RequestID string
}

func (a Actor) validate() error {
	if a.StaffID == uuid.Nil || a.TenantID == uuid.Nil || a.Scope != ScopeTenant {
		return ErrActorScope
	}
	return nil
}

// Pool is the database handle the service needs (satisfied by *db.Pool).
type Pool interface {
	WithPrincipalScope(ctx context.Context, tenantID, principalID uuid.UUID, fn db.TxFunc) error
}

// Config wires a Service.
type Config struct {
	Pool Pool
	// Proofs signs the ADR 0110 actor proofs. nil means actorproof.Default(); with neither, every call fails closed.
	Proofs *actorproof.Issuer
	// LockTimeout is SET LOCAL lock_timeout of every transaction (default "5s").
	LockTimeout string
}

// Service is the residence maker-checker API.
type Service struct{ cfg Config }

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Pool == nil {
		return nil, errors.New("residenceapproval: a pool is required")
	}
	if cfg.LockTimeout == "" {
		cfg.LockTimeout = "5s"
	}
	return &Service{cfg: cfg}, nil
}

func (s *Service) runActor(ctx context.Context, a Actor, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if err := a.validate(); err != nil {
		return err
	}
	wrapped := func(ctx context.Context, tx pgx.Tx) error {
		var iso string
		if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&iso); err != nil {
			return fmt.Errorf("residenceapproval: read isolation level: %w", err)
		}
		if iso != "read committed" {
			return fmt.Errorf("%w: transactions must be READ COMMITTED, got %s", ErrInvalidInput, iso)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, s.cfg.LockTimeout); err != nil {
			return fmt.Errorf("residenceapproval: set lock_timeout: %w", err)
		}
		return fn(ctx, tx)
	}
	return classify(s.cfg.Pool.WithPrincipalScope(ctx, a.TenantID, a.StaffID, wrapped))
}

func (s *Service) attachProof(ctx context.Context, tx pgx.Tx, a Actor, op, target, hash string) error {
	iss := s.cfg.Proofs
	if iss == nil {
		iss = actorproof.Default()
	}
	if iss == nil {
		return ErrProofUnavailable
	}
	return iss.Attach(ctx, tx, actorproof.Claims{Actor: a.StaffID, Scope: a.Scope, Tenant: a.TenantID, Operation: op, Target: target, PayloadHash: hash})
}

func tenantS(id uuid.UUID) *string { return actorproof.S(id.String()) }

// ---- policy ---------------------------------------------------------------------------------------------------------------

// PolicyChange is the result of SetPolicy.
type PolicyChange struct {
	PolicyID            uuid.UUID
	Status              string // active (stricter, executed at once) | pending (less strict, needs approvals)
	RequiredApprovals   int
	PreviousMode        Mode
	RequestedMode       Mode
	ExecutedImmediately bool
}

// SetPolicy files a change of the tenant's residence approval mode. A stricter mode is active at once; a less strict mode is
// filed pending and needs two approvals by two distinct Persons (neither the requester's). The mode in force is the database's:
// the caller does not state the "previous" mode, so the stricter / less strict decision cannot be spoofed.
func (s *Service) SetPolicy(ctx context.Context, a Actor, mode Mode, reasonCode string) (PolicyChange, error) {
	if !mode.Valid() || !ValidReasonCode(reasonCode) {
		return PolicyChange{}, fmt.Errorf("%w: mode or reason code", ErrInvalidInput)
	}
	var out PolicyChange
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantS(a.TenantID), actorproof.S(string(mode)), actorproof.S(reasonCode))
		if err := s.attachProof(ctx, tx, a, OpPolicySet, actorproof.TargetNew, hash); err != nil {
			return err
		}
		var prev string
		if err := tx.QueryRow(ctx, `SELECT o_policy_id, o_status, o_required_approvals, o_previous_mode FROM residence_policy_set($1, $2, $3)`,
			a.TenantID, string(mode), reasonCode).Scan(&out.PolicyID, &out.Status, &out.RequiredApprovals, &prev); err != nil {
			return err
		}
		out.PreviousMode, out.RequestedMode, out.ExecutedImmediately = Mode(prev), mode, out.Status == "active"
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: a.TenantID, ActorType: audit.ActorStaff, ActorID: a.StaffID, Action: "residence_policy.changed",
			TargetType: "residence_approval_policy", TargetID: out.PolicyID.String(), Outcome: audit.OutcomeSuccess,
			IPAddress: a.IPAddress, UserAgent: a.UserAgent, RequestID: a.RequestID,
			Metadata: map[string]any{"previous_mode": prev, "requested_mode": string(mode), "status": out.Status,
				"required_approvals": out.RequiredApprovals, "reason_code": reasonCode},
		})
	})
	return out, err
}

// PolicyApproval is the result of ApprovePolicy.
type PolicyApproval struct {
	Status            string // active once the required approvals are recorded, else pending
	Approvals         int
	RequiredApprovals int
}

// ApprovePolicy records one approval of a PENDING (less strict) policy change; the approval that reaches the required count
// activates it in the same transaction.
func (s *Service) ApprovePolicy(ctx context.Context, a Actor, policyID uuid.UUID, reasonCode string) (PolicyApproval, error) {
	if policyID == uuid.Nil || !ValidReasonCode(reasonCode) {
		return PolicyApproval{}, fmt.Errorf("%w: policy id or reason code", ErrInvalidInput)
	}
	var out PolicyApproval
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantS(a.TenantID), actorproof.S(policyID.String()), actorproof.S(reasonCode))
		if err := s.attachProof(ctx, tx, a, OpPolicyApprove, policyID.String(), hash); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT o_status, o_approvals, o_required_approvals FROM residence_policy_approve($1, $2, $3)`,
			a.TenantID, policyID, reasonCode).Scan(&out.Status, &out.Approvals, &out.RequiredApprovals); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: a.TenantID, ActorType: audit.ActorStaff, ActorID: a.StaffID, Action: "residence_policy.approved",
			TargetType: "residence_approval_policy", TargetID: policyID.String(), Outcome: audit.OutcomeSuccess,
			IPAddress: a.IPAddress, UserAgent: a.UserAgent, RequestID: a.RequestID,
			Metadata: map[string]any{"status": out.Status, "approvals": out.Approvals, "required_approvals": out.RequiredApprovals,
				"reason_code": reasonCode},
		})
	})
	return out, err
}

// PolicyVersion is one row of the policy history.
type PolicyVersion struct {
	ID                uuid.UUID
	Mode              Mode
	PreviousMode      Mode
	Status            string
	RequiredApprovals int
	Approvals         int
	ReasonCode        string
	RequestedBy       uuid.UUID
	RequestedByScope  string
	RequestedAt       time.Time
	ActivatedAt       *time.Time
}

// Policy is the tenant's mode in force and the version history (newest first, at most 200).
type Policy struct {
	EffectiveMode Mode
	Versions      []PolicyVersion
}

// ReadPolicy reads the policy. An empty history means mode none (the default). No proof (a read).
func (s *Service) ReadPolicy(ctx context.Context, a Actor) (Policy, error) {
	out := Policy{EffectiveMode: ModeNone}
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT o_policy_id, o_mode, o_previous_mode, o_status, o_required_approvals, o_approvals, o_reason_code,
			o_requested_by, o_requested_by_scope, o_requested_at, o_activated_at, o_effective_mode FROM residence_policy_list($1)`, a.TenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v PolicyVersion
			var mode, prev, eff string
			if err := rows.Scan(&v.ID, &mode, &prev, &v.Status, &v.RequiredApprovals, &v.Approvals, &v.ReasonCode, &v.RequestedBy,
				&v.RequestedByScope, &v.RequestedAt, &v.ActivatedAt, &eff); err != nil {
				return err
			}
			v.Mode, v.PreviousMode = Mode(mode), Mode(prev)
			out.EffectiveMode = Mode(eff)
			out.Versions = append(out.Versions, v)
		}
		return rows.Err()
	})
	if err != nil {
		return Policy{}, err
	}
	if !out.EffectiveMode.Valid() {
		out.EffectiveMode = ModeNone
	}
	return out, nil
}

// ---- determination approval -----------------------------------------------------------------------------------------------

// ApproveDetermination approves ONE reviewer determination, bound to the verification and the exact determined_at the approver
// saw (a later determination replaces it and makes the approval stale). Tenant scope only; the second Person must differ from the
// determiner in staff account AND Person (the database refuses otherwise). The audit row carries ids only: never the country.
func (s *Service) ApproveDetermination(ctx context.Context, a Actor, verificationID uuid.UUID, determinedAt time.Time, reasonCode string) error {
	if verificationID == uuid.Nil || determinedAt.IsZero() || !ValidReasonCode(reasonCode) {
		return fmt.Errorf("%w: verification, determined_at or reason code", ErrInvalidInput)
	}
	at := determinedAt.UTC().Truncate(time.Microsecond)
	return s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantS(a.TenantID), actorproof.S(verificationID.String()), actorproof.TS(&at), actorproof.S(reasonCode))
		if err := s.attachProof(ctx, tx, a, OpDeterminationApprove, verificationID.String(), hash); err != nil {
			return err
		}
		var checkID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT o_check_id FROM residence_determination_approve($1, $2, $3, $4)`,
			a.TenantID, verificationID, at, reasonCode).Scan(&checkID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: a.TenantID, ActorType: audit.ActorStaff, ActorID: a.StaffID, Action: "kyc.residence_determination_approved",
			TargetType: "kyc_verification", TargetID: verificationID.String(), Outcome: audit.OutcomeSuccess,
			IPAddress: a.IPAddress, UserAgent: a.UserAgent, RequestID: a.RequestID,
			Metadata: map[string]any{"check_id": checkID.String(), "reason_code": reasonCode},
		})
	})
}

// classify maps a database error to a service error.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case "RD001":
		return fmt.Errorf("%w: %s", ErrForbidden, pe.Message)
	case "CG001", "CG020":
		return fmt.Errorf("%w: %s", ErrForbidden, pe.Message)
	case "RD002":
		return fmt.Errorf("%w: %s", ErrInvalidInput, pe.Message)
	case "RD003":
		return fmt.Errorf("%w: %s", ErrNotPending, pe.Message)
	case "RD004":
		return fmt.Errorf("%w: %s", ErrFourEyes, pe.Message)
	case "RD006":
		return fmt.Errorf("%w: %s", ErrNoPerson, pe.Message)
	case "RD040":
		return ErrNotFound
	case "23505":
		return fmt.Errorf("%w: %s", ErrDuplicate, pe.Message)
	case "AP001", "AP002", "AP003", "AP004", "AP005":
		return fmt.Errorf("%w: actor proof refused (%s)", ErrForbidden, pe.Code)
	}
	return err
}
