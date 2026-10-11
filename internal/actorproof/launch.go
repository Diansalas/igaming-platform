package actorproof

import (
	"sort"
	"strings"

	"github.com/google/uuid"
)

// LaunchRequest is the caller-supplied content of a launch authorisation request
// (ADR 0112 4.2), exactly the columns migration 0130's SQL function
// launch_request_content_digest() binds. from_status, to_status, required_approvals,
// payload_hash and the actor columns are forced by the database and are bound through
// payload_hash on every later step, so they are not part of this digest.
type LaunchRequest struct {
	TenantID    uuid.UUID
	SubjectKind string     // "tenant" | "brand"
	BrandID     *uuid.UUID // nil for a tenant subject
	Action      string

	LicensingModel, LicensingStatus *string
	LicenceID                       *uuid.UUID
	DeterminationReference          *string
	DeterminationByRole             *string
	ResponsibleOperatorName         *string
	ResponsibleOperatorRegistration string
	JurisdictionIDs                 []uuid.UUID
	ConditionsNote                  string
	ReadinessSnapshotHash           string
	AcknowledgedCheckCodes          []string
	ReasonCode                      string
}

// Digest is SHA-256(k2_canonical(...)) over the content columns, identical to the SQL
// function. Arrays are comma-joined in byte order (uuid order is byte order; text uses
// COLLATE "C" in SQL), an empty array is SQL NULL.
func (r LaunchRequest) Digest() string {
	tenant := r.TenantID.String()
	jur := make([]string, 0, len(r.JurisdictionIDs))
	for _, j := range r.JurisdictionIDs {
		jur = append(jur, j.String())
	}
	sort.Strings(jur)
	acks := append([]string(nil), r.AcknowledgedCheckCodes...)
	sort.Strings(acks)
	join := func(v []string) *string {
		if len(v) == 0 {
			return nil
		}
		s := strings.Join(v, ",")
		return &s
	}
	return Digest(
		&tenant, S(r.SubjectKind), UUIDP(r.BrandID), S(r.Action),
		r.LicensingModel, r.LicensingStatus, UUIDP(r.LicenceID),
		r.DeterminationReference, r.DeterminationByRole,
		r.ResponsibleOperatorName, S(r.ResponsibleOperatorRegistration),
		join(jur), S(r.ConditionsNote), S(r.ReadinessSnapshotHash), join(acks), S(r.ReasonCode),
	)
}
