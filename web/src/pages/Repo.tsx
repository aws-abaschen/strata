import { useQuery } from '@tanstack/react-query'
import { Link, useParams, useSearchParams } from 'react-router'
import { useApi, useAuth } from '../auth'
import { Chips, CopyButton, Empty, ErrorBox, Loading, Rev } from '../components'
import { subject, when } from '../format'

export function RepoPage() {
  const { repo = '' } = useParams()
  const api = useApi()
  const { config } = useAuth()
  const [params, setParams] = useSearchParams()
  const info = useQuery({ queryKey: ['repo', repo], queryFn: () => api.repo(repo) })
  const refs = useQuery({ queryKey: ['refs', repo], queryFn: () => api.refs(repo) })
  const ref = params.get('ref') ?? refs.data?.find((r) => r.name === 'main')?.name ?? refs.data?.[0]?.name ?? 'main'
  const log = useQuery({ queryKey: ['log', repo, ref], queryFn: () => api.log(repo, ref), enabled: refs.isSuccess })
  const head = refs.data?.find((r) => r.name === ref)

  return (
    <section>
      <div className="page-head">
        <h1>
          <Link to="/">Repositories</Link> / {repo}
        </h1>
        {!!refs.data?.length && (
          <label className="ref-select">
            Ref{' '}
            <select value={ref} onChange={(e) => setParams({ ref: e.target.value })}>
              {refs.data.map((r) => (
                <option key={r.name}>{r.name}</option>
              ))}
            </select>
          </label>
        )}
      </div>
      {info.data?.description && <p className="muted">{info.data.description}</p>}
      {info.error && <ErrorBox error={info.error} />}
      {head && (
        <p className="muted">
          <Link to={`/repos/${repo}/changes/${head.target}/files`}>Browse files</Link> at <Rev repo={repo} rev={head.target} /> · moved by{' '}
          {head.updatedBy} {when(head.updatedAt)}
        </p>
      )}

      {(refs.isPending || log.isPending) && <Loading />}
      {log.error && <ErrorBox error={log.error} />}
      {refs.isSuccess && refs.data.length === 0 && (
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
    </section>
  )
}
