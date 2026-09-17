package idempotency

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// This file closes Security finding S-6 (docs/security/security-
// architecture.md, Stage 4H-B0-R5 section): ADR 0038 §14.1's composed
// key ("{provider reference}#{occurrence_ordinal}") was specified as
// plain string concatenation with no delimiter reservation, escaping
// rule, or charset validation. A provider reference containing the "#"
// delimiter collides two distinct events into the same composed string
// (the finding's own example: ref="A#1" with no ordinal, and ref="A"
// with ordinal "1", both naively compose to "A#1").
//
// The fix is a length-prefixed encoding: the exact byte length of the
// reference is recorded before it, so decomposition always knows exactly
// where the reference ends and the discriminator suffix begins,
// regardless of what bytes the reference itself contains - including
// literal "#", ":", digits, or any other byte sequence that could
// otherwise be mistaken for another valid encoding's structure. No
// charset restriction on the reference is required for this property to
// hold; TestComposeOccurrenceKey_NoTwoDistinctPairsCollide proves it
// exhaustively over adversarial inputs designed to break a naive
// delimiter scheme.

// ErrInvalidComposedKey is returned by DecomposeOccurrenceKey when
// composed is not a well-formed output of ComposeOccurrenceKey.
var ErrInvalidComposedKey = errors.New("idempotency: malformed composed occurrence key")

// ErrEmptyReference is returned by ComposeOccurrenceKey when reference is
// empty - an empty reference carries no distinguishing information at
// all and must never be accepted as one occurrence's identity (it would
// make the length-prefix encoding "0:" for every such reference,
// collapsing every empty-reference occurrence with the same
// discriminator onto the identical composed key by construction, not by
// accident).
var ErrEmptyReference = errors.New("idempotency: provider reference must not be empty")

// occurrenceDiscriminatorSep is the delimiter between the reference's
// length-prefixed bytes and its optional discriminator suffix. It needs
// no reservation/escaping rule of its own precisely because the length
// prefix already fixes the exact byte offset it must appear at (or the
// exact end-of-string if no discriminator is present) - it is a purely
// cosmetic marker for readability (and for the "one discriminator
// present, distinguishable from none" check below), never load-bearing
// for the decomposition's correctness.
const occurrenceDiscriminatorSep = "#"

// occurrenceLengthSep separates the decimal reference-length prefix from
// the reference's own bytes. Same non-load-bearing rationale as
// occurrenceDiscriminatorSep above - decomposition locates it by scanning
// for the first ':' rather than trusting any assumption about what the
// reference itself contains, and validates the prefix is composed only
// of decimal digits before treating it as a length.
const occurrenceLengthSep = ":"

// ComposeOccurrenceKey losslessly encodes reference and an optional
// discriminator (nil for a single-occurrence transaction type that needs
// none - ADR 0038 §14.1's "NULL/not applicable" case) into the single
// opaque string an adapter submits as provider_tx_id (external-provider
// mode) or idempotency_key (in-house mode) - see Mode/Assignment in
// routing.go.
//
// Encoding: "<decimal length of reference in bytes>:<reference bytes>"
// followed by "#<discriminator>" if discriminator is non-nil (which may
// itself be an empty string - that is a valid, distinct occurrence from
// "no discriminator at all", and round-trips correctly).
//
// This is the general-purpose form. ComposeOccurrenceKeyWithOrdinal is a
// convenience wrapper for the common case where the discriminator is
// ADR 0038 §14.1's own strictly-increasing OccurrenceOrdinal rather than
// an arbitrary provider-committed event/message id.
func ComposeOccurrenceKey(reference string, discriminator *string) (string, error) {
	if reference == "" {
		return "", ErrEmptyReference
	}
	var b strings.Builder
	b.WriteString(strconv.Itoa(len(reference)))
	b.WriteString(occurrenceLengthSep)
	b.WriteString(reference)
	if discriminator != nil {
		b.WriteString(occurrenceDiscriminatorSep)
		b.WriteString(*discriminator)
	}
	return b.String(), nil
}

// ComposeOccurrenceKeyWithOrdinal is ComposeOccurrenceKey specialized for
// ADR 0038 §14.1's strictly-increasing-integer OccurrenceOrdinal case.
// ordinal == nil composes with no discriminator at all (the single-
// occurrence case); a non-nil ordinal must be >= 1 (an ordinal of 0 or
// negative is never a legitimate "first occurrence, second occurrence, ..."
// count and is rejected rather than silently accepted). A caller that
// later needs the ordinal back out of a composed key uses the general
// DecomposeOccurrenceKey and parses its string discriminator itself
// (strconv.ParseInt) - this package does not duplicate that handful of
// lines behind a dedicated decompose-with-ordinal function with no
// current caller.
func ComposeOccurrenceKeyWithOrdinal(reference string, ordinal *OccurrenceOrdinal) (string, error) {
	if ordinal == nil {
		return ComposeOccurrenceKey(reference, nil)
	}
	if *ordinal < 1 {
		return "", fmt.Errorf("%w: occurrence ordinal must be >= 1, got %d", ErrInvalidComposedKey, *ordinal)
	}
	d := strconv.FormatInt(int64(*ordinal), 10)
	return ComposeOccurrenceKey(reference, &d)
}

// DecomposeOccurrenceKey is ComposeOccurrenceKey's exact inverse: for any
// composed string it produced, it recovers the identical (reference,
// discriminator) pair. Returns ErrInvalidComposedKey for any input that
// is not a well-formed ComposeOccurrenceKey output - including a raw,
// legacy, un-composed provider reference from before this package
// existed, which callers must not attempt to decompose as if it already
// carried this encoding.
func DecomposeOccurrenceKey(composed string) (reference string, discriminator *string, err error) {
	idx := strings.Index(composed, occurrenceLengthSep)
	if idx < 0 {
		return "", nil, fmt.Errorf("%w: missing length prefix separator", ErrInvalidComposedKey)
	}
	lengthPrefix := composed[:idx]
	if lengthPrefix == "" {
		return "", nil, fmt.Errorf("%w: empty length prefix", ErrInvalidComposedKey)
	}
	for _, r := range lengthPrefix {
		if r < '0' || r > '9' {
			return "", nil, fmt.Errorf("%w: length prefix %q is not decimal", ErrInvalidComposedKey, lengthPrefix)
		}
	}
	refLen, err := strconv.Atoi(lengthPrefix)
	if err != nil {
		return "", nil, fmt.Errorf("%w: parse length prefix: %v", ErrInvalidComposedKey, err)
	}
	if refLen <= 0 {
		// ComposeOccurrenceKey never emits a zero-length reference
		// (ErrEmptyReference rejects it at composition time), so a
		// decomposed length of 0 or negative can only mean malformed or
		// adversarial input, never a legitimate output.
		return "", nil, fmt.Errorf("%w: reference length must be positive, got %d", ErrInvalidComposedKey, refLen)
	}

	remainder := composed[idx+len(occurrenceLengthSep):]
	if len(remainder) < refLen {
		return "", nil, fmt.Errorf("%w: declared reference length %d exceeds remaining input", ErrInvalidComposedKey, refLen)
	}
	reference = remainder[:refLen]
	suffix := remainder[refLen:]

	switch {
	case suffix == "":
		return reference, nil, nil
	case strings.HasPrefix(suffix, occurrenceDiscriminatorSep):
		d := suffix[len(occurrenceDiscriminatorSep):]
		return reference, &d, nil
	default:
		return "", nil, fmt.Errorf("%w: unexpected trailing bytes after reference", ErrInvalidComposedKey)
	}
}
