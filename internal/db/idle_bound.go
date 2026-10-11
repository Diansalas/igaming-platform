package db

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxIdleInTxTimeout is the loosest idle_in_transaction_session_timeout a database URL may set.
const MaxIdleInTxTimeout = time.Hour

// parsePGDuration parses a PostgreSQL time setting: a bare number is milliseconds, otherwise a
// number with one of the units us, ms, s, min, h, d.
func parsePGDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("empty value")
	}
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("not a non-negative integer duration")
	}
	n, err := strconv.ParseInt(v[:i], 10, 64)
	if err != nil {
		return 0, err
	}
	unit := strings.TrimSpace(v[i:])
	var mult time.Duration
	switch unit {
	case "", "ms":
		mult = time.Millisecond
	case "us":
		mult = time.Microsecond
	case "s":
		mult = time.Second
	case "min":
		mult = time.Minute
	case "h":
		mult = time.Hour
	case "d":
		mult = 24 * time.Hour
	default:
		return 0, fmt.Errorf("unknown unit %q", unit)
	}
	if n > int64(24*time.Hour/mult)*365 { // keep the multiplication far from overflow
		return 0, fmt.Errorf("value too large")
	}
	return time.Duration(n) * mult, nil
}

// checkIdleInTxBound refuses a URL-supplied idle_in_transaction_session_timeout that disables the
// timeout (0), is unparsable, or exceeds MaxIdleInTxTimeout.
func checkIdleInTxBound(v string) error {
	d, err := parsePGDuration(v)
	if err != nil {
		return fmt.Errorf("unusable value (%v)", err)
	}
	if d <= 0 {
		return fmt.Errorf("0 would disable the timeout")
	}
	if d > MaxIdleInTxTimeout {
		return fmt.Errorf("exceeds the %s bound", MaxIdleInTxTimeout)
	}
	return nil
}
