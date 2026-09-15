package kyc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// ErrMalwareDetected is returned by UploadDocument when the configured
// MalwareScanner reports the content is not clean - the upload is
// refused outright, nothing is stored, nothing is inserted.
var ErrMalwareDetected = errors.New("kyc: uploaded content failed malware scanning")

const documentColumns = `id, tenant_id, brand_id, player_account_id, person_id, verification_id,
	document_type, issuing_country, version, status, storage_provider, storage_reference,
	content_type, size_bytes, original_filename, checksum_sha256, document_expires_at,
	rejection_reason, uploaded_at, reviewed_at, reviewed_by`

func scanDocument(row pgx.Row) (Document, error) {
	var d Document
	var issuingCountry, rejectionReason *string
	var reviewedBy *uuid.UUID
	err := row.Scan(&d.ID, &d.TenantID, &d.BrandID, &d.PlayerAccountID, &d.PersonID, &d.VerificationID,
		&d.DocumentType, &issuingCountry, &d.Version, &d.Status, &d.StorageProvider, &d.StorageReference,
		&d.ContentType, &d.SizeBytes, &d.OriginalFilename, &d.ChecksumSHA256, &d.DocumentExpiresAt,
		&rejectionReason, &d.UploadedAt, &d.ReviewedAt, &reviewedBy,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("kyc: scan document: %w", err)
	}
	if issuingCountry != nil {
		d.IssuingCountry = *issuingCountry
	}
	if rejectionReason != nil {
		d.RejectionReason = *rejectionReason
	}
	if reviewedBy != nil {
		d.ReviewedBy = *reviewedBy
	}
	return d, nil
}

// UploadDocumentParams is UploadDocument's input. TenantID/BrandID/
// PlayerAccountID/PersonID/VerificationID must already be server-resolved
// - never client-supplied. Filename/Content come from the multipart
// upload as-is (unvalidated) - ValidateUpload and the malware scan below
// are what make this call safe to expose to a player directly.
type UploadDocumentParams struct {
	TenantID          uuid.UUID
	BrandID           uuid.UUID
	PlayerAccountID   uuid.UUID
	PersonID          uuid.UUID
	VerificationID    uuid.UUID
	DocumentType      DocumentType
	IssuingCountry    string
	Filename          string
	Content           []byte
	DocumentExpiresAt *string // ISO-8601 date, optional, parsed by the caller before this if needed - kept as a plain field here since this package does no date-format opinion beyond passing it through
}

// UploadDocument validates (size/content-sniffing/extension consistency),
// malware-scans, stores (via storage), records a new kyc_documents row -
// a version-incremented NEW row if this player_account_id already has a
// document of this DocumentType (directive §5: never overwrite history),
// version 1 otherwise - and then, when provider is non-nil, calls
// KYCProvider.SubmitVerification with the verification's CURRENT full
// document set (not just the one just uploaded - a real vendor evaluates
// the accumulated evidence, not one file in isolation) and applies the
// normalized outcome to the verification's own status exactly like
// CreateVerification/the Orchestrator's callback handling already do
// (adversarial/integrations specialist review finding, Stage 4F: an
// earlier version of this stage defined SubmitVerification on the
// KYCProvider interface but never actually called it from anywhere,
// leaving it and GetVerification untested dead code - directive §9
// explicitly named SubmitVerification as a method to implement, not
// merely declare). provider may be nil (mirrors PersonResolver/
// PaymentOrchestrator's own nil-tolerant callers) - a caller that hasn't
// wired a KYCProvider still gets a stored, reviewable document, just
// without the provider-side submission step. tx must already be
// tenant-scoped.
func UploadDocument(ctx context.Context, tx pgx.Tx, storage DocumentStorageProvider, scanner MalwareScanner, provider KYCProvider, params UploadDocumentParams) (Document, error) {
	if params.VerificationID == uuid.Nil {
		return Document{}, fmt.Errorf("%w: verification_id is required", ErrInvalidTransition)
	}

	sniffedType, sanitizedName, err := ValidateUpload(params.Filename, params.Content)
	if err != nil {
		return Document{}, err
	}

	clean, err := scanner.Scan(ctx, params.Content)
	if err != nil {
		// Fail closed - a scanner that could not run is treated exactly
		// like a positive detection, never like "assume clean" (this
		// type's own doc comment / MalwareScanner's own contract).
		return Document{}, fmt.Errorf("kyc: malware scan unavailable: %w", err)
	}
	if !clean {
		return Document{}, ErrMalwareDetected
	}

	sum := sha256.Sum256(params.Content)
	checksum := hex.EncodeToString(sum[:])

	stored, err := storage.Store(ctx, sniffedType, params.Content)
	if err != nil {
		return Document{}, fmt.Errorf("kyc: store document content: %w", err)
	}

	var version int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM kyc_documents WHERE player_account_id = $1 AND document_type = $2`,
		params.PlayerAccountID, params.DocumentType,
	).Scan(&version); err != nil {
		return Document{}, fmt.Errorf("kyc: resolve document version: %w", err)
	}

	d := Document{
		ID: uuid.New(), TenantID: params.TenantID, BrandID: params.BrandID,
		PlayerAccountID: params.PlayerAccountID, PersonID: params.PersonID, VerificationID: params.VerificationID,
		DocumentType: params.DocumentType, IssuingCountry: params.IssuingCountry, Version: version,
		Status: DocumentPendingReview, StorageProvider: storage.ID(), StorageReference: stored.Reference,
		ContentType: sniffedType, SizeBytes: int64(len(params.Content)), OriginalFilename: sanitizedName, ChecksumSHA256: checksum,
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO kyc_documents (id, tenant_id, brand_id, player_account_id, person_id, verification_id,
			document_type, issuing_country, version, status, storage_provider, storage_reference,
			content_type, size_bytes, original_filename, checksum_sha256, document_expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, $10, $11, $12, $13, $14, $15, $16, NULLIF($17, '')::timestamptz)`,
		d.ID, d.TenantID, d.BrandID, d.PlayerAccountID, d.PersonID, d.VerificationID,
		d.DocumentType, d.IssuingCountry, d.Version, d.Status, d.StorageProvider, d.StorageReference,
		d.ContentType, d.SizeBytes, d.OriginalFilename, d.ChecksumSHA256, derefOrEmpty(params.DocumentExpiresAt),
	)
	if err != nil {
		return Document{}, fmt.Errorf("kyc: insert document: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: params.TenantID, ActorType: audit.ActorPlayer, ActorID: params.PlayerAccountID,
		Action: "kyc.document_uploaded", TargetType: "kyc_document", TargetID: d.ID.String(),
		Outcome:  audit.OutcomeSuccess,
		Metadata: map[string]any{"document_type": string(d.DocumentType), "version": d.Version, "content_type": d.ContentType, "size_bytes": d.SizeBytes},
	}); err != nil {
		return Document{}, fmt.Errorf("kyc: audit document upload: %w", err)
	}

	if provider != nil {
		if err := submitVerificationDocuments(ctx, tx, provider, params.VerificationID); err != nil {
			return Document{}, err
		}
	}

	return scanDocument(tx.QueryRow(ctx, `SELECT `+documentColumns+` FROM kyc_documents WHERE id = $1`, d.ID))
}

// submitVerificationDocuments gathers verificationID's CURRENT full set of
// non-rejected documents and calls KYCProvider.SubmitVerification with
// them, then applies the normalized result exactly like
// updateVerificationStatus/the Orchestrator's callback handling already
// do - idempotent (a verification already in a terminal status is left
// untouched) and never overwrites reviewed_at/reviewed_by (a provider
// submission is not a staff review - see updateVerificationStatus's own
// doc comment).
func submitVerificationDocuments(ctx context.Context, tx pgx.Tx, provider KYCProvider, verificationID uuid.UUID) error {
	v, err := GetVerificationByID(ctx, tx, verificationID)
	if err != nil {
		return err
	}
	if isTerminal(v.Status) {
		return nil
	}

	rows, err := tx.Query(ctx, `SELECT id, document_type FROM kyc_documents WHERE verification_id = $1 AND status <> $2`,
		verificationID, DocumentRejected)
	if err != nil {
		return fmt.Errorf("kyc: list documents for submission: %w", err)
	}
	var submitted []SubmittedDocument
	for rows.Next() {
		var sd SubmittedDocument
		if err := rows.Scan(&sd.DocumentID, &sd.DocumentType); err != nil {
			rows.Close()
			return fmt.Errorf("kyc: scan document for submission: %w", err)
		}
		submitted = append(submitted, sd)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("kyc: list documents for submission: %w", err)
	}

	result, err := provider.SubmitVerification(ctx, v.ProviderReference, submitted)
	if err != nil {
		return fmt.Errorf("kyc: submit verification to provider: %w", err)
	}

	auditOutcome := audit.OutcomeSuccess
	if result.Outcome == ProviderError {
		auditOutcome = audit.OutcomeFailure
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: v.TenantID, ActorType: audit.ActorSystem,
		Action: "kyc.verification_submitted_to_provider", TargetType: "kyc_verification", TargetID: v.ID.String(),
		Outcome:  auditOutcome,
		Metadata: map[string]any{"provider_id": v.ProviderID, "document_count": len(submitted), "provider_outcome": string(result.Outcome), "reason": result.Reason},
	}); err != nil {
		return fmt.Errorf("kyc: audit provider submission: %w", err)
	}

	if result.Outcome == ProviderError {
		return nil
	}
	newStatus, ok := statusForOutcome(result.Outcome)
	if !ok {
		return fmt.Errorf("kyc: provider returned an unrecognized outcome %q", result.Outcome)
	}
	_, err = updateVerificationStatus(ctx, tx, v.ID, newStatus, result.Reason)
	return err
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// GetDocumentByID reads one document's metadata, RLS-scoped.
func GetDocumentByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Document, error) {
	return scanDocument(tx.QueryRow(ctx, `SELECT `+documentColumns+` FROM kyc_documents WHERE id = $1`, id))
}

// ListDocumentsForAccount returns every document version for
// playerAccountID, newest first.
func ListDocumentsForAccount(ctx context.Context, tx pgx.Tx, playerAccountID uuid.UUID) ([]Document, error) {
	rows, err := tx.Query(ctx, `SELECT `+documentColumns+` FROM kyc_documents WHERE player_account_id = $1 ORDER BY uploaded_at DESC`, playerAccountID)
	if err != nil {
		return nil, fmt.Errorf("kyc: list documents: %w", err)
	}
	defer rows.Close()
	var out []Document
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDocumentContent resolves d's stored bytes via storage AND writes the
// mandatory "sensitive document access" audit record (directive §7/§17) -
// every content read, player or staff, is audited, never silently served.
func GetDocumentContent(ctx context.Context, tx pgx.Tx, storage DocumentStorageProvider, d Document, accessorType audit.ActorType, accessorID uuid.UUID) (contentType string, content []byte, err error) {
	contentType, content, err = storage.Retrieve(ctx, d.StorageReference)
	if err != nil {
		return "", nil, fmt.Errorf("kyc: retrieve document content: %w", err)
	}
	if auditErr := audit.Record(ctx, tx, audit.Entry{
		TenantID: d.TenantID, ActorType: accessorType, ActorID: accessorID,
		Action: "kyc.document_accessed", TargetType: "kyc_document", TargetID: d.ID.String(),
		Outcome: audit.OutcomeSuccess, Metadata: map[string]any{"document_type": string(d.DocumentType), "version": d.Version},
	}); auditErr != nil {
		return "", nil, fmt.Errorf("kyc: audit document access: %w", auditErr)
	}
	return contentType, content, nil
}

// ReviewDocumentParams is ReviewDocument's input.
type ReviewDocumentParams struct {
	DocumentID uuid.UUID
	StaffID    uuid.UUID
	NewStatus  DocumentStatus // must be DocumentApproved or DocumentRejected
	Reason     string
}

// ReviewDocument transitions a document out of pending_review - a
// PermVerificationReview-gated staff action. Only the four review-related
// columns change (status/reviewed_at/reviewed_by/rejection_reason) -
// migration 0040's trigger enforces this is the ONLY kind of UPDATE this
// table ever accepts, so this function's own SQL is the sole legitimate
// caller of UPDATE on kyc_documents in this codebase.
func ReviewDocument(ctx context.Context, tx pgx.Tx, params ReviewDocumentParams) (Document, error) {
	if params.NewStatus != DocumentApproved && params.NewStatus != DocumentRejected {
		return Document{}, fmt.Errorf("%w: status must be approved or rejected", ErrInvalidTransition)
	}
	if params.StaffID == uuid.Nil || params.DocumentID == uuid.Nil {
		return Document{}, fmt.Errorf("%w: staff_id and document_id are required", ErrInvalidTransition)
	}

	current, err := GetDocumentByID(ctx, tx, params.DocumentID)
	if err != nil {
		return Document{}, err
	}
	if current.Status != DocumentPendingReview {
		return Document{}, fmt.Errorf("%w: document %s is not pending review (status %q)", ErrInvalidTransition, params.DocumentID, current.Status)
	}
	if params.NewStatus == DocumentRejected && params.Reason == "" {
		return Document{}, fmt.Errorf("%w: reason is required to reject a document", ErrInvalidTransition)
	}

	_, err = tx.Exec(ctx,
		`UPDATE kyc_documents SET status = $1, rejection_reason = NULLIF($2, ''), reviewed_at = now(), reviewed_by = $3 WHERE id = $4`,
		params.NewStatus, params.Reason, params.StaffID, params.DocumentID,
	)
	if err != nil {
		return Document{}, fmt.Errorf("kyc: review document: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: current.TenantID, ActorType: audit.ActorStaff, ActorID: params.StaffID,
		Action: "kyc.document_reviewed", TargetType: "kyc_document", TargetID: params.DocumentID.String(),
		Outcome:  audit.OutcomeSuccess,
		Metadata: map[string]any{"document_type": string(current.DocumentType), "new_status": string(params.NewStatus), "reason": params.Reason},
	}); err != nil {
		return Document{}, fmt.Errorf("kyc: audit review document: %w", err)
	}
	return GetDocumentByID(ctx, tx, params.DocumentID)
}
