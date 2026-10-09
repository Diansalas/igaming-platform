package payoutinstrument

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// ErrStatementImportUnsealed is returned by VerifyStatementImportInTx for
// an import that carries no seal at all.
var ErrStatementImportUnsealed = errors.New("payoutinstrument: payment statement import is unsealed")

// VerifyStatementImportInTx re-reads import importID and ALL its lines
// (line_no order) in tx's session and verifies the Go seal over them (ADR 0111
// 4.3: "the Go executor verifies the seal and recomputes lines_digest"). Any
// failure - no keys, no seal, unknown kid, a stored line count that differs,
// a tampered column - is an error (fail closed). The import must be visible to
// the session.
func VerifyStatementImportInTx(ctx context.Context, tx pgx.Tx, keys *Keys, tenantID, importID uuid.UUID) error {
	if keys == nil {
		return fmt.Errorf("%w: no import seal keys in this process", ErrImportSealInvalid)
	}
	in := statement.ImportSealInput{ImportID: importID, TenantID: tenantID}
	var seal, kid, service *string
	if err := tx.QueryRow(ctx, `
		SELECT provider_id, source_label, is_mock, coverage_start, coverage_end, content_digest, line_count, fetched_at,
		       payout_lines_carry_merchant_reference, imported_by_service, import_seal, seal_kid
		  FROM payment_statement_imports WHERE id = $1 AND tenant_id = $2`, importID, tenantID).
		Scan(&in.ProviderID, &in.SourceLabel, &in.IsMock, &in.CoverageStart, &in.CoverageEnd, &in.ContentDigest, &in.LineCount,
			&in.FetchedAt, &in.PayoutLinesCarryMerchantReference, &service, &seal, &kid); err != nil {
		return fmt.Errorf("payoutinstrument: read statement import %s for seal verification: %w", importID, err)
	}
	if seal == nil || kid == nil || service == nil {
		return fmt.Errorf("%w: import %s", ErrStatementImportUnsealed, importID)
	}
	in.ImportedByService = *service
	rows, err := tx.Query(ctx, `
		SELECT line_no, provider_id, kind, provider_reference, merchant_reference, original_provider_reference, settlement_reference,
		       status, amount::text, asset_code, occurred_at
		  FROM payment_statement_lines WHERE tenant_id = $1 AND import_id = $2 ORDER BY line_no`, tenantID, importID)
	if err != nil {
		return fmt.Errorf("payoutinstrument: read statement lines for seal verification: %w", err)
	}
	var lines []statement.ImportLineCanon
	for rows.Next() {
		var l statement.ImportLineCanon
		if err := rows.Scan(&l.LineNo, &l.ProviderID, &l.Kind, &l.ProviderReference, &l.MerchantReference, &l.OriginalProviderReference,
			&l.SettlementReference, &l.Status, &l.Amount, &l.AssetCode, &l.OccurredAt); err != nil {
			rows.Close()
			return err
		}
		lines = append(lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(lines) != in.LineCount {
		return fmt.Errorf("%w: import %s declares %d lines, %d visible", ErrImportSealInvalid, importID, in.LineCount, len(lines))
	}
	in.LinesDigest = statement.ImportLinesDigest(lines)
	if err := keys.VerifyStatementImport(in, *kid, *seal); err != nil {
		return fmt.Errorf("%w: import %s", err, importID)
	}
	return nil
}
