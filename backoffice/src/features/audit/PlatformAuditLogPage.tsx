import { listPlatformAuditLog } from '../../api/audit'
import { AuditLogViewer } from './AuditLogViewer'

export function PlatformAuditLogPage() {
  return (
    <AuditLogViewer
      title="Platform audit log"
      description="Platform-scoped audit entries (tenant provisioning, licence assignment, and other platform-wide events)."
      fetcher={listPlatformAuditLog}
      queryKeyPrefix="platform-audit-log"
    />
  )
}
