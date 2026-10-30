import { useState, type ReactNode } from 'react'
import { Link } from 'react-router'
import type { Metadata } from './api'
import { short } from './format'

export function Chips({ metadata }: { metadata: Metadata }) {
  const attrs = Object.entries(metadata.attributes ?? {})
  if (!metadata.workItems?.length && !metadata.labels?.length && !attrs.length) return null
  return (
    <span className="chips">
      {metadata.workItems?.map((w) => (
        <Link key={`${w.system}:${w.key}`} className="chip chip-work" to={`/search?q=${encodeURIComponent(`${w.system}:${w.key}`)}`}>
          {w.system === 'jira' ? w.key : `${w.system}:${w.key}`}
        </Link>
      ))}
      {metadata.labels?.map((l) => (
        <Link key={l} className="chip chip-label" to={`/search?label=${encodeURIComponent(l)}`}>
          #{l}
        </Link>
      ))}
      {attrs.map(([k, v]) => (
        <span key={k} className="chip chip-attr">
          {k}={v}
        </span>
      ))}
    </span>
  )
}

export function Rev({ repo, rev }: { repo: string; rev: string }) {
  return (
    <Link className="mono" to={`/repos/${repo}/changes/${rev}`} title={rev}>
      {short(rev)}
    </Link>
  )
}

export function CopyButton({ text, label = 'Copy' }: { text: string | (() => Promise<string>); label?: string }) {
  const [done, setDone] = useState(false)
  return (
    <button
      type="button"
      className="btn btn-quiet"
      onClick={async () => {
        await navigator.clipboard.writeText(typeof text === 'string' ? text : await text())
        setDone(true)
        setTimeout(() => setDone(false), 1500)
      }}
    >
      {done ? 'Copied' : label}
    </button>
  )
}

export function Loading() {
  return <p className="muted">Loading…</p>
}

export function ErrorBox({ error }: { error: unknown }) {
  return <p className="error">{error instanceof Error ? error.message : String(error)}</p>
}

export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      <h3>{title}</h3>
      {children}
    </div>
  )
}
