package statement

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// endlessLines is an infinite NDJSON body: it never returns EOF, so a
// decoder that buffered the whole body (or every line) would never
// return. Reaching either cap is the only way out.
type endlessLines struct {
	line []byte
	pos  int
	read int64
}

func (e *endlessLines) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], e.line[e.pos:])
		n += c
		e.pos = (e.pos + c) % len(e.line)
	}
	e.read += int64(n)
	return n, nil
}

func decodeTestLine(raw json.RawMessage) (PaymentStatementLine, error) {
	var l struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return PaymentStatementLine{}, err
	}
	return PaymentStatementLine{ProviderReference: l.Ref}, nil
}

func TestLimitPaymentStatementBody_ExactCapPassesOneMoreByteFails(t *testing.T) {
	b, err := io.ReadAll(LimitPaymentStatementBody(strings.NewReader("12345"), 5))
	if err != nil || string(b) != "12345" {
		t.Fatalf("a body of exactly the cap must pass: %q %v", b, err)
	}
	b, err = io.ReadAll(LimitPaymentStatementBody(strings.NewReader("123456"), 5))
	if !errors.Is(err, ErrPaymentStatementBodyTooLarge) {
		t.Fatalf("one byte over the cap must fail with ErrPaymentStatementBodyTooLarge, got %v", err)
	}
	if len(b) > 5 {
		t.Fatalf("no byte beyond the cap may be delivered, got %d", len(b))
	}
}

func TestLimitPaymentStatementBody_DefaultsToMax(t *testing.T) {
	r := LimitPaymentStatementBody(strings.NewReader(""), 0).(*boundedBody)
	if r.max != MaxPaymentStatementBodyBytes {
		t.Fatalf("expected the default cap %d, got %d", MaxPaymentStatementBodyBytes, r.max)
	}
}

func TestPaymentLineCollector_RefusesLinePastCap(t *testing.T) {
	c := NewPaymentLineCollector(2)
	for i := 0; i < 2; i++ {
		if err := c.Add(PaymentStatementLine{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Add(PaymentStatementLine{}); !errors.Is(err, ErrPaymentStatementTooManyLines) {
		t.Fatalf("expected ErrPaymentStatementTooManyLines, got %v", err)
	}
	if len(c.Lines()) != 2 {
		t.Fatalf("the refused line must not be kept, got %d", len(c.Lines()))
	}
	if NewPaymentLineCollector(0).max != MaxPaymentStatementLines {
		t.Fatal("a zero cap must default to MaxPaymentStatementLines")
	}
}

// Streaming line cap: an endless body with a generous byte cap stops at
// maxLines+1, having read only a bounded prefix.
func TestDecodePaymentStatementJSONLines_StopsAtLineCapWhileStreaming(t *testing.T) {
	body := &endlessLines{line: []byte(`{"ref":"r"}` + "\n")}
	_, err := DecodePaymentStatementJSONLines(body, 1<<20, 10, decodeTestLine)
	if !errors.Is(err, ErrPaymentStatementTooManyLines) {
		t.Fatalf("expected ErrPaymentStatementTooManyLines, got %v", err)
	}
	if body.read > 1<<16 {
		t.Fatalf("the decoder read %d bytes to find 11 lines; it must stream, not buffer", body.read)
	}
}

// Streaming byte cap: an endless body with a generous line cap stops at the
// byte cap.
func TestDecodePaymentStatementJSONLines_StopsAtByteCap(t *testing.T) {
	body := &endlessLines{line: []byte(`{"ref":"r"}` + "\n")}
	_, err := DecodePaymentStatementJSONLines(body, 4096, MaxPaymentStatementLines, decodeTestLine)
	if !errors.Is(err, ErrPaymentStatementBodyTooLarge) {
		t.Fatalf("expected ErrPaymentStatementBodyTooLarge, got %v", err)
	}
	if body.read > 4096+1 {
		t.Fatalf("read %d bytes past a 4096-byte cap", body.read)
	}
}

func TestDecodePaymentStatementJSONLines_WithinCaps(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 3; i++ {
		buf.WriteString(`{"ref":"r"}` + "\n")
	}
	lines, err := DecodePaymentStatementJSONLines(&buf, 1024, 3, decodeTestLine)
	if err != nil || len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d %v", len(lines), err)
	}
}
