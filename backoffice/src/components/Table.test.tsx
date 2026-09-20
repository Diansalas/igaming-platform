import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { Table, type Column } from './Table'

interface Row {
  id: string
  name: string
}

const columns: Column<Row>[] = [
  { key: 'name', header: 'Name', render: (r) => r.name },
]

describe('Table', () => {
  it('renders rows', () => {
    render(<Table columns={columns} rows={[{ id: '1', name: 'Alice' }, { id: '2', name: 'Bob' }]} getRowKey={(r) => r.id} />)
    expect(screen.getByText('Alice')).toBeInTheDocument()
    expect(screen.getByText('Bob')).toBeInTheDocument()
  })

  it('renders an empty state when there is no data', () => {
    render(<Table columns={columns} rows={[]} getRowKey={(r) => r.id} emptyMessage="Nothing here" />)
    expect(screen.getByText('Nothing here')).toBeInTheDocument()
  })

  it('renders a loading state instead of rows or empty state', () => {
    render(<Table columns={columns} rows={[]} getRowKey={(r) => r.id} isLoading />)
    expect(screen.getByRole('status')).toBeInTheDocument()
  })

  it('calls the page-change callback with the next offset', async () => {
    const onPageChange = vi.fn()
    render(
      <Table
        columns={columns}
        rows={[{ id: '1', name: 'Alice' }]}
        getRowKey={(r) => r.id}
        pagination={{ limit: 1, offset: 0, total: 3, onPageChange }}
      />,
    )
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Next' }))
    expect(onPageChange).toHaveBeenCalledWith(1)
  })

  it('disables Previous on the first page', () => {
    render(
      <Table
        columns={columns}
        rows={[{ id: '1', name: 'Alice' }]}
        getRowKey={(r) => r.id}
        pagination={{ limit: 1, offset: 0, total: 3, onPageChange: vi.fn() }}
      />,
    )
    expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled()
  })
})
