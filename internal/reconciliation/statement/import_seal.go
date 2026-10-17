package statement

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PAY-PAYOUT-UNBOUND-RESOLVE-1 (ADR 0111 4.3, S-5; ADR 0110 T10): the
// canonical content of a sealed payment statement import. Pure data and
// encoding only - no key: the HMAC is computed by an ImportSealer (the B13 key
// module, internal/payoutinstrument), so the reconciliation package stays free
// of key material and of any package that could move money (R3).
//
// Seal input: canon("imp", import id, tenant, provider, source_label, is_mock,
// coverage_start, coverage_end, content_digest, line_count, fetched_at,
// payout_lines_carry_merchant_reference, imported_by_service, lines_digest).
// lines_digest is SHA-256 over the canonical encoding, in line_no order, of
// EVERY line column the M4 verdict or reconciliation reads (line_no,
// provider_id - review amendment L-1/sec: a line re-pointed at another
// provider breaks the seal -, kind, provider_reference, merchant_reference, original_provider_reference,
// settlement_reference, status, amount, asset_code, occurred_at). Any column
// later added to the verdict must join the digest in the same change. The
// encoding is the k2_canonical form ("<octet length>:<value>", NULL as "~",
// joined by ","); timestamps are UTC microsecond text.

// ImportLineCanon is one stored statement line as the digest reads it.
type ImportLineCanon struct {
	LineNo                    int
	ProviderID                string
	Kind                      string
	ProviderReference         string
	MerchantReference         *string
	OriginalProviderReference *string
	SettlementReference       *string
	Status                    string
	Amount                    string // decimal minor units, as stored (NUMERIC(38,0))
	AssetCode                 string
	OccurredAt                time.Time
}

// ImportSealInput is the sealed import row plus its lines digest.
type ImportSealInput struct {
	ImportID                          uuid.UUID
	TenantID                          uuid.UUID
	ProviderID                        string
	SourceLabel                       string
	IsMock                            bool
	CoverageStart                     time.Time
	CoverageEnd                       time.Time
	ContentDigest                     []byte
	LineCount                         int
	FetchedAt                         time.Time
	PayoutLinesCarryMerchantReference bool
	ImportedByService                 string
	LinesDigest                       string
}

// ImportSealer seals an import's canonical content (implemented by
// payoutinstrument.Keys). It returns the hex seal and the key id used.
type ImportSealer interface {
	SealStatementImport(in ImportSealInput) (seal, kid string, err error)
}

// ImportSealVerifier verifies a stored import seal over in (implemented by
// payoutinstrument.Keys; constant-time). Any failure - unknown kid, empty
// seal, mismatch - is an error. F2 / SR11-10: the payment_statement cursor
// trusts only a run row whose backing import carries a seal that verifies.
type ImportSealVerifier interface {
	VerifyStatementImport(in ImportSealInput, kid, seal string) error
}

// ImportSealKeys both seals and verifies (payoutinstrument.Keys).
type ImportSealKeys interface {
	ImportSealer
	ImportSealVerifier
}

// RunSealInput is one payment_statement run row as its seal reads it (F2 / SRF2-1, migration 0168): the run id,
// tenant, provider, stream, the import the run matched, the import's coverage window, window_truncated and run_at.
// Every column the cursor or the M2 reconciliation-currency gate reads is inside the seal.
type RunSealInput struct {
	RunID           uuid.UUID
	TenantID        uuid.UUID
	ProviderID      string
	Stream          string
	ImportID        uuid.UUID
	CoverageStart   time.Time
	CoverageEnd     time.Time
	WindowTruncated bool
	RunAt           time.Time
}

// RunSealer seals a run row (payoutinstrument.Keys); RunSealVerifier verifies one (constant time).
type RunSealer interface {
	SealPaymentRun(in RunSealInput) (seal, kid string, err error)
}

// RunSealVerifier verifies a stored run seal; any failure is an error.
type RunSealVerifier interface {
	VerifyPaymentRun(in RunSealInput, kid, seal string) error
}

// StreamSealKeys is everything the payment_statement stream needs from the key module: import and run seals.
type StreamSealKeys interface {
	ImportSealKeys
	RunSealer
	RunSealVerifier
}

// RunCanon is the canonical run-seal pre-image of in (domain tag "run").
func RunCanon(in RunSealInput) string {
	return canonJoin(sp("run"), sp(strings.ToLower(in.RunID.String())), sp(strings.ToLower(in.TenantID.String())), sp(in.ProviderID),
		sp(in.Stream), sp(strings.ToLower(in.ImportID.String())), canonTime(in.CoverageStart), canonTime(in.CoverageEnd),
		sp(strconv.FormatBool(in.WindowTruncated)), canonTime(in.RunAt))
}

func canonField(f *string) string {
	if f == nil {
		return "~"
	}
	return strconv.Itoa(len(*f)) + ":" + *f
}

func canonJoin(fields ...*string) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = canonField(f)
	}
	return strings.Join(parts, ",")
}

func sp(s string) *string { return &s }

func canonTime(t time.Time) *string {
	s := t.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
	return &s
}

// ImportLinesDigest returns the hex SHA-256 over every line's canonical
// encoding, in the order given (callers pass line_no order). Each line is
// length-prefixed, so line boundaries cannot be confused.
func ImportLinesDigest(lines []ImportLineCanon) string {
	h := sha256.New()
	for i := range lines {
		l := &lines[i]
		c := canonJoin(sp("line"), sp(strconv.Itoa(l.LineNo)), sp(l.ProviderID), sp(l.Kind), sp(l.ProviderReference), l.MerchantReference,
			l.OriginalProviderReference, l.SettlementReference, sp(l.Status), sp(l.Amount), sp(l.AssetCode), canonTime(l.OccurredAt))
		h.Write([]byte(strconv.Itoa(len(c))))
		h.Write([]byte{':'})
		h.Write([]byte(c))
		h.Write([]byte{','})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ImportCanon is the canonical seal pre-image of in (domain tag "imp", S-5).
func ImportCanon(in ImportSealInput) string {
	return canonJoin(sp("imp"), sp(strings.ToLower(in.ImportID.String())), sp(strings.ToLower(in.TenantID.String())),
		sp(in.ProviderID), sp(in.SourceLabel), sp(strconv.FormatBool(in.IsMock)), canonTime(in.CoverageStart), canonTime(in.CoverageEnd),
		sp(hex.EncodeToString(in.ContentDigest)), sp(strconv.Itoa(in.LineCount)), canonTime(in.FetchedAt),
		sp(strconv.FormatBool(in.PayoutLinesCarryMerchantReference)), sp(in.ImportedByService), sp(in.LinesDigest))
}
