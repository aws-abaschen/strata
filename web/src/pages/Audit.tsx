import { useInfiniteQuery } from '@tanstack/react-query'
import type { AuditRecord } from '../api'
import { useApi } from '../auth'
import { Empty, ErrorBox, Loading } from '../components'
import { short } from '../format'

function summary(r: AuditRecord): string {
  const d = r.detail as { subject?: string; note?: string; ref?: { name?: string; target?: string } | string; settings?: { extends?: { layer: string; ref: string } } }
  if (d.note) return d.note
  if (d.subject) return d.subject
  if (d.ref && typeof d.ref === 'object') return `${d.ref.name} → ${short(d.ref.target ?? '') || 'deleted'}`
  if (r.action === 'RepoSettingsChanged') return d.settings?.extends ? `extends ${d.settings.extends.layer}@${d.settings.extends.ref}` : 'extends no layer'
  return ''
}

/** A repository's hash-chained ledger, oldest first. */
export function AuditPanel({ repo }: { repo: string }) {
  const api = useApi()
  const q = useInfiniteQuery({
    queryKey: ['audit', repo],
    initialPageParam: 0,
    queryFn: ({ pageParam }) => api.auditPage(repo, pageParam),
    getNextPageParam: (p) => p.next,
  })
  if (q.isPending) return <Loading />
  if (q.error) return <ErrorBox error={q.error} />
  const records = q.data.pages.flatMap((p) => p.records)
  const broken = q.data.pages.find((p) => !p.verified)
  if (records.length === 0) return <Empty title="No audit records yet" />
  return (
    <>
      {broken ? (
        <p className="error">Chain verification failed: {broken.error}</p>
      ) : (
        <p className="muted">
          {records.length} records, chain checked by the server. For an independent check, run <code>strata audit --verify</code>.
        </p>
      )}
      <table className="table">
        <thead>
          <tr>
            <th>#</th>
            <th>When</th>
            <th>Who</th>
            <th>Action</th>
            <th>What</th>
            <th>Hash</th>
          </tr>
        </thead>
        <tbody>
          {records.map((r) => (
            <tr key={r.seq}>
              <td className="muted">{r.seq}</td>
              <td className="small">{new Date(r.at).toLocaleString()}</td>
              <td>{r.actor}</td>
              <td>
                <span className="chip chip-attr">{r.action}</span>
              </td>
              <td>{summary(r)}</td>
              <td className="mono muted" title={`hash ${r.hash}\nprev ${r.prev || '(start of chain)'}`}>
                {short(r.hash)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {q.hasNextPage && (
        <button className="btn btn-quiet" onClick={() => q.fetchNextPage()} disabled={q.isFetchingNextPage}>
          Load more
        </button>
      )}
    </>
  )
}
