import { listTenantAuditLog } from '../../api/audit'
import { AuditLogViewer } from './AuditLogViewer'

export function TenantAuditLogPage() {
  return (
    <AuditLogViewer
      title="Audit log"
      description="This tenant's own audit trail."
      fetcher={listTenantAuditLog}
      queryKeyPrefix="tenant-audit-log"
    />
  )
}
