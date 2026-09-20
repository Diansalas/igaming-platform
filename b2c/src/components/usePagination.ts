import { useState } from 'react'

/** Generic offset/limit pagination state, shared by every list page - same shape as backoffice's usePagination. */
export function usePagination(limit = 20) {
  const [offset, setOffset] = useState(0)
  return {
    limit,
    offset,
    setOffset,
    resetToFirstPage: () => setOffset(0),
  }
}
