package launchgov

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Hard precondition failure codes (ADR 0112 5.2). They refuse; nothing else does. In particular
// there is NO rule "no licence => not launchable": four recorded bases exist and each is always
// available as long as the record is internally consistent and does not contradict the registry.
const (
	H1SubjectClosed       = "H-1:subject_closed"
	H1IllegalTransition   = "H-1:illegal_transition"
	H1StatusChanged       = "H-1:subject_status_changed"
	H2LicenceRequired     = "H-2:licence_id_required"
	H2LicenceMustBeAbsent = "H-2:licence_id_must_be_absent"
	H2LicenceNotFound     = "H-2:licence_not_found"
	H2LicenseeMismatch    = "H-2:licensee_mismatch"
	H2TenantModel         = "H-2:tenant_licensing_model_mismatch"
	H2TenantBinding       = "H-2:tenant_licence_binding_mismatch"
	H2Determination       = "H-2:determination_required"
	H2StatusMismatch      = "H-2:licensing_status_mismatch"
	H2UnknownModel        = "H-2:unknown_licensing_model"
	H3NoJurisdiction      = "H-3:no_jurisdiction"
	H3ConfigMissing       = "H-3:config_missing_or_not_in_effect"
	H6WithdrawalsHolding  = "H-6:withdrawals_holding_funds"
)

// moveFor returns the status the action moves a subject in `from` to, and whether the move is
// legal (ADR 0112 3.1; mirrors launch_action_move_legal in migration 0128).
func moveFor(action, from string) (to string, ok bool) {
	switch action {
	case ActionActivate:
		return StatusActive, from == StatusPendingLaunch
	case ActionSuspend:
		return StatusSuspended, from == StatusActive
	case ActionReactivate:
		return StatusActive, from == StatusSuspended
	case ActionClose:
		return StatusClosed, from == StatusPendingLaunch || from == StatusActive || from == StatusSuspended
	case ActionRatify:
		return from, from == StatusActive || from == StatusSuspended
	}
	return "", false
}

// assessment is the result of one read-only evaluation: the readiness, the subject's current
// status and the hard-precondition failures. Hard failures are NOT readiness checks.
type assessment struct {
	Readiness Readiness
	From      string
	Failures  []string
	Holds     int64
}

// assess reads, in ONE read-only tenant-scoped transaction (the route-validated tenant), the
// subject's status, the hard preconditions H-1..H-3 and H-6 and the readiness checks. It writes
// nothing.
func (s *Service) assess(ctx context.Context, in EvalInput) (assessment, error) {
	if in.Subject.TenantID == uuid.Nil {
		return assessment{}, fmt.Errorf("%w: tenant id is required", ErrInvalidInput)
	}
	doConfig, doFunds := checkSetFor(in.Action)
	if !doConfig && !doFunds {
		return assessment{}, fmt.Errorf("%w: unknown action %q", ErrInvalidInput, in.Action)
	}
	a := assessment{}
	a.Readiness = Readiness{Version: SnapshotVersion, ComputedAt: s.now(), Environment: s.cfg.Environment, Info: map[string]int64{}}
	err := s.cfg.Pool.WithTenantReadOnly(ctx, in.Subject.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var q string
		var args []any
		if in.Subject.BrandID != nil {
			q, args = `SELECT status FROM brands WHERE id = $1 AND tenant_id = $2`, []any{*in.Subject.BrandID, in.Subject.TenantID}
		} else {
			q, args = `SELECT status FROM tenants WHERE id = $1`, []any{in.Subject.TenantID}
		}
		if err := tx.QueryRow(ctx, q, args...).Scan(&a.From); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if a.From == StatusClosed {
			a.Failures = append(a.Failures, H1SubjectClosed)
		} else if _, ok := moveFor(in.Action, a.From); !ok {
			a.Failures = append(a.Failures, H1IllegalTransition)
		}
		if doConfig {
			f, err := s.licensingFailures(ctx, tx, in.Subject.TenantID, in.Licensing)
			if err != nil {
				return err
			}
			a.Failures = append(a.Failures, f...)
			f, err = jurisdictionFailures(ctx, tx, in.Subject.TenantID, in.JurisdictionIDs, s.now())
			if err != nil {
				return err
			}
			a.Failures = append(a.Failures, f...)
			a.Readiness.Checks = append(a.Readiness.Checks, s.configChecks(ctx, tx, in)...)
		}
		if doFunds {
			a.Readiness.Checks = append(a.Readiness.Checks, fundsCheck(ctx, tx, in.Subject, &a.Readiness))
		}
		if in.Action == ActionClose {
			n, err := holdBearingWithdrawals(ctx, tx, in.Subject)
			if err != nil {
				return err
			}
			a.Holds = n
			if n > 0 {
				a.Failures = append(a.Failures, H6WithdrawalsHolding)
			}
		}
		return nil
	})
	if err != nil {
		return assessment{}, err
	}
	if doConfig {
		a.Readiness.Checks = append(a.Readiness.Checks, s.providerTierCheck(), alertsCheck())
	}
	sortChecks(a.Readiness.Checks)
	return a, nil
}

// licensingFailures is hard precondition H-2: the recorded licensing basis is internally
// consistent and does not contradict the licence registry. It never decides whether a licence is
// legally required.
func (s *Service) licensingFailures(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, rec *LicensingRecord) ([]string, error) {
	if rec == nil {
		return []string{H2UnknownModel}, nil
	}
	var fails []string
	switch rec.Model {
	case LicModelPlatform, LicModelTenant, LicModelOther, LicModelNotApplic:
	default:
		return []string{H2UnknownModel}, nil
	}
	switch rec.Status {
	case LicStatusInForce, LicStatusAppliedPend, LicStatusConditional, LicStatusSuspended, LicStatusExpired, LicStatusNotRequired:
	default:
		fails = append(fails, H2StatusMismatch)
	}
	// not_required_per_determination belongs to the not-applicable basis and to no other.
	if (rec.Status == LicStatusNotRequired) != (rec.Model == LicModelNotApplic) {
		fails = append(fails, H2StatusMismatch)
	}
	needsDetermination := rec.Model == LicModelOther || rec.Model == LicModelNotApplic
	if needsDetermination && (strings.TrimSpace(rec.DeterminationReference) == "" || strings.TrimSpace(rec.DeterminationByRole) == "") {
		fails = append(fails, H2Determination)
	}
	if rec.Model == LicModelNotApplic && rec.LicenceID != nil {
		fails = append(fails, H2LicenceMustBeAbsent)
	}
	licenceBacked := rec.Model == LicModelPlatform || rec.Model == LicModelTenant
	if licenceBacked && rec.LicenceID == nil {
		fails = append(fails, H2LicenceRequired)
	}
	if rec.LicenceID != nil && rec.Model != LicModelNotApplic {
		var tModel string
		var tLicence *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT licensing_model, licence_id FROM tenants WHERE id = $1`, tenantID).Scan(&tModel, &tLicence); err != nil {
			return nil, fmt.Errorf("launchgov: read tenant licensing: %w", err)
		}
		var licensee string
		err := tx.QueryRow(ctx, `SELECT licensee FROM licences WHERE id = $1`, *rec.LicenceID).Scan(&licensee)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			fails = append(fails, H2LicenceNotFound)
		case err != nil:
			return nil, fmt.Errorf("launchgov: read licence: %w", err)
		default:
			wantModel, wantLicensee := "under_platform_licence", "platform"
			if rec.Model == LicModelTenant {
				wantModel, wantLicensee = "own_licence", "tenant"
			}
			if rec.Model == LicModelOther {
				// No basis-implied licensee: the licence must agree with how the tenant is licensed.
				wantModel = tModel
				wantLicensee = map[string]string{"under_platform_licence": "platform", "own_licence": "tenant"}[tModel]
			}
			if licensee != wantLicensee {
				fails = append(fails, H2LicenseeMismatch)
			}
			if tModel != wantModel {
				fails = append(fails, H2TenantModel)
			}
			if tLicence == nil || *tLicence != *rec.LicenceID {
				fails = append(fails, H2TenantBinding)
			}
		}
	}
	return dedupe(fails), nil
}

// jurisdictionFailures is hard precondition H-3: at least one jurisdiction, each with a tenant
// jurisdiction configuration in effect.
func jurisdictionFailures(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, ids []uuid.UUID, now time.Time) ([]string, error) {
	distinct := map[uuid.UUID]bool{}
	for _, j := range ids {
		distinct[j] = true
	}
	if len(distinct) == 0 {
		return []string{H3NoJurisdiction}, nil
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT jurisdiction_id) FROM tenant_jurisdiction_configs
		WHERE tenant_id = $1 AND jurisdiction_id = ANY($2) AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)`,
		tenantID, ids, now).Scan(&n); err != nil {
		return nil, fmt.Errorf("launchgov: read jurisdiction configs: %w", err)
	}
	if n != len(distinct) {
		return []string{H3ConfigMissing}, nil
	}
	return nil, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
