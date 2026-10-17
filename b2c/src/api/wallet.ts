import type { MinorAmount } from '../lib/amount'
import { apiFetch } from './client'

// Mirrors internal/httpserver/wallet_handlers.go's walletSummaryResponse
// exactly (field names/JSON tags) - amounts are integer minor units,
// matching every other financial API in this codebase. This app NEVER
// computes a balance client-side; it only renders exactly what the
// server returns.
export interface WalletSummary {
  wallet_id: string
  asset_code: string
  status: string
  cash_balance: MinorAmount
  available_balance: MinorAmount
  held_for_withdrawal: MinorAmount
  locked_balance: MinorAmount
  bonus_balance: MinorAmount
}

export function listMyWallets(): Promise<WalletSummary[]> {
  return apiFetch<WalletSummary[]>('/v1/me/wallets')
}

export function getMyWallet(assetCode: string): Promise<WalletSummary> {
  return apiFetch<WalletSummary>(`/v1/me/wallets/${encodeURIComponent(assetCode)}`)
}
