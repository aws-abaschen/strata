import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useNavigate } from 'react-router'
import { useApi } from '../auth'
import { Empty, ErrorBox, Loading } from '../components'
import { when } from '../format'

export function Repos() {
  const api = useApi()
  const qc = useQueryClient()
  const nav = useNavigate()
  const repos = useQuery({ queryKey: ['repos'], queryFn: api.repos })
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const create = useMutation({
    mutationFn: () => api.createRepo(name.trim(), description.trim()),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ['repos'] })
      nav(`/repos/${r.name}`)
    },
  })

  return (
    <section>
      <div className="page-head">
        <h1>Repositories</h1>
      </div>
      <form
        className="card form-row"
        onSubmit={(e) => {
          e.preventDefault()
          create.mutate()
        }}
      >
        <input
          aria-label="Repository name"
          placeholder="new-repository"
          value={name}
          onChange={(e) => setName(e.target.value.toLowerCase())}
          pattern="[a-z0-9][a-z0-9-]{1,62}"
          required
        />
        <input aria-label="Description" placeholder="Description (optional)" value={description} onChange={(e) => setDescription(e.target.value)} />
        <button className="btn" disabled={create.isPending}>
          Create
        </button>
      </form>
      {create.error && <ErrorBox error={create.error} />}

      {repos.isPending && <Loading />}
      {repos.error && <ErrorBox error={repos.error} />}
      {repos.data?.length === 0 && <Empty title="No repositories yet">Create one above, then push to it with the strata CLI.</Empty>}
      {!!repos.data?.length && (
        <ul className="list">
          {repos.data.map((r) => (
            <li key={r.name}>
              <Link to={`/repos/${r.name}`} className="list-title">
                {r.name}
              </Link>
              <span className="muted">{r.description}</span>
              <span className="muted right">
                created by {r.createdBy} · {when(r.createdAt)}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
