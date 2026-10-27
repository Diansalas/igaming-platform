package aml

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RecordScreeningSkippingValidation presents the DATABASE with an input that Validate would refuse, so a test can prove that
// aml_screening_record and the table CHECKs hold the SR7-2 line on their own (defence in depth behind the Go mirror).
func (s *Service) RecordScreeningSkippingValidation(ctx context.Context, a Actor, in RecordScreeningInput) (ScreeningRecorded, error) {
	return s.recordScreening(ctx, a, in)
}

// lic30 (LOW-4): TEST-ONLY. The production monitor reads one bounded batch per pass (ScreeningParkedScan); this loops every batch of the
// tenant in one transaction, unbounded, so it is deliberately not part of the production API.
// Counts only: the persons parked for want of a screening result and those past the alert threshold (lic26 N-9, lic29).
func ScreeningParkedOverdue(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (parked, overdue int, err error) {
	var after *uuid.UUID
	for {
		sc, e := ScreeningParkedScan(ctx, tx, tenantID, after, ParkedScanBatch)
		if e != nil {
			return 0, 0, e
		}
		parked += sc.Parked
		overdue += sc.Overdue
		if sc.Next == nil {
			return parked, overdue, nil
		}
		after = sc.Next
	}
}

// AuthorScreeningPolicySkippingGoChecks calls the database author without the Go-side bounds, so a test can prove that
// aml_screening_policy_author holds the effective-time bound and the re-screen floor on its own (defence in depth, lic30).
func (s *Service) AuthorScreeningPolicySkippingGoChecks(ctx context.Context, a Actor, in SetScreeningPolicyInput) (uuid.UUID, error) {
	return s.writeScreeningPolicy(ctx, a, in, "aml_screening_policy_author")
}
