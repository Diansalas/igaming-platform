export function EmptyState({ message = 'Nothing to show yet.' }: { message?: string }) {
  return (
    <div className="flex flex-col items-center gap-1 rounded-lg border border-dashed border-border p-10 text-center text-slate-500">
      <p className="text-sm">{message}</p>
    </div>
  )
}
