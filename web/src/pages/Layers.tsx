import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Fragment, useState } from 'react'
import { Link } from 'react-router'
import type { LayerMode, Repo } from '../api'
import { useApi } from '../auth'
import { Empty, ErrorBox, Loading } from '../components'
import { bytes, short } from '../format'

const modeHelp: Record<LayerMode, string> = {
  locked: 'Lower levels cannot change it',
  override: 'Lower levels may replace it',
  append: 'Lower levels may add to it',
}

export function ModeChip({ mode }: { mode: LayerMode }) {
  return (
    <span className={`chip chip-mode-${mode}`} title={modeHelp[mode]}>
      {mode}
    </span>
  )
}

/** The resolved configuration of a repository, and what it extends. */
export function LayersPanel({ repo, ref }: { repo: Repo; ref: string }) {
  const api = useApi()
  const eff = useQuery({ queryKey: ['layers', repo.name, ref], queryFn: () => api.layers(repo.name, ref) })
  const [open, setOpen] = useState<string>()

  return (
    <>
      <ExtendsForm repo={repo} />
      {eff.isPending && <Loading />}
      {eff.error && <ErrorBox error={eff.error} />}
      {eff.data && eff.data.chain.length > 0 && (
        <>
          <p className="chain" aria-label="Layer chain">
            {eff.data.chain.map((l) => (
              <Fragment key={l.layer}>
                <Link to={`/repos/${l.layer}?tab=fleet`} className="chain-link">
                  {l.layer}
                  <span className="muted">@{l.ref}</span>
                  {l.missing ? <span className="muted"> (empty)</span> : <span className="mono muted"> {short(l.revision ?? '')}</span>}
                </Link>
                <span className="chain-arrow" aria-hidden>
                  →
                </span>
              </Fragment>
            ))}
            <span className="chain-link chain-self">{repo.name}</span>
          </p>
          {eff.data.violations.length > 0 && (
            <div className="notice">
              {eff.data.violations.map((v) => (
                <p key={v.path + v.source}>
                  <span className="mono">{v.path}</span> from {v.source} is ignored: locked by {v.lockedBy}.
                </p>
              ))}
            </div>
          )}
          {eff.data.files.length === 0 ? (
            <Empty title="The layers add no files yet" />
          ) : (
            <table className="table">
              <thead>
                <tr>
                  <th>File</th>
                  <th>Mode</th>
                  <th>Comes from (top first)</th>
                  <th>Size</th>
                </tr>
              </thead>
              <tbody>
                {eff.data.files.map((f) => (
                  <Fragment key={f.path}>
                    <tr>
                      <td>
                        <button type="button" className="link mono" onClick={() => setOpen(open === f.path ? undefined : f.path)} aria-expanded={open === f.path}>
                          {f.path}
                        </button>
                      </td>
                      <td>
                        <ModeChip mode={f.mode} /> <span className="muted small">by {f.modeSetBy}</span>
                      </td>
                      <td>
                        {f.parts.map((p, i) => (
                          <span key={i} className={p.source === repo.name ? 'source source-self' : 'source'}>
                            {p.source}
                          </span>
                        ))}
                      </td>
                      <td className="muted">{bytes(f.parts.reduce((n, p) => n + p.size, 0))}</td>
                    </tr>
                    {open === f.path && (
                      <tr>
                        <td colSpan={4}>
                          {f.content !== undefined ? <pre className="code file-body">{f.content}</pre> : <p className="muted">Binary or too large to show.</p>}
                        </td>
                      </tr>
                    )}
                  </Fragment>
                ))}
              </tbody>
            </table>
          )}
        </>
      )}
    </>
  )
}

function ExtendsForm({ repo }: { repo: Repo }) {
  const api = useApi()
  const qc = useQueryClient()
  const repos = useQuery({ queryKey: ['repos'], queryFn: api.repos })
  const [layer, setLayer] = useState(repo.extends?.layer ?? '')
  const [ref, setRef] = useState(repo.extends?.ref ?? 'main')
  const save = useMutation({
    mutationFn: () => api.setExtends(repo.name, layer ? { layer, ref: ref || 'main' } : null),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['repo', repo.name] })
      qc.invalidateQueries({ queryKey: ['layers', repo.name] })
      qc.invalidateQueries({ queryKey: ['repos'] })
    },
  })
  const candidates = (repos.data ?? []).filter((r) => r.kind === 'layer' && r.name !== repo.name)
  const changed = layer !== (repo.extends?.layer ?? '') || (layer !== '' && ref !== (repo.extends?.ref ?? 'main'))

  return (
    <form
      className="card form-row"
      onSubmit={(e) => {
        e.preventDefault()
        save.mutate()
      }}
    >
      <label className="field">
        <span className="muted">Extends</span>
        <select value={layer} onChange={(e) => setLayer(e.target.value)} disabled={repos.isPending}>
          <option value="">No layer</option>
          {candidates.map((r) => (
            <option key={r.name} value={r.name}>
              {r.name}
            </option>
          ))}
        </select>
      </label>
      <label className="field">
        <span className="muted">At ref</span>
        <input value={ref} onChange={(e) => setRef(e.target.value)} disabled={!layer} placeholder="main" />
      </label>
      <button className="btn" disabled={!changed || save.isPending}>
        Save
      </button>
      {save.error && <ErrorBox error={save.error} />}
      {save.data?.violations.map((v) => (
        <p key={v.path} className="error">
          {v.path} is locked by {v.lockedBy}: remove it from {repo.name} or pushes will be rejected.
        </p>
      ))}
    </form>
  )
}

/** Every repository that inherits from a Layer, directly or not. */
export function FleetPanel({ layer }: { layer: string }) {
  const api = useApi()
  const subs = useQuery({ queryKey: ['subscribers', layer], queryFn: () => api.subscribers(layer) })
  if (subs.isPending) return <Loading />
  if (subs.error) return <ErrorBox error={subs.error} />
  if (subs.data.length === 0) return <Empty title="No repository extends this layer yet" />
  return (
    <>
      <p className="muted">
        {subs.data.length} repositor{subs.data.length === 1 ? 'y inherits' : 'ies inherit'} from {layer}.
      </p>
      <ul className="list">
        {subs.data.map((s) => (
          <li key={s.repo.name} style={{ paddingLeft: `${s.depth}rem` }}>
            <Link to={`/repos/${s.repo.name}?tab=layers`} className="list-title">
              {s.repo.name}
            </Link>
            <span className={`chip ${s.repo.kind === 'layer' ? 'chip-label' : 'chip-attr'}`}>{s.repo.kind}</span>
            <span className="muted right">
              via {s.via}@{s.repo.extends?.ref}
            </span>
          </li>
        ))}
      </ul>
    </>
  )
}
