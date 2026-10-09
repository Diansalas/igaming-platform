package reconciliation

import (
	"strconv"
	"sync"
	"time"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// PAY-PAYOUT-UNBOUND-RESOLVE-1 (ADR 0111 4.3, S-3/S-5; ADR 0110 T10): sealed
// statement imports.
//
// The importer writes the Go import seal IN the import INSERT (migration 0125
// columns import_seal, seal_kid), together with the source declaration that
// comes ONLY from the in-process source value (is_mock from the Synthetic
// marker, payout_lines_carry_merchant_reference from the optional
// payoutMerchantReferenceDeclarer interface) and the importing service's name.
// Without keys (no PAYOUT_INSTRUMENT_KEYS in a dev/MOCK process) the import is
// stored unsealed: reconciliation still reads it, but it is never eligible M4
// evidence (fail closed). The startup gate requires the keys whenever a
// non-MOCK statement source is registered.

// ImportedByServicePaymentStatement is the imported_by_service of every import
// written by IngestPaymentStatement.
const ImportedByServicePaymentStatement = "reconciliation.payment_statement"

// payoutMerchantReferenceDeclarer is the optional source declaration (S-3): a
// source whose payout lines always carry the platform's merchant reference says
// so in code. Absent = false (the not-paid verdict then stays insufficient).
type payoutMerchantReferenceDeclarer interface {
	PayoutLinesCarryMerchantReference() bool
}

func declaresPayoutMerchantReference(src statement.PaymentStatementSource) bool {
	d, ok := src.(payoutMerchantReferenceDeclarer)
	return ok && d.PayoutLinesCarryMerchantReference()
}

var (
	defaultImportSealerMu sync.RWMutex
	defaultImportSealer   statement.ImportSealer
)

// SetDefaultImportSealer installs the process-wide statement-import sealer
// (cmd/platform-api, once, from the B13 key module). nil = imports unsealed.
func SetDefaultImportSealer(k statement.ImportSealer) {
	defaultImportSealerMu.Lock()
	defaultImportSealer = k
	defaultImportSealerMu.Unlock()
}

// DefaultImportSealer returns the process-wide sealer (nil when none).
func DefaultImportSealer() statement.ImportSealer {
	defaultImportSealerMu.RLock()
	defer defaultImportSealerMu.RUnlock()
	return defaultImportSealer
}

// statementLinesCanon renders the statement's lines (line_no = index) exactly as
// they will be stored and later re-read.
func statementLinesCanon(stmt statement.PaymentStatement) []statement.ImportLineCanon {
	out := make([]statement.ImportLineCanon, len(stmt.Lines))
	for i, l := range stmt.Lines {
		out[i] = statement.ImportLineCanon{
			LineNo: i, Kind: l.Kind, ProviderReference: l.ProviderReference,
			MerchantReference: nullIfEmpty(l.MerchantReference), OriginalProviderReference: nullIfEmpty(l.OriginalProviderReference),
			SettlementReference: nullIfEmpty(l.SettlementReference), Status: l.Status,
			Amount: strconv.FormatInt(l.Amount, 10), AssetCode: l.AssetCode, OccurredAt: l.OccurredAt.UTC().Truncate(time.Microsecond),
		}
	}
	return out
}
