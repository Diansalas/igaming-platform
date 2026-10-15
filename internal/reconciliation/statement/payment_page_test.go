package statement

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func pl(ref string, at time.Time) PaymentStatementLine {
	return PaymentStatementLine{ProviderID: "p", ProviderReference: ref, OccurredAt: at}
}

func TestPagePaymentLines_UnboundedReturnsAllSorted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := []PaymentStatementLine{pl("c", t0.Add(2*time.Second)), pl("a", t0), pl("b", t0.Add(time.Second))}
	page, cutoff, trunc, err := PagePaymentLines(in, 0)
	if err != nil || trunc || !cutoff.IsZero() || len(page) != 3 || page[0].ProviderReference != "a" || page[2].ProviderReference != "c" {
		t.Fatalf("unbounded: %v %v %v %v", page, cutoff, trunc, err)
	}
	if in[0].ProviderReference != "c" {
		t.Fatal("input must not be modified")
	}
	// A bound equal to the size is not a truncation.
	if _, _, trunc, _ := PagePaymentLines(in, 3); trunc {
		t.Fatal("len == bound must not be truncated")
	}
}

func TestPagePaymentLines_CutsAtInstantBoundary_NoSplitNoSkipNoDouble(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var in []PaymentStatementLine
	// instants: 0:1 line, 1:3 lines (straddles a bound of 3 after the first), 2:2 lines, 3:1 line
	for i, n := range []int{1, 3, 2, 1} {
		for j := 0; j < n; j++ {
			in = append(in, pl(fmt.Sprintf("r%d-%d", i, j), t0.Add(time.Duration(i)*time.Second)))
		}
	}
	// Walk the whole statement page by page: every line exactly once.
	seen := map[string]int{}
	rest := in
	cursor := time.Time{}
	pages := 0
	for len(rest) > 0 {
		pages++
		if pages > 10 {
			t.Fatal("did not terminate")
		}
		page, cutoff, trunc, err := PagePaymentLines(rest, 3)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		for _, l := range page {
			seen[l.ProviderReference]++
			if !cursor.IsZero() && l.OccurredAt.Before(cursor) {
				t.Fatalf("line %s before the cursor", l.ProviderReference)
			}
			if trunc && !l.OccurredAt.Before(cutoff) {
				t.Fatalf("line %s at or after the cutoff in the page", l.ProviderReference)
			}
		}
		if !trunc {
			break
		}
		if !cutoff.After(cursor) {
			t.Fatal("cutoff must advance strictly")
		}
		cursor = cutoff
		var next []PaymentStatementLine
		for _, l := range rest {
			if !l.OccurredAt.Before(cutoff) {
				next = append(next, l)
			}
		}
		rest = next
	}
	if len(seen) != len(in) {
		t.Fatalf("lines seen %d, want %d", len(seen), len(in))
	}
	for ref, n := range seen {
		if n != 1 {
			t.Fatalf("line %s seen %d times", ref, n)
		}
	}
	if pages < 2 {
		t.Fatalf("the bound must have forced several pages, got %d", pages)
	}
}

func TestPagePaymentLines_UnsplittableGroupFailsClosed(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := []PaymentStatementLine{pl("a", at), pl("b", at), pl("c", at), pl("d", at.Add(time.Second))}
	_, _, _, err := PagePaymentLines(in, 2)
	if !errors.Is(err, ErrPaymentStatementUnpageable) {
		t.Fatalf("a first group larger than the bound must be refused, got %v", err)
	}
}

func TestPagePaymentLines_SubMicrosecondDifferencesAreOneInstant(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 1000, time.UTC)
	in := []PaymentStatementLine{pl("a", at), pl("b", at.Add(300*time.Nanosecond)), pl("c", at.Add(time.Second))}
	if _, _, _, err := PagePaymentLines(in, 1); !errors.Is(err, ErrPaymentStatementUnpageable) {
		t.Fatalf("two lines in the same microsecond must not be separated: %v", err)
	}
}

// R3-OBS-BOUND-1 / A11 read-only requirement: a payment statement source cannot perform writes BY
// CONSTRUCTION. Its whole method set is Label, ProviderID and Fetch(ctx, PaymentFetchRequest); neither the
// request nor the statement carries a transaction, a callback, a channel, a pointer or an interface through
// which a database or provider write could be reached. A new method or field here must be a conscious
// decision that changes this test.
func TestPaymentStatementSource_TypeLevelReadOnly(t *testing.T) {
	it := reflect.TypeOf((*PaymentStatementSource)(nil)).Elem()
	var names []string
	for i := 0; i < it.NumMethod(); i++ {
		names = append(names, it.Method(i).Name)
	}
	if fmt.Sprint(names) != "[Fetch Label ProviderID]" {
		t.Fatalf("PaymentStatementSource method set changed: %v", names)
	}
	fetch, _ := it.MethodByName("Fetch")
	if fetch.Type.NumIn() != 2 || fetch.Type.NumOut() != 2 ||
		fetch.Type.In(0).String() != "context.Context" || fetch.Type.In(1) != reflect.TypeOf(PaymentFetchRequest{}) ||
		fetch.Type.Out(0) != reflect.TypeOf(PaymentStatement{}) || fetch.Type.Out(1).String() != "error" {
		t.Fatalf("Fetch signature changed: %v", fetch.Type)
	}
	var walk func(rt reflect.Type, path string)
	walk = func(rt reflect.Type, path string) {
		switch rt.Kind() {
		case reflect.Struct:
			if rt == reflect.TypeOf(time.Time{}) {
				return
			}
			for i := 0; i < rt.NumField(); i++ {
				walk(rt.Field(i).Type, path+"."+rt.Field(i).Name)
			}
		case reflect.Slice, reflect.Array:
			walk(rt.Elem(), path+"[]")
		case reflect.String, reflect.Bool, reflect.Int, reflect.Int64, reflect.Uint8:
		default:
			// uuid.UUID is [16]byte (array of Uint8); anything else (func, chan, ptr, interface, map) is a write path.
			t.Errorf("%s has kind %s: a statement request/response must be plain data", path, rt.Kind())
		}
	}
	walk(reflect.TypeOf(PaymentFetchRequest{}), "PaymentFetchRequest")
	walk(reflect.TypeOf(PaymentStatement{}), "PaymentStatement")
}
