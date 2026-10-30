import { useEffect, useRef, useState } from 'react'
import { BrowserRouter, Link, NavLink, Route, Routes, useNavigate } from 'react-router'
import { useAuth } from './auth'
import { CopyButton, ErrorBox, Loading } from './components'
import { ChangePage } from './pages/Change'
import { FilesPage } from './pages/Files'
import { RepoPage } from './pages/Repo'
import { Repos } from './pages/Repos'
import { SearchPage } from './pages/Search'

export function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/callback" element={<Callback />} />
        <Route path="*" element={<Shell />} />
      </Routes>
    </BrowserRouter>
  )
}

function Shell() {
  const { user, signIn, signOut } = useAuth()
  if (user === undefined) return <Loading />
  if (!user) {
    return (
      <main className="signin">
        <Logo />
        <h1>Strata</h1>
        <p className="muted">Serverless version control for code, docs, configuration and AI skills.</p>
        <button className="btn" onClick={() => signIn()}>
          Sign in
        </button>
      </main>
    )
  }
  return (
    <>
      <header className="top">
        <Link to="/" className="brand">
          <Logo /> Strata
        </Link>
        <nav>
          <NavLink to="/" end>
            Repositories
          </NavLink>
          <NavLink to="/search">Work tracking</NavLink>
        </nav>
        <div className="account">
          <span className="muted">{(user.profile.email as string | undefined) ?? user.profile.sub}</span>
          <CopyButton label="CLI token" text={user.id_token ?? ''} />
          <button className="btn btn-quiet" onClick={() => signOut()}>
            Sign out
          </button>
        </div>
      </header>
      <main className="page">
        <Routes>
          <Route path="/" element={<Repos />} />
          <Route path="/search" element={<SearchPage />} />
          <Route path="/repos/:repo" element={<RepoPage />} />
          <Route path="/repos/:repo/changes/:rev" element={<ChangePage />} />
          <Route path="/repos/:repo/changes/:rev/files/*" element={<FilesPage />} />
          <Route path="*" element={<p>Page not found.</p>} />
        </Routes>
      </main>
    </>
  )
}

function Callback() {
  const { completeSignIn } = useAuth()
  const nav = useNavigate()
  const [error, setError] = useState<unknown>()
  const started = useRef(false)
  useEffect(() => {
    if (started.current) return // the code can be redeemed only once (StrictMode runs effects twice)
    started.current = true
    completeSignIn()
      .then((to) => nav(to, { replace: true }))
      .catch(setError)
  }, [completeSignIn, nav])
  return <main className="signin">{error ? <ErrorBox error={error} /> : <Loading />}</main>
}

function Logo() {
  return (
    <svg className="logo" viewBox="0 0 32 32" aria-hidden>
      <rect width="32" height="32" rx="7" fill="var(--accent)" />
      <path d="M7 11h18M7 16h14M7 21h10" stroke="#fff" strokeWidth="3" strokeLinecap="round" />
    </svg>
  )
}
