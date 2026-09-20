export function NotAuthorized() {
  return (
    <div className="flex flex-col items-center gap-2 rounded-lg border border-amber-200 bg-amber-50 p-10 text-center">
      <p className="text-sm font-medium text-amber-800">
        Your role does not have access to this section.
      </p>
      <p className="text-xs text-amber-700">
        If you believe this is a mistake, contact a tenant or platform administrator.
      </p>
    </div>
  )
}
