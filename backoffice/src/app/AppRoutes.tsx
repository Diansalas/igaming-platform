import { Navigate, Route, Routes } from 'react-router-dom'
import { LoginPage } from '../auth/LoginPage'
import { RequireAuth } from '../auth/RequireAuth'
import { RequireNavPermission } from '../auth/RequireNavPermission'
import { AppLayout } from '../layout/AppLayout'
import { BonusLayout } from '../features/bonus/BonusNav'
import { CampaignListPage } from '../features/bonus/CampaignListPage'
import { ChangeRequestQueuePage } from '../features/bonus/ChangeRequestQueuePage'
import { KycQueuePage } from '../features/kyc/KycQueuePage'
import { RgQueuePage } from '../features/rg/RgQueuePage'
import { PlayerDetailPage } from '../features/players/PlayerDetailPage'
import { PlayerListPage } from '../features/players/PlayerListPage'
import { BrandDetailPage } from '../features/tenants/BrandDetailPage'
import { MyTenantPage } from '../features/tenants/MyTenantPage'
import { TenantDetailPage } from '../features/tenants/TenantDetailPage'
import { TenantListPage } from '../features/tenants/TenantListPage'
import { WithdrawalDetailPage } from '../features/withdrawals/WithdrawalDetailPage'
import { WithdrawalQueuePage } from '../features/withdrawals/WithdrawalQueuePage'
import { SportsbookBetsPage } from '../features/sportsbook/SportsbookBetsPage'
import { CasinoRoundsPage } from '../features/casino/CasinoRoundsPage'
import { PlatformAuditLogPage } from '../features/audit/PlatformAuditLogPage'
import { TenantAuditLogPage } from '../features/audit/TenantAuditLogPage'
import { HomePage } from './HomePage'
import { NotFoundPage } from './NotFoundPage'

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />

      <Route
        element={
          <RequireAuth>
            <AppLayout />
          </RequireAuth>
        }
      >
        <Route index element={<HomePage />} />

        <Route
          path="tenants"
          element={
            <RequireNavPermission permission="tenants">
              <TenantListPage />
            </RequireNavPermission>
          }
        />
        <Route
          path="tenants/:tenantId"
          element={
            <RequireNavPermission permission={['tenants', 'ownTenant']}>
              <TenantDetailPage />
            </RequireNavPermission>
          }
        />
        <Route
          path="tenants/:tenantId/brands/:brandId"
          element={
            <RequireNavPermission permission={['tenants', 'ownTenant']}>
              <BrandDetailPage />
            </RequireNavPermission>
          }
        />
        <Route
          path="my-tenant"
          element={
            <RequireNavPermission permission="ownTenant">
              <MyTenantPage />
            </RequireNavPermission>
          }
        />

        <Route
          path="players"
          element={
            <RequireNavPermission permission="players">
              <PlayerListPage />
            </RequireNavPermission>
          }
        />
        <Route
          path="players/:id"
          element={
            <RequireNavPermission permission="players">
              <PlayerDetailPage />
            </RequireNavPermission>
          }
        />

        <Route
          path="kyc"
          element={
            <RequireNavPermission permission="kyc">
              <KycQueuePage />
            </RequireNavPermission>
          }
        />

        <Route
          path="rg"
          element={
            <RequireNavPermission permission="rg">
              <RgQueuePage />
            </RequireNavPermission>
          }
        />

        <Route
          path="bonus"
          element={
            <RequireNavPermission permission="bonus">
              <BonusLayout />
            </RequireNavPermission>
          }
        >
          <Route index element={<Navigate to="campaigns" replace />} />
          <Route path="campaigns" element={<CampaignListPage />} />
          <Route path="change-requests" element={<ChangeRequestQueuePage />} />
        </Route>

        <Route
          path="withdrawals"
          element={
            <RequireNavPermission permission="withdrawals">
              <WithdrawalQueuePage />
            </RequireNavPermission>
          }
        />
        <Route
          path="withdrawals/:id"
          element={
            <RequireNavPermission permission="withdrawals">
              <WithdrawalDetailPage />
            </RequireNavPermission>
          }
        />

        <Route
          path="sportsbook"
          element={
            <RequireNavPermission permission="sportsbook">
              <SportsbookBetsPage />
            </RequireNavPermission>
          }
        />

        <Route
          path="casino"
          element={
            <RequireNavPermission permission="casino">
              <CasinoRoundsPage />
            </RequireNavPermission>
          }
        />

        <Route
          path="audit-log"
          element={
            <RequireNavPermission permission="tenantAudit">
              <TenantAuditLogPage />
            </RequireNavPermission>
          }
        />
        <Route
          path="platform-audit-log"
          element={
            <RequireNavPermission permission="platformAudit">
              <PlatformAuditLogPage />
            </RequireNavPermission>
          }
        />

        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  )
}
