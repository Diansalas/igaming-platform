package httpserver

import (
	"net/http"
	"strconv"
)

// Stage 5 (Operator Back Office MVP) pagination convention. Every new
// admin list endpoint this stage uses this shared shape so the Back
// Office's table component can be generic across domains rather than
// bespoke per page.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// pageParams is the parsed, bounds-checked ?limit=&offset= query pair.
type pageParams struct {
	Limit  int
	Offset int
}

// parsePageParams reads limit/offset from the request query string,
// clamping to sane bounds rather than rejecting out-of-range input -
// pagination parameters are not security-sensitive, so fail-safe
// (clamp) is preferable to fail-closed (reject) here. Malformed
// (non-numeric) values fall back to the default/zero rather than
// erroring, for the same reason.
func parsePageParams(r *http.Request) pageParams {
	p := pageParams{Limit: defaultPageLimit, Offset: 0}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.Limit = n
		}
	}
	if p.Limit > maxPageLimit {
		p.Limit = maxPageLimit
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			p.Offset = n
		}
	}
	return p
}

// pagedResponse is the shared envelope for every Stage 5 admin list
// endpoint: {"items": [...], "limit": N, "offset": N, "total": N}.
// `Items` is always a non-nil, possibly-empty slice (never `null` over
// the wire).
type pagedResponse[T any] struct {
	Items  []T `json:"items"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

func newPagedResponse[T any](items []T, p pageParams, total int) pagedResponse[T] {
	if items == nil {
		items = []T{}
	}
	return pagedResponse[T]{Items: items, Limit: p.Limit, Offset: p.Offset, Total: total}
}
