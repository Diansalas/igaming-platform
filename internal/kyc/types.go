// Package kyc implements Stage 4F's platform-owned player verification
// (KYC) and document-management foundation. It does NOT implement real
// KYC/AML - no vendor is selected, no real provider API is integrated,
// and no vendor API is invented. The package exists to define the
// PLATFORM-OWNED verification/document model and a provider-neutral
// boundary a real vendor adapter slots into later (docs/decisions/0028).
//
// No second identity model is introduced: Verification and Document both
// carry person_id purely as a denormalized anchor to the EXISTING,
// unchanged Person/PlayerAccount/Tenant/Brand model (internal/identity) -
// this package adds no new identity concept, only a verification/
// document subsystem hanging off that existing model. KYC status is
// never collapsed into identity.PlayerAccountStatus - see
// docs/decisions/0028 §1 for why they are kept as distinct, independently
// evolving concepts.
package kyc

import (
	"time"

	"github.com/google/uuid"
)

// VerificationStatus is kyc_verifications.status - the platform-owned
// state machine, never a raw provider-specific string (directive §10).
type VerificationStatus string

const (
	StatusUnverified     VerificationStatus = "unverified"
	StatusPending        VerificationStatus = "pending"
	StatusReviewRequired VerificationStatus = "review_required"
	StatusApproved       VerificationStatus = "approved"
	StatusRejected       VerificationStatus = "rejected"
	StatusExpired        VerificationStatus = "expired"
)

// Verification mirrors one kyc_verifications row - one verification
// attempt/session for a PlayerAccount. TenantID/BrandID/PlayerAccountID
// are this row's OWNING scope (tenant-owned, RLS-enforced - migration
// 0040); PersonID is a denormalized, non-enforcing anchor to the
// platform-wide Person (see this package's own doc comment and ADR 0028
// §7's recorded OPEN DECISION on cross-tenant reuse).
type Verification struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	BrandID           uuid.UUID
	PlayerAccountID   uuid.UUID
	PersonID          uuid.UUID
	Status            VerificationStatus
	ProviderID        string
	ProviderReference string
	Reason            string
	SubmittedAt       *time.Time
	ReviewedAt        *time.Time
	ReviewedBy        uuid.UUID // uuid.Nil if never staff-reviewed
	ExpiresAt         *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// DocumentStatus is kyc_documents.status - the document's own, narrower
// review lifecycle (UPLOAD -> PENDING_REVIEW -> APPROVED|REJECTED,
// directive §5), independent of its parent Verification's own status.
type DocumentStatus string

const (
	DocumentPendingReview DocumentStatus = "pending_review"
	DocumentApproved      DocumentStatus = "approved"
	DocumentRejected      DocumentStatus = "rejected"
)

// DocumentType mirrors kyc_documents.document_type's CHECK constraint -
// intentionally a small, fixed set (not free-form text) so the platform
// never has to guess what "document_type" a raw string denotes.
type DocumentType string

const (
	DocumentPassport       DocumentType = "passport"
	DocumentNationalID     DocumentType = "national_id"
	DocumentDriversLicense DocumentType = "drivers_license"
	DocumentProofOfAddress DocumentType = "proof_of_address"
	DocumentSelfie         DocumentType = "selfie"
	DocumentOther          DocumentType = "other"
)

// Document mirrors one kyc_documents row. Immutable after upload except
// Status/ReviewedAt/ReviewedBy/RejectionReason (migration 0040's trigger
// enforces this at the database level, not just here) - a reupload of the
// same DocumentType is a NEW row (Version+1), never an edit of this one.
type Document struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	BrandID           uuid.UUID
	PlayerAccountID   uuid.UUID
	PersonID          uuid.UUID
	VerificationID    uuid.UUID
	DocumentType      DocumentType
	IssuingCountry    string
	Version           int
	Status            DocumentStatus
	StorageProvider   string
	StorageReference  string
	ContentType       string
	SizeBytes         int64
	OriginalFilename  string
	ChecksumSHA256    string
	DocumentExpiresAt *time.Time
	RejectionReason   string
	UploadedAt        time.Time
	ReviewedAt        *time.Time
	ReviewedBy        uuid.UUID
}
