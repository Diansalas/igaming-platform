package payoutinstrument

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
)

// canon is the k2_canonical length-prefixed encoding (ADR 0111 2.2, M-1): each
// field as "<octet length>:<value>", nil as "~", joined by ",". Identical in
// form to actorproof.Digest's pre-image, so a field's content can never be
// confused with its neighbour's.
func canon(fields ...*string) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		if f == nil {
			parts[i] = "~"
			continue
		}
		parts[i] = strconv.Itoa(len(*f)) + ":" + *f
	}
	return strings.Join(parts, ",")
}

func cs(s string) *string { return &s }

// cu is a lowercase canonical UUID field.
func cu(u uuid.UUID) *string { s := strings.ToLower(u.String()); return &s }

// cup is cu for an optional UUID (nil -> NULL "~").
func cup(u *uuid.UUID) *string {
	if u == nil {
		return nil
	}
	return cu(*u)
}

// ct is the actorproof timestamp encoding (UTC microsecond text).
func ct(t time.Time) *string { return actorproof.TS(&t) }

// carr is a sorted-array field: the elements are sorted, then canon-encoded
// as a nested canonical string, so an array is order-independent and
// unambiguous.
func carr(elems []string) *string {
	cp := append([]string(nil), elems...)
	sort.Strings(cp)
	fields := make([]*string, len(cp))
	for i := range cp {
		fields[i] = &cp[i]
	}
	s := canon(fields...)
	return &s
}

func itoa(n int) string { return strconv.Itoa(n) }
