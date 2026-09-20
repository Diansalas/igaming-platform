import { Button } from './Button'

export interface PaginationProps {
  limit: number
  offset: number
  total: number
  onPageChange: (nextOffset: number) => void
}

/**
 * Generic pager built around the shared {items,limit,offset,total}
 * envelope every Stage 5 admin list endpoint returns
 * (internal/httpserver/pagination.go) - used identically by every
 * feature's list page.
 */
export function Pagination({ limit, offset, total, onPageChange }: PaginationProps) {
  if (total === 0) return null

  const from = offset + 1
  const to = Math.min(offset + limit, total)
  const hasPrevious = offset > 0
  const hasNext = offset + limit < total

  return (
    <div className="flex items-center justify-between border-t border-border px-4 py-3 text-sm text-slate-600">
      <span>
        Showing {from}-{to} of {total}
      </span>
      <div className="flex gap-2">
        <Button
          variant="secondary"
          disabled={!hasPrevious}
          onClick={() => onPageChange(Math.max(0, offset - limit))}
        >
          Previous
        </Button>
        <Button variant="secondary" disabled={!hasNext} onClick={() => onPageChange(offset + limit)}>
          Next
        </Button>
      </div>
    </div>
  )
}
