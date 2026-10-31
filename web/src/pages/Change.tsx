import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useParams } from 'react-router'
import type { WorkItem } from '../api'
import { useApi } from '../auth'
import { Chips, CopyButton, ErrorBox, Loading, Rev } from '../components'
import { parseWorkItem, splitList, subject, when } from '../format'

export function ChangePage() {
  const { repo = '', rev = '' } = useParams()
  const api = useApi()
  const q = useQuery({ queryKey: ['change', repo, rev], queryFn: () => api.change(repo, rev) })

  if (q.isPending) return <Loading />
  if (q.error) return <ErrorBox error={q.error} />
  const { change: c, annotations, approvals = [], effectiveMetadata, latestRevision } = q.data
  const body = c.message.slice(subject(c.message).length).trim()

  return (
    <section>
      <div className="page-head">
        <h1>
          <Link to={`/repos/${repo}`}>{repo}</Link> / {subject(c.message)}
        </h1>
        <Link className="btn" to={`/repos/${repo}/changes/${rev}/files`}>
          Browse files
        </Link>
      </div>
      {latestRevision !== c.revision && (
        <p className="notice">
          This revision was amended. Latest: <Rev repo={repo} rev={latestRevision} />
        </p>
      )}
      <div className="card">
        <dl className="facts">
          <dt>Revision</dt>
          <dd className="mono">
            {c.revision} <CopyButton text={c.revision} />
          </dd>
          <dt>Change ID</dt>
          <dd className="mono">{c.changeId}</dd>
          <dt>Author</dt>
          <dd>
            {c.author} · {new Date(c.createdAt).toLocaleString()}
          </dd>
          <dt>Parents</dt>
          <dd>{c.parents.length ? c.parents.map((p) => <Rev key={p} repo={repo} rev={p} />) : <span className="muted">none (root)</span>}</dd>
          {c.predecessor && (
            <>
              <dt>Amends</dt>
              <dd>
                <Rev repo={repo} rev={c.predecessor} />
              </dd>
            </>
          )}
          <dt>Tracking</dt>
          <dd>
            <Chips metadata={effectiveMetadata} />
          </dd>
        </dl>
        {body && <pre className="message">{body}</pre>}
      </div>

      <h2>Approvals</h2>
      <p className="muted">Sign-offs bind to this exact revision. You cannot approve a change you wrote.</p>
      {approvals.length === 0 && <p className="muted">None yet.</p>}
      <ul className="list">
        {approvals.map((a) => (
          <li key={a.approver}>
            <span>{a.note || <span className="muted">approved</span>}</span>
            <span className="muted right">
              {a.approver} · {when(a.createdAt)}
            </span>
          </li>
        ))}
      </ul>
      <ApproveButton repo={repo} rev={rev} />

      <h2>Annotations</h2>
      <p className="muted">Add tickets, labels or notes after the fact. The revision itself never changes.</p>
      {annotations.length === 0 && <p className="muted">None yet.</p>}
      <ul className="list">
        {annotations.map((a, i) => (
          <li key={i}>
            <span>{a.note || <span className="muted">metadata only</span>}</span>
            <Chips metadata={a.metadata} />
            <span className="muted right">
              {a.author} · {when(a.createdAt)}
            </span>
          </li>
        ))}
      </ul>
      <AnnotateForm repo={repo} rev={rev} />
    </section>
  )
}

function ApproveButton({ repo, rev }: { repo: string; rev: string }) {
  const api = useApi()
  const qc = useQueryClient()
  const [note, setNote] = useState('')
  const m = useMutation({
    mutationFn: () => api.approve(repo, rev, note.trim()),
    onSuccess: () => {
      setNote('')
      qc.invalidateQueries({ queryKey: ['change', repo, rev] })
    },
  })
  return (
    <div className="card stack">
      <input aria-label="Approval note" placeholder="Why you approve (optional)" value={note} onChange={(e) => setNote(e.target.value)} />
      <div>
        <button className="btn" disabled={m.isPending} onClick={() => m.mutate()}>
          Approve this revision
        </button>
      </div>
      {m.error && <ErrorBox error={m.error} />}
    </div>
  )
}

function AnnotateForm({ repo, rev }: { repo: string; rev: string }) {
  const api = useApi()
  const qc = useQueryClient()
  const [items, setItems] = useState('')
  const [labels, setLabels] = useState('')
  const [note, setNote] = useState('')
  const [formError, setFormError] = useState('')
  const m = useMutation({
    mutationFn: (workItems: WorkItem[]) => api.annotate(repo, rev, note.trim(), { workItems, labels: splitList(labels) }),
    onSuccess: () => {
      setItems('')
      setLabels('')
      setNote('')
      qc.invalidateQueries({ queryKey: ['change', repo, rev] })
    },
  })
  return (
    <form
      className="card stack"
      onSubmit={(e) => {
        e.preventDefault()
        const raw = splitList(items)
        const parsed = raw.map(parseWorkItem)
        const bad = raw.filter((_, i) => !parsed[i])
        if (bad.length) return setFormError(`Not a work item: ${bad.join(', ')}`)
        setFormError('')
        m.mutate(parsed as WorkItem[])
      }}
    >
      <div className="form-row">
        <input aria-label="Work items" placeholder="Work items: PAY-42, servicenow:CHG001" value={items} onChange={(e) => setItems(e.target.value)} />
        <input aria-label="Labels" placeholder="Labels: risk:low, release/2026.11" value={labels} onChange={(e) => setLabels(e.target.value)} />
      </div>
      <textarea aria-label="Note" placeholder="Note (optional)" rows={2} value={note} onChange={(e) => setNote(e.target.value)} />
      <div>
        <button className="btn" disabled={m.isPending}>
          Add annotation
        </button>
      </div>
      {formError && <p className="error">{formError}</p>}
      {m.error && <ErrorBox error={m.error} />}
    </form>
  )
}
