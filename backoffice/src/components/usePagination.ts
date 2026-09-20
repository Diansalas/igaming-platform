import { useState } from 'react'

/** Generic offset/limit pagination state, shared by every list page. */
export function usePagination(limit = 20) {
  const [offset, setOffset] = useState(0)
  return {
    limit,
    offset,
    setOffset,
    /** Call when a filter changes, so a new filter always starts back at page one. */
    resetToFirstPage: () => setOffset(0),
  }
}
