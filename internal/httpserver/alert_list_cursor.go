package httpserver

// ADR 0114 S1b: the opaque keyset cursor of GET /v1/admin/alerts.
//
// The list orders by (first_seen_at DESC, id DESC) and a cursor names the last row served,
// so a page boundary is stable under concurrent inserts (a new alert has a newer
// first_seen_at and sorts before the cursor, never inside the remaining pages) and under
// ties (id breaks them). The cursor is NOT a security token: it carries a position in data
// the caller may read anyway, so tampering can only move the caller to another position of
// the same result set. It is validated strictly (shape, UUID, time range, size) and bound to
// the filter it was issued for, so a cursor replayed against a different state/severity/kind
// is refused instead of silently paging a different set.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	alertCursorVersion = 1
	alertCursorMaxLen  = 400
	// Bounds that keep the value inside what PostgreSQL timestamptz accepts.
	alertCursorMinMicros = int64(946684800) * 1_000_000  // 2000-01-01
	alertCursorMaxMicros = int64(7258118400) * 1_000_000 // 2200-01-01
)

type alertCursor struct {
	Micros int64
	ID     uuid.UUID
}

type alertCursorWire struct {
	V int    `json:"v"`
	T int64  `json:"t"`
	I string `json:"i"`
	F string `json:"f"`
}

var errAlertCursor = errors.New("invalid alert cursor")

func alertCursorFilter(state, severity, kind string) string {
	return state + "|" + severity + "|" + kind
}

func encodeAlertCursor(firstSeen time.Time, id, filter string) string {
	raw, _ := json.Marshal(alertCursorWire{V: alertCursorVersion, T: firstSeen.UnixMicro(), I: id, F: filter})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeAlertCursor(s, filter string) (alertCursor, error) {
	if s == "" || len(s) > alertCursorMaxLen {
		return alertCursor{}, errAlertCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return alertCursor{}, errAlertCursor
	}
	var w alertCursorWire
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil || dec.More() {
		return alertCursor{}, errAlertCursor
	}
	if w.V != alertCursorVersion || w.F != filter || w.T < alertCursorMinMicros || w.T > alertCursorMaxMicros {
		return alertCursor{}, errAlertCursor
	}
	id, err := uuid.Parse(w.I)
	if err != nil || id.String() != w.I {
		return alertCursor{}, errAlertCursor
	}
	return alertCursor{Micros: w.T, ID: id}, nil
}
