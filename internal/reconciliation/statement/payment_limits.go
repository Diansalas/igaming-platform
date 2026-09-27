package statement

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Security condition C1 (docs/plans/payment-readiness/rv-prh-i5-security.md):
// a payment statement source that reads a statement over the wire MUST NOT
// buffer an unbounded body or build an unbounded line slice before the
// stream's own line cap runs. These helpers are the contract's enforcement
// primitives: a real PaymentStatementSource reads its response body through
// LimitPaymentStatementBody and collects lines with a PaymentLineCollector
// (or uses DecodePaymentStatementJSONLines, which does both). Either
// sentinel returned from Fetch makes the stream refuse the import with
// nothing stored (a P1), exactly like its own line cap.

// ErrPaymentStatementBodyTooLarge means the wire body exceeded the byte cap.
var ErrPaymentStatementBodyTooLarge = errors.New("statement: payment statement body exceeds the byte cap")

// ErrPaymentStatementTooManyLines means the statement exceeded the line cap.
var ErrPaymentStatementTooManyLines = errors.New("statement: payment statement exceeds the line cap")

// LimitPaymentStatementBody wraps body so that at most maxBytes bytes are
// ever delivered: it reads through io.LimitReader(body, maxBytes+1) and
// returns ErrPaymentStatementBodyTooLarge as soon as a byte beyond maxBytes
// arrives, instead of silently truncating. maxBytes <= 0 or above
// MaxPaymentStatementBodyBytes means MaxPaymentStatementBodyBytes.
func LimitPaymentStatementBody(body io.Reader, maxBytes int64) io.Reader {
	if maxBytes <= 0 || maxBytes > MaxPaymentStatementBodyBytes {
		maxBytes = MaxPaymentStatementBodyBytes
	}
	return &boundedBody{r: io.LimitReader(body, maxBytes+1), max: maxBytes}
}

type boundedBody struct {
	r    io.Reader
	max  int64
	read int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += int64(n)
	if b.read > b.max {
		return n - int(b.read-b.max), ErrPaymentStatementBodyTooLarge
	}
	return n, err
}

// PaymentLineCollector accumulates lines up to a cap and refuses the first
// line beyond it, so a source stops decoding at maxLines+1.
type PaymentLineCollector struct {
	max   int
	lines []PaymentStatementLine
}

// NewPaymentLineCollector caps at maxLines (<= 0 or above
// MaxPaymentStatementLines means MaxPaymentStatementLines).
func NewPaymentLineCollector(maxLines int) *PaymentLineCollector {
	if maxLines <= 0 || maxLines > MaxPaymentStatementLines {
		maxLines = MaxPaymentStatementLines
	}
	return &PaymentLineCollector{max: maxLines}
}

// Add appends l, or returns ErrPaymentStatementTooManyLines when the cap is
// already reached (the line is not kept).
func (c *PaymentLineCollector) Add(l PaymentStatementLine) error {
	if len(c.lines) >= c.max {
		return fmt.Errorf("%w: more than %d lines", ErrPaymentStatementTooManyLines, c.max)
	}
	c.lines = append(c.lines, l)
	return nil
}

// Lines returns the collected lines.
func (c *PaymentLineCollector) Lines() []PaymentStatementLine { return c.lines }

// DecodePaymentStatementJSONLines streams a body of consecutive JSON values
// (NDJSON or concatenated JSON), one per statement line, decoding each with
// decodeLine. It reads through LimitPaymentStatementBody(body, maxBytes) and
// a PaymentLineCollector(maxLines): it never takes in more than the byte cap
// or keeps more than maxLines decoded lines, and stops at the first value
// past either cap.
func DecodePaymentStatementJSONLines(body io.Reader, maxBytes int64, maxLines int, decodeLine func(json.RawMessage) (PaymentStatementLine, error)) ([]PaymentStatementLine, error) {
	dec := json.NewDecoder(LimitPaymentStatementBody(body, maxBytes))
	c := NewPaymentLineCollector(maxLines)
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return c.Lines(), nil
		}
		if err != nil {
			if errors.Is(err, ErrPaymentStatementBodyTooLarge) {
				return nil, err
			}
			return nil, fmt.Errorf("statement: decode payment statement line %d: %w", len(c.Lines()), err)
		}
		l, err := decodeLine(raw)
		if err != nil {
			return nil, fmt.Errorf("statement: payment statement line %d: %w", len(c.Lines()), err)
		}
		if err := c.Add(l); err != nil {
			return nil, err
		}
	}
}
