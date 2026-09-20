import type { ReactNode } from 'react'
import { EmptyState } from './EmptyState'
import { ErrorState } from './ErrorState'
import { LoadingSpinner } from './LoadingSpinner'
import { Pagination, type PaginationProps } from './Pagination'

export interface Column<T> {
  key: string
  header: string
  render: (row: T) => ReactNode
  className?: string
}

export interface TableProps<T> {
  columns: Column<T>[]
  rows: T[]
  getRowKey: (row: T) => string
  isLoading?: boolean
  error?: unknown
  onRetry?: () => void
  emptyMessage?: string
  onRowClick?: (row: T) => void
  pagination?: PaginationProps
}

/**
 * The one generic, paginated/searchable/filterable-capable list-table
 * component the whole app reuses (Blueprint's "build it ONCE" data grid
 * requirement) - it knows nothing about tenants/players/withdrawals/etc.
 * Filtering/search inputs live in each feature's page, next to the query
 * that feeds `rows`; this component only renders whatever rows it is
 * given plus the shared pagination footer.
 */
export function Table<T>({
  columns,
  rows,
  getRowKey,
  isLoading,
  error,
  onRetry,
  emptyMessage,
  onRowClick,
  pagination,
}: TableProps<T>) {
  return (
    <div className="overflow-hidden rounded-lg border border-border bg-surface shadow-sm">
      {isLoading ? (
        <div className="flex justify-center p-10">
          <LoadingSpinner />
        </div>
      ) : error ? (
        <div className="p-6">
          <ErrorState error={error} onRetry={onRetry} />
        </div>
      ) : rows.length === 0 ? (
        <div className="p-6">
          <EmptyState message={emptyMessage} />
        </div>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <thead className="bg-surface-alt text-xs uppercase tracking-wide text-slate-500">
              <tr>
                {columns.map((col) => (
                  <th key={col.key} className={`px-4 py-3 font-medium ${col.className ?? ''}`}>
                    {col.header}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {rows.map((row) => (
                <tr
                  key={getRowKey(row)}
                  onClick={onRowClick ? () => onRowClick(row) : undefined}
                  className={onRowClick ? 'cursor-pointer hover:bg-surface-alt' : undefined}
                >
                  {columns.map((col) => (
                    <td key={col.key} className={`px-4 py-3 text-slate-700 ${col.className ?? ''}`}>
                      {col.render(row)}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {!isLoading && !error && pagination && <Pagination {...pagination} />}
    </div>
  )
}
