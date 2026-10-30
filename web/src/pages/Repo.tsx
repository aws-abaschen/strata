import { useQuery } from '@tanstack/react-query'
import { Link, useParams, useSearchParams } from 'react-router'
import { useApi, useAuth } from '../auth'
import { Chips, CopyButton, Empty, ErrorBox, Loading, Rev } from '../components'
import { subject, when } from '../format'
import { FleetPanel, LayersPanel } from './Layers'

type Tab = 'history' | 'layers' | 'fleet'

export function RepoPage() {
  const { repo = '' } = useParams()
  const api = useApi()
  const [params, setParams] = useSearchParams()
  const info = useQuery({ queryKey: ['repo', repo], queryFn: () => api.repo(repo) })
  const refs = useQuery({ queryKey: ['refs', repo], queryFn: () => api.refs(repo) })
  const ref = params.get('ref') ?? refs.data?.find((r) => r.name === 'main')?.name ?? refs.data?.[0]?.name ?? 'main'
  const isLayer = info.data?.kind === 'layer'
  const tab = (params.get('tab') as Tab | null) ?? 'history'
  const tabs: [Tab, string][] = [
    ['history', 'History'],
    ['layers', 'Layers'],
    ...(isLayer ? ([['fleet', 'Fleet']] as [Tab, string][]) : []),
  ]
  const go = (t: Tab) => setParams((p) => (p.set('tab', t), p))

  return (
    <section>
      <div className="page-head">
        <h1>
          <Link to="/">Repositories</Link> / {repo} {isLayer && <span className="chip chip-label">layer</span>}
        </h1>
        {!!refs.data?.length && (
          <label className="ref-select">
            Ref{' '}
            <select value={ref} onChange={(e) => setParams((p) => (p.set('ref', e.target.value), p))}>
              {refs.data.map((r) => (
                <option key={r.name}>{r.name}</option>
              ))}
            </select>
          </label>
        )}
      </div>
      {info.data?.description && <p className="muted">{info.data.description}</p>}
      {info.data?.extends && (
        <p className="muted">
          Extends{' '}
          <Link to={`/repos/${info.data.extends.layer}`}>
            {info.data.extends.layer}@{info.data.extends.ref}
          </Link>
        </p>
      )}
      {info.error && <ErrorBox error={info.error} />}

      <nav className="tabs" role="tablist">
        {tabs.map(([t, label]) => (
          <button key={t} role="tab" aria-selected={tab === t} className={tab === t ? 'tab active' : 'tab'} onClick={() => go(t)}>
            {label}
          </button>
        ))}
      </nav>

      {tab === 'history' && <History repo={repo} ref_={ref} refsReady={refs.isSuccess} hasRefs={!!refs.data?.length} />}
      {tab === 'layers' && info.data && <LayersPanel repo={info.data} ref={ref} />}
      {tab === 'fleet' && isLayer && <FleetPanel layer={repo} />}
    </section>
  )
}

function History({ repo, ref_, refsReady, hasRefs }: { repo: string; ref_: string; refsReady: boolean; hasRefs: boolean }) {
  const api = useApi()
  const { config } = useAuth()
  const refs = useQuery({ queryKey: ['refs', repo], queryFn: () => api.refs(repo) })
  const log = useQuery({ queryKey: ['log', repo, ref_], queryFn: () => api.log(repo, ref_), enabled: refsReady })
  const head = refs.data?.find((r) => r.name === ref_)

  return (
    <>
      {head && (
        <p className="muted">
          <Link to={`/repos/${repo}/changes/${head.target}/files`}>Browse files</Link> at <Rev repo={repo} rev={head.target} /> · moved by{' '}
          {head.updatedBy} {when(head.updatedAt)}
        </p>
      )}
      {(!refsReady || log.isPending) && <Loading />}
      {log.error && <ErrorBox error={log.error} />}
      {refsReady && !hasRefs && (
        <Empty title="Nothing pushed yet">
          <p>Push your first change with the CLI:</p>
          <pre className="code">
            {`export STRATA_API=${config.apiUrl}
export STRATA_TOKEN=<token from the account menu>
strata init --repo ${repo}
strata push -m "PAY-1 first change" --label team:payments`}
          </pre>
          <CopyButton label="Copy commands" text={`strata init --repo ${repo}\nstrata push -m "PAY-1 first change"`} />
        </Empty>
      )}
      {!!log.data?.length && (
        <ul className="list">
          {log.data.map((c) => (
            <li key={c.revision}>
              <Link to={`/repos/${repo}/changes/${c.revision}`} className="list-title">
                {subject(c.message)}
              </Link>
              <Chips metadata={c.metadata} />
              <span className="muted right">
                <Rev repo={repo} rev={c.revision} /> · {c.author} · {when(c.createdAt)}
              </span>
            </li>
          ))}
        </ul>
      )}
    </>
  )
}
