import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useSearchParams } from 'react-router'
import { useApi } from '../auth'
import { Empty, ErrorBox, Loading, Rev } from '../components'
import { parseWorkItem, when } from '../format'

/** Cross-repository search: every change linked to a work item or label. */
export function SearchPage() {
  const api = useApi()
  const [params, setParams] = useSearchParams()
  const q = params.get('q') ?? ''
  const label = params.get('label') ?? ''
  const [input, setInput] = useState(label ? `#${label}` : q)
  const item = q ? parseWorkItem(q) : undefined

  const res = useQuery({
    queryKey: ['search', q, label],
    enabled: !!item || !!label,
    queryFn: async () => (item ? (await api.findWorkItem(item)).changes : (await api.findLabel(label)).changes),
  })

  return (
    <section>
      <div className="page-head">
        <h1>Work tracking</h1>
      </div>
      <form
        className="card form-row"
        onSubmit={(e) => {
          e.preventDefault()
          const v = input.trim()
          setParams(v.startsWith('#') ? { label: v.slice(1).toLowerCase() } : { q: v })
        }}
      >
        <input
          aria-label="Work item or label"
          placeholder="PAY-42, servicenow:CHG001, or #risk:high"
          value={input}
          onChange={(e) => setInput(e.target.value)}
        />
        <button className="btn">Search</button>
      </form>
      {q && !item && <p className="error">“{q}” is not a work item. Use KEY-123, system:KEY, or #label.</p>}
      {res.isFetching && <Loading />}
      {res.error && <ErrorBox error={res.error} />}
      {res.data?.length === 0 && <Empty title="No changes found" />}
      {!!res.data?.length && (
        <>
          <p className="muted">
            {res.data.length} change{res.data.length === 1 ? '' : 's'} across {new Set(res.data.map((c) => c.repo)).size} repositor
            {new Set(res.data.map((c) => c.repo)).size === 1 ? 'y' : 'ies'}
          </p>
          <ul className="list">
            {res.data.map((c) => (
              <li key={`${c.repo}/${c.revision}`}>
                <Link to={`/repos/${c.repo}`} className="repo-tag">
                  {c.repo}
                </Link>
                <Link to={`/repos/${c.repo}/changes/${c.revision}`} className="list-title">
                  {c.subject}
                </Link>
                {c.via === 'annotation' && <span className="chip chip-attr">via annotation</span>}
                <span className="muted right">
                  <Rev repo={c.repo} rev={c.revision} /> · {c.author} · {when(c.createdAt)}
                </span>
              </li>
            ))}
          </ul>
        </>
      )}
    </section>
  )
}
