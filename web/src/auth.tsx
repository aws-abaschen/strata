import { User, UserManager, WebStorageStateStore } from 'oidc-client-ts'
import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { Api } from './api'

/** Runtime config, written to /config.json by the CDK deployment. */
export interface Config {
  apiUrl: string
  authority: string
  clientId: string
  authDomain: string
  /** Local development only (backend/cmd/devserver): sign in as this user, no OIDC. */
  devUser?: string
}

export async function loadConfig(): Promise<Config> {
  const res = await fetch('/config.json', { cache: 'no-store' })
  if (!res.ok) throw new Error('config.json not found')
  return (await res.json()) as Config
}

interface AuthState {
  config: Config
  user: User | null | undefined // undefined while loading
  api: Api
  signIn: () => Promise<void>
  signOut: () => Promise<void>
  completeSignIn: () => Promise<string>
  /** Managed login page where the signed-in user registers a passkey. */
  addPasskeyUrl: string
}

const AuthContext = createContext<AuthState | null>(null)

export function AuthProvider({ config, children }: { config: Config; children: ReactNode }) {
  const origin = window.location.origin
  const manager = useMemo(
    () =>
      new UserManager({
        authority: config.authority,
        client_id: config.clientId,
        redirect_uri: `${origin}/callback`,
        post_logout_redirect_uri: `${origin}/`,
        response_type: 'code',
        scope: 'openid email profile',
        automaticSilentRenew: true,
        userStore: new WebStorageStateStore({ store: window.localStorage }),
      }),
    [config, origin],
  )
  const [user, setUser] = useState<User | null | undefined>(undefined)

  useEffect(() => {
    if (config.devUser) {
      setUser(new User({ id_token: config.devUser, access_token: '', token_type: 'Bearer', profile: { sub: config.devUser, email: config.devUser, iss: 'dev', aud: 'dev', exp: 0, iat: 0 } }))
      return
    }
    manager.getUser().then((u) => setUser(u && !u.expired ? u : null))
    const loaded = (u: User) => setUser(u)
    const unloaded = () => setUser(null)
    manager.events.addUserLoaded(loaded)
    manager.events.addUserUnloaded(unloaded)
    manager.events.addAccessTokenExpired(unloaded)
    return () => {
      manager.events.removeUserLoaded(loaded)
      manager.events.removeUserUnloaded(unloaded)
      manager.events.removeAccessTokenExpired(unloaded)
    }
  }, [manager, config.devUser])

  const value = useMemo<AuthState>(
    () => ({
      config,
      user,
      // The API's JWT authorizer expects the ID token (its audience is the client id).
      api: new Api(config.apiUrl, async () => config.devUser ?? (await manager.getUser())?.id_token),
      signIn: () => manager.signinRedirect({ state: window.location.pathname + window.location.search }),
      signOut: async () => {
        if (config.devUser) return
        await manager.removeUser()
        const q = new URLSearchParams({ client_id: config.clientId, logout_uri: `${origin}/` })
        window.location.href = `${config.authDomain}/logout?${q}`
      },
      addPasskeyUrl: `${config.authDomain}/passkeys/add?${new URLSearchParams({ client_id: config.clientId, redirect_uri: `${origin}/` })}`,
      completeSignIn: async () => {
        const u = await manager.signinRedirectCallback()
        return typeof u.state === 'string' && u.state.startsWith('/') ? u.state : '/'
      },
    }),
    [config, user, manager, origin],
  )
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthState {
  const v = useContext(AuthContext)
  if (!v) throw new Error('useAuth outside AuthProvider')
  return v
}

export const useApi = () => useAuth().api
