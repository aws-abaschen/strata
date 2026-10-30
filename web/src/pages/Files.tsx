import { useQuery } from '@tanstack/react-query'
import { Link, useParams } from 'react-router'
import { useApi } from '../auth'
import { ErrorBox, Loading, Rev } from '../components'
import { bytes } from '../format'

const MAX_PREVIEW = 512 * 1024

export function FilesPage() {
  const { repo = '', rev = '', '*': splat = '' } = useParams()
  const path = splat.replace(/\/$/, '')
  const api = useApi()
  const q = useQuery({ queryKey: ['tree', repo, rev, path], queryFn: () => api.tree(repo, rev, path) })
  const base = `/repos/${repo}/changes/${rev}/files`
  const parts = path ? path.split('/') : []

  return (
    <section>
      <div className="page-head">
        <h1>
          <Link to={`/repos/${repo}`}>{repo}</Link> / <Link to={base}>files</Link>
          {parts.map((p, i) => (
            <span key={i}>
              {' / '}
              <Link to={`${base}/${parts.slice(0, i + 1).join('/')}`}>{p}</Link>
            </span>
          ))}
        </h1>
        <span className="muted">
          at <Rev repo={repo} rev={rev} />
        </span>
      </div>
      {q.isPending && <Loading />}
      {q.error && <ErrorBox error={q.error} />}
      {q.data && 'entries' in q.data && (
        <ul className="list files">
          {[...q.data.entries]
            .sort((a, b) => Number(b.kind === 'dir') - Number(a.kind === 'dir') || a.name.localeCompare(b.name))
            .map((e) => (
              <li key={e.name}>
                <span className="file-icon" aria-hidden>
                  {e.kind === 'dir' ? '▸' : '·'}
                </span>
                <Link to={`${base}/${path ? `${path}/` : ''}${e.name}`}>{e.name}</Link>
                <span className="muted right">{e.kind === 'dir' ? '' : bytes(e.size)}</span>
              </li>
            ))}
        </ul>
      )}
      {q.data && 'file' in q.data && <FileView url={q.data.url} size={q.data.file.size ?? 0} name={q.data.file.name} />}
    </section>
  )
}

function FileView({ url, size, name }: { url: string; size: number; name: string }) {
  const text = useQuery({
    queryKey: ['blob', url],
    enabled: size <= MAX_PREVIEW,
    staleTime: Infinity,
    queryFn: async () => {
      const buf = new Uint8Array(await (await fetch(url)).arrayBuffer())
      if (buf.includes(0)) return null // binary
      return new TextDecoder().decode(buf)
    },
  })
  return (
    <div className="card">
      <div className="file-head">
        <span className="mono">{name}</span>
        <span className="muted">{bytes(size)}</span>
        <a className="btn btn-quiet" href={url} download={name}>
          Download
        </a>
      </div>
      {size > MAX_PREVIEW && <p className="muted">Too large to preview.</p>}
      {text.isPending && size <= MAX_PREVIEW && <Loading />}
      {text.data === null && <p className="muted">Binary file.</p>}
      {typeof text.data === 'string' && <pre className="code file-body">{text.data}</pre>}
      {text.error && <ErrorBox error={text.error} />}
    </div>
  )
}
