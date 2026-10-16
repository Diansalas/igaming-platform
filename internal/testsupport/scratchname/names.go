// Package scratchname holds the pure (database-free, build-tag-free) naming and selection logic for scratch
// databases, so the name pattern and the reaper's selection rules are unit
// tested in the default `go test ./...` run.
package scratchname

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// nameTimeLayout is the UTC creation-timestamp layout embedded in every
// scratch database name.
const nameTimeLayout = "20060102150405"

// maxIdentLen is PostgreSQL's identifier limit (NAMEDATALEN-1).
const maxIdentLen = 63

var (
	prefixRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// nameRe is the EXACT scratch-database name pattern:
	//
	//	<prefix><yyyymmddhhmmss UTC>p<pid>x<8 hex>
	//
	// The reaper refuses every database whose name does not match it.
	nameRe = regexp.MustCompile(`^([a-z][a-z0-9_]*?)(\d{14})p(\d{1,10})x([0-9a-f]{8})$`)
)

// ScratchName is the parsed form of a scratch database name.
type ScratchName struct {
	Prefix  string
	Created time.Time // UTC
	PID     int
	Rand    string
}

// BuildName returns a scratch database name for prefix, created at now by
// process pid, with an 8-hex random suffix (rand8 must be 8 lowercase hex
// characters).
func BuildName(prefix string, now time.Time, pid int, rand8 string) (string, error) {
	if !prefixRe.MatchString(prefix) {
		return "", fmt.Errorf("scratchname: prefix %q must match %s", prefix, prefixRe)
	}
	if pid < 0 {
		return "", fmt.Errorf("scratchname: negative pid %d", pid)
	}
	name := prefix + now.UTC().Format(nameTimeLayout) + "p" + strconv.Itoa(pid) + "x" + rand8
	if len(name) > maxIdentLen {
		return "", fmt.Errorf("scratchname: name %q is %d bytes, over the %d-byte identifier limit", name, len(name), maxIdentLen)
	}
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("scratchname: generated name %q does not match the scratch pattern (bad random suffix?)", name)
	}
	return name, nil
}

// ParseName parses a scratch database name; ok is false for anything that
// does not match the exact pattern (including the legacy prefix+16hex form).
func ParseName(name string) (ScratchName, bool) {
	m := nameRe.FindStringSubmatch(name)
	if m == nil {
		return ScratchName{}, false
	}
	ts, err := time.ParseInLocation(nameTimeLayout, m[2], time.UTC)
	if err != nil {
		return ScratchName{}, false
	}
	pid, err := strconv.Atoi(m[3])
	if err != nil {
		return ScratchName{}, false
	}
	return ScratchName{Prefix: m[1], Created: ts, PID: pid, Rand: m[4]}, true
}

// Candidate is one database as seen by the reaper.
type Candidate struct {
	Name       string
	Owner      string
	IsTemplate bool
	Sessions   int // sessions at listing time
}

// SelectReapable returns the names of candidates that may be dropped: the
// name matches the exact scratch pattern, was created at least maxAge before
// now (a creation time in the future is never reapable), is owned by owner,
// is not a template and has no sessions. maxAge must be positive and owner
// non-empty; otherwise nothing is selected. Everything else is refused.
func SelectReapable(now time.Time, maxAge time.Duration, owner string, cands []Candidate) []string {
	if maxAge <= 0 || owner == "" {
		return nil
	}
	var out []string
	for _, c := range cands {
		if c.IsTemplate || c.Sessions != 0 || c.Owner != owner {
			continue
		}
		p, ok := ParseName(c.Name)
		if !ok {
			continue
		}
		if now.Sub(p.Created) < maxAge {
			continue
		}
		out = append(out, c.Name)
	}
	return out
}
