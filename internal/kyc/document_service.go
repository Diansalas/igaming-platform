package kyc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/providercred"
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
// malware-scans, stores (via storage), and records a new kyc_documents row -
// a version-incremented NEW row if this player_account_id already has a
// document of this DocumentType (directive §5: never overwrite history),
// version 1 otherwise. tx must already be tenant-scoped.
//
// ADR 0095 §15.3 (PRH-I2): this function is phase A ONLY now - it no longer
// calls KYCProvider.SubmitVerification itself. That call (phase B, with no
// database transaction held across it) is the separate, pool-based
// SubmitVerification function below; a caller that wants the pre-split
// "upload, then submit to the provider" behaviour calls UploadDocument
// (inside its own short transaction) and then SubmitVerification
// (pool-based) afterward - exactly how internal/httpserver's upload handler
// is wired. This split is what actually removes the DB-transaction-held-
// across-a-provider-call hazard §1 of ADR 0095 named; the previous single-
// transaction shape is superseded, not merely renamed.
func UploadDocument(ctx context.Context, tx pgx.Tx, storage DocumentStorageProvider, scanner MalwareScanner, params UploadDocumentParams) (Document, error) {
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

	return scanDocument(tx.QueryRow(ctx, `SELECT `+documentColumns+` FROM kyc_documents WHERE id = $1`, d.ID))
}

// gatherSubmissionDocuments is SubmitVerification's phase-A read: the
// verification (terminal-guard) and its CURRENT full set of non-rejected
// documents (not just the one just uploaded - a real vendor evaluates the
// accumulated evidence, not one file in isolation), read in a short,
// read-only, tenant-scoped transaction. Returns ok=false (no error) when
// the verification is already terminal - SubmitVerification's own no-op
// case, mirroring updateVerificationStatus's identical idempotency
// convention.
func gatherSubmissionDocuments(ctx context.Context, tx pgx.Tx, verificationID uuid.UUID) (v Verification, submitted []SubmittedDocument, ok bool, err error) {
	v, err = GetVerificationByID(ctx, tx, verificationID)
	if err != nil {
		return Verification{}, nil, false, err
	}
	if isTerminal(v.Status) {
		return v, nil, false, nil
	}

	rows, err := tx.Query(ctx, `SELECT id, document_type FROM kyc_documents WHERE verification_id = $1 AND status <> $2 ORDER BY id`,
		verificationID, DocumentRejected)
	if err != nil {
		return Verification{}, nil, false, fmt.Errorf("kyc: list documents for submission: %w", err)
	}
	for rows.Next() {
		var sd SubmittedDocument
		if err := rows.Scan(&sd.DocumentID, &sd.DocumentType); err != nil {
			rows.Close()
			return Verification{}, nil, false, fmt.Errorf("kyc: scan document for submission: %w", err)
		}
		submitted = append(submitted, sd)
	}
	if err := rows.Err(); err != nil {
		return Verification{}, nil, false, fmt.Errorf("kyc: list documents for submission: %w", err)
	}
	return v, submitted, true, nil
}

// submissionIdempotencyKey is ADR 0095 §15.3's content-derived idempotency
// key: "ks:" + verification_id + ":" + sha256(sorted document ids) - the
// SAME document set always derives the SAME key, so a retry submitting an
// unchanged set is recognizable to a vendor that honors idempotency keys,
// while a set that has since grown or shrunk (a new upload, a document
// rejected) derives a genuinely different key.
func submissionIdempotencyKey(verificationID uuid.UUID, docs []SubmittedDocument) string {
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = d.DocumentID.String()
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		_, _ = h.Write([]byte(id))
		_, _ = h.Write([]byte{0})
	}
	return "ks:" + verificationID.String() + ":" + hex.EncodeToString(h.Sum(nil))
}

// applySubmissionResult is SubmitVerification's phase C: an ambiguous,
// timeout, or transport-error result (ProviderError - IC condition 2, ADR
// 0095 §15.3) is mapped to the SAME "no state change, one failure audit
// row" handling document_service.go's pre-split code already used - it is
// NEVER passed to statusForOutcome and NEVER read as a definitive outcome,
// so kyc_verifications.status is left exactly as it was. A definitive
// outcome applies the existing terminal-guarded updateVerificationStatus
// plus its own success audit row - identical to the pre-split behaviour,
// just moved into its own short, bounded transaction.
func applySubmissionResult(ctx context.Context, tx pgx.Tx, v Verification, submitted []SubmittedDocument, result ProviderResult) (Verification, error) {
	result, reasonTruncated := normalizeProviderResult(result)

	auditOutcome := audit.OutcomeSuccess
	if result.Outcome == ProviderError {
		auditOutcome = audit.OutcomeFailure
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: v.TenantID, ActorType: audit.ActorSystem,
		Action: "kyc.verification_submitted_to_provider", TargetType: "kyc_verification", TargetID: v.ID.String(),
		Outcome:  auditOutcome,
		Metadata: withReasonTruncated(map[string]any{"provider_id": v.ProviderID, "document_count": len(submitted), "provider_outcome": string(result.Outcome), "reason": result.Reason}, reasonTruncated),
	}); err != nil {
		return Verification{}, fmt.Errorf("kyc: audit provider submission: %w", err)
	}

	if result.Outcome == ProviderError {
		// IC condition 2: an ambiguous/timeout/transport-error result is
		// NEVER read by statusForOutcome, and kyc_verifications.status is
		// left exactly as it was - identical to today's ProviderError
		// handling. The next document upload re-submits the full current
		// set under a new content-derived key (KYC-SUBMIT-OUTBOX-1 stays
		// deferred, ADR 0095 §15.3/§25 condition 5: a durable submission
		// outbox is a HARD PRECONDITION on the first real KYC adapter, not
		// merely a flagged future item - no real adapter is accepted into
		// this or any later stage without one landing first).
		return v, nil
	}
	newStatus, ok := statusForOutcome(result.Outcome)
	if !ok {
		return Verification{}, fmt.Errorf("kyc: provider returned an unrecognized outcome %q", result.Outcome)
	}
	return updateVerificationStatus(ctx, tx, v.ID, newStatus, result.Reason)
}

// SubmitVerification is ADR 0095 §15.3's phase A(read)/B/C split of the
// former inline submitVerificationDocuments step (PRH-I2): a short,
// read-only transaction gathers the verification's terminal-guard and its
// current non-rejected document set (phase A); the provider is called with
// NO database transaction/pooled connection held, using a per-call outbound
// credential resolved via outbound (PROV-OUTBOUND-CRED-1) and a
// content-derived idempotency key (phase B); the result is applied under a
// short, bounded, ctx-independent transaction (phase C).
//
// IC condition 2 (ADR 0095 §25): an ambiguous, timeout, or transport-error
// result leaves kyc_verifications.status UNCHANGED - see
// applySubmissionResult's own doc comment. Callers: internal/httpserver's
// document-upload handler, after UploadDocument's own transaction has
// already committed the new document row.
//
// pool/outbound/provider follow CreateVerification's identical nil-fails-
// closed convention. A verification already in a terminal status, or one
// with no current non-rejected documents to submit at all (the caller races
// a rejection, or has none yet), is a documented no-op returning the
// verification unchanged and a nil error - never an error for "nothing to
// do".
// tenantID must be the caller's own server-resolved tenant (the same scope
// the verification row belongs to) - never client-supplied; it scopes every
// transaction this function opens via pool.WithTenant, mirroring every
// other tenant-scoped entry point in this codebase.
func SubmitVerification(ctx context.Context, pool providercred.TenantTxRunner, outbound OutboundCredentialResolver, provider KYCProvider, tenantID uuid.UUID, verificationID uuid.UUID) (Verification, error) {
	if provider == nil {
		return Verification{}, fmt.Errorf("%w: no KYC provider configured", ErrProviderUnavailable)
	}
	if pool == nil {
		return Verification{}, fmt.Errorf("%w: KYC submission has no transaction runner configured", ErrProviderUnavailable)
	}
	if tenantID == uuid.Nil {
		return Verification{}, fmt.Errorf("%w: tenant_id is required", ErrInvalidTransition)
	}

	var (
		v         Verification
		submitted []SubmittedDocument
		proceed   bool
	)
	if err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v, submitted, proceed, err = gatherSubmissionDocuments(ctx, tx, verificationID)
		return err
	}); err != nil {
		return Verification{}, err
	}
	if !proceed {
		return v, nil
	}
	providerID := provider.ID()

	if outbound == nil {
		return Verification{}, fmt.Errorf("%w: no outbound credential resolver configured", ErrProviderUnavailable)
	}
	cred, err := outbound.Resolve(ctx, pool, tenantID, providerID)
	if err != nil {
		slog.Default().Warn("kyc_submit_verification_credential_unavailable",
			"tenant_id", tenantID.String(), "verification_id", v.ID.String(), "provider_id", providerID)
		return Verification{}, fmt.Errorf("%w: resolve outbound credential: %v", ErrProviderUnavailable, err)
	}
	if cred.TenantID != tenantID || cred.ProviderID != providerID || cred.Domain != "kyc" {
		return Verification{}, fmt.Errorf("%w: outbound credential binding mismatch", ErrProviderUnavailable)
	}

	call := CallContext{
		TenantID: tenantID, ProviderID: providerID, Credential: cred,
		IdempotencyKey: submissionIdempotencyKey(v.ID, submitted), Deadline: time.Now().Add(defaultProviderCallTimeout),
	}
	result, err := provider.SubmitVerification(ctx, v.ProviderReference, submitted, call)
	if err != nil {
		// A transport-level failure here is IC condition 2's own case,
		// applied without ever reaching applySubmissionResult (there is no
		// ProviderResult to apply): kyc_verifications.status is left
		// completely untouched, exactly like a returned ProviderError
		// outcome, since neither ever reaches statusForOutcome. No audit
		// row is written for this specific failure (mirrors
		// CreateVerification's identical phase-B-failure silence) - the
		// next upload re-submits the current document set.
		slog.Default().Warn("kyc_submit_verification_provider_call_failed",
			"tenant_id", tenantID.String(), "verification_id", v.ID.String(), "provider_id", providerID)
		return v, fmt.Errorf("%w: submit verification to provider: %v", ErrProviderUnavailable, err)
	}

	phaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), phaseCTimeout)
	defer cancel()
	var applied Verification
	if err := pool.WithTenant(phaseCtx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		applied, err = applySubmissionResult(ctx, tx, v, submitted, result)
		return err
	}); err != nil {
		slog.Default().Error("kyc_submit_verification_phase_c_failed",
			"tenant_id", tenantID.String(), "verification_id", v.ID.String(), "error", err.Error())
		return Verification{}, fmt.Errorf("kyc: submit verification: apply result: %w", err)
	}
	return applied, nil
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
