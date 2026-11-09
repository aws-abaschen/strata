import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router'
import type { MergeResult } from '../api'
import { useApi } from '../auth'
import { Empty, ErrorBox, Loading, Rev } from '../components'

/** Merge another branch into the selected ref, on the server. */
export function MergePanel({ repo, target }: { repo: string; target: string }) {
  const api = useApi()
  const qc = useQueryClient()
  const refs = useQuery({ queryKey: ['refs', repo], queryFn: () => api.refs(repo) })
  const sources = refs.data?.filter((r) => r.name !== target) ?? []
  const [picked, setPicked] = useState('')
  const source = sources.some((r) => r.name === picked) ? picked : (sources[0]?.name ?? '')
  const [toReview, setToReview] = useState(false)
  const [message, setMessage] = useState('')
  const preview = useQuery({
    queryKey: ['merge-preview', repo, source, target],
    queryFn: () => api.merge(repo, { source, target, dryRun: true }),
    enabled: !!source,
  })
  // The source revision is part of the name, so a later merge of the same
  // branch gets its own review ref and never replaces an approved result.
  const reviewRef = `review/merge-${source.replace(/[^A-Za-z0-9._/-]/g, '-')}-${(preview.data?.source ?? '').slice(0, 7)}`
  const into = toReview ? reviewRef : target
  const run = useMutation({
    mutationFn: () => api.merge(repo, { source, target, into, message: message.trim() || undefined }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['refs', repo] })
      qc.invalidateQueries({ queryKey: ['log', repo] })
      qc.invalidateQueries({ queryKey: ['merge-preview', repo] })
    },
  })

  if (refs.isPending) return <Loading />
  if (refs.error) return <ErrorBox error={refs.error} />
  if (!sources.length)
    return (
      <Empty title="Nothing to merge">
        <p>Only {target} exists. Push to another ref (for example with strata push --review NAME) to merge it here.</p>
      </Empty>
    )

  const p = preview.data
  const canMerge = p?.status === 'fast-forward' || p?.status === 'merged'
  return (
    <div className="stack">
      <div className="card stack">
        <div className="form-row">
          <label className="field">
            Merge
            <select
              value={source}
              onChange={(e) => {
                setPicked(e.target.value)
                run.reset()
              }}
            >
              {sources.map((r) => (
                <option key={r.name}>{r.name}</option>
              ))}
            </select>
          </label>
          <span className="field">
            into <strong>{target}</strong>
          </span>
        </div>
        {preview.isPending && <Loading />}
        {preview.error && <ErrorBox error={preview.error} />}
        {p && <Preview repo={repo} source={source} target={target} result={p} />}
        {canMerge && (
          <>
            {p.status === 'merged' && (
              <input
                aria-label="Merge message"
                placeholder={`Merge ${source} into ${target}`}
                value={message}
                onChange={(e) => setMessage(e.target.value)}
              />
            )}
            <label>
              <input type="checkbox" checked={toReview} onChange={(e) => setToReview(e.target.checked)} /> Send the result to{' '}
              <span className="mono">{reviewRef}</span> for approval first
            </label>
            <div>
              <button className="btn" disabled={run.isPending} onClick={() => run.mutate()}>
                {p.status === 'fast-forward' ? `Fast-forward ${into}` : `Merge into ${into}`}
              </button>
            </div>
          </>
        )}
        {run.error && (
          <>
            <ErrorBox error={run.error} />
            {!toReview && (
              <p className="muted">
                If {target} needs approvals, send the result to a review ref, have a colleague approve it, then promote it from the change
                page.
              </p>
            )}
          </>
        )}
        {run.data && <Done repo={repo} target={target} result={run.data} />}
      </div>
    </div>
  )
}

function Preview({ repo, source, target, result: r }: { repo: string; source: string; target: string; result: MergeResult }) {
  switch (r.status) {
    case 'up-to-date':
      return (
        <p className="muted">
          {target} already contains {source}.
        </p>
      )
    case 'fast-forward':
      return (
        <p>
          {target} is behind {source}: merging moves it to <Rev repo={repo} rev={r.source} />. No merge change is written, so approvals on
          that revision count.
        </p>
      )
    case 'merged':
      return (
        <p>
          Both sides changed since <Rev repo={repo} rev={r.base ?? ''} />, without conflicts. Merging writes a change with two parents:{' '}
          <Rev repo={repo} rev={r.target} /> ({target}) and <Rev repo={repo} rev={r.source} /> ({source}).
        </p>
      )
    case 'conflicts': {
      const clashes = r.conflicts.filter((c) => !c.limit).length
      const limits = r.conflicts.length - clashes
      return (
        <>
          <p className="error">
            {clashes > 0 && <>{clashes === 1 ? '1 conflict' : `${clashes} conflicts`}: {source} does not merge cleanly into {target}. </>}
            {limits > 0 && <>{limits === 1 ? '1 file is' : `${limits} files are`} past what the server merges. </>}
            Nothing will be written.
          </p>
          <ul className="list">
            {r.conflicts.map((c) => (
              <li key={c.path}>
                <span className="mono">{c.path}</span>
                <span className="muted right">{c.reason}</span>
              </li>
            ))}
          </ul>
          {clashes > 0 && (
            <p className="muted">
              Edit {source} so it no longer conflicts (for example, make the same change {target} made in those places), push, then merge
              again.
            </p>
          )}
          {limits > 0 && (
            <p className="muted">
              Files past the server's limits cannot be merged here yet, whatever their content. Make {source} carry the same content as{' '}
              {target} for those files, or keep the change to them on one branch only.
            </p>
          )}
        </>
      )
    }
  }
}

function Done({ repo, target, result: r }: { repo: string; target: string; result: MergeResult }) {
  const rev = r.change?.revision ?? r.ref?.target ?? r.source
  return (
    <p className="notice">
      {r.into} is now at <Rev repo={repo} rev={rev} />.
      {r.into !== target && (
        <>
          {' '}
          Ask a colleague who wrote none of it to <Link to={`/repos/${repo}/changes/${rev}`}>approve it</Link>, then promote it to{' '}
          {target} from the same page.
        </>
      )}
    </p>
  )
}
