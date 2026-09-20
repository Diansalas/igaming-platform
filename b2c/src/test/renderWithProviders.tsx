import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import type { ReactElement } from 'react'
import { MemoryRouter } from 'react-router-dom'
import { AuthProvider } from '../auth/AuthContext'
import { BetSlipProvider } from '../features/betslip/BetSlipContext'

export function renderWithProviders(ui: ReactElement, { route = '/' }: { route?: string } = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <BetSlipProvider>
          <MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>
        </BetSlipProvider>
      </AuthProvider>
    </QueryClientProvider>,
  )
}
