import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { AuthProvider, loadConfig } from './auth'
import './styles.css'

const root = createRoot(document.getElementById('root')!)
const queries = new QueryClient({ defaultOptions: { queries: { staleTime: 30_000, retry: 1 } } })

loadConfig()
  .then((config) =>
    root.render(
      <StrictMode>
        <QueryClientProvider client={queries}>
          <AuthProvider config={config}>
            <App />
          </AuthProvider>
        </QueryClientProvider>
      </StrictMode>,
    ),
  )
  .catch(() =>
    root.render(
      <main className="signin">
        <h1>Strata is not configured</h1>
        <p className="muted">
          Copy <code>public/config.example.json</code> to <code>public/config.json</code> and fill it from the stack outputs
          (<code>just outputs</code>).
        </p>
      </main>,
    ),
  )
