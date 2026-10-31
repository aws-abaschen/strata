// Typed client for the Strata API. Types mirror backend/internal/core.

export type Hash = string

export interface WorkItem {
  system: string
  key: string
  url?: string
}

export interface Metadata {
  workItems?: WorkItem[]
  labels?: string[]
  attributes?: Record<string, string>
}

export interface LayerRef {
  layer: string
  ref: string
}

export interface Repo {
  name: string
  description?: string
  kind: 'code' | 'layer'
  extends?: LayerRef
  createdBy: string
  createdAt: string
}

export type LayerMode = 'locked' | 'override' | 'append'

export interface LayerPart {
  source: string
  revision?: Hash
  hash: Hash
  size: number
}

export interface LayerFile {
  path: string
  mode: LayerMode
  modeSetBy: string
  kind: 'file' | 'exec'
  parts: LayerPart[]
  content?: string
}

export interface Violation {
  path: string
  source: string
  lockedBy: string
}

export interface Effective {
  repo: string
  chain: { layer: string; ref: string; revision?: Hash; missing?: boolean }[]
  files: LayerFile[]
  violations: Violation[]
}

export interface Subscriber {
  repo: Repo
  via: string
  depth: number
}

export interface Ref {
  name: string
  target: Hash
  updatedBy: string
  updatedAt: string
}

export interface Change {
  revision: Hash
  changeId: string
  repo: string
  tree: Hash
  parents: Hash[]
  predecessor?: Hash
  author: string
  createdAt: string
  message: string
  metadata: Metadata
}

export interface Annotation {
  repo: string
  revision: Hash
  author: string
  createdAt: string
  note?: string
  metadata: Metadata
}

export interface ChangeDetail {
  change: Change
  annotations: Annotation[]
  effectiveMetadata: Metadata
  latestRevision: Hash
}

export interface TreeEntry {
  name: string
  kind: 'file' | 'exec' | 'dir'
  hash: Hash
  size?: number
}

export type TreeResult =
  | { path: string; tree: Hash; entries: TreeEntry[] }
  | { path: string; file: TreeEntry; url: string }

export interface ChangeSummary {
  repo: string
  revision: Hash
  changeId: string
  subject: string
  author: string
  createdAt: string
  via: 'change' | 'annotation'
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message)
  }
}

export type TokenSource = () => Promise<string | undefined>

export class Api {
  constructor(
    private readonly base: string,
    private readonly token: TokenSource,
  ) {}

  private async call<T>(method: string, path: string, body?: unknown): Promise<T> {
    const token = await this.token()
    const res = await fetch(this.base.replace(/\/$/, '') + path, {
      method,
      headers: {
        ...(token ? { authorization: `Bearer ${token}` } : {}),
        ...(body === undefined ? {} : { 'content-type': 'application/json' }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (!res.ok) {
      const text = await res.text()
      let msg = text
      try {
        msg = (JSON.parse(text) as { error?: string; message?: string }).error ?? msg
      } catch {
        // not JSON
      }
      throw new ApiError(res.status, msg || res.statusText)
    }
    return (await res.json()) as T
  }

  me = () => this.call<{ identity: string }>('GET', '/me')
  repos = () => this.call<{ repos: Repo[] }>('GET', '/repos').then((r) => r.repos)
  createRepo = (name: string, description: string, kind: Repo['kind'] = 'code', ext?: LayerRef) =>
    this.call<Repo>('POST', '/repos', { name, description, kind, extends: ext })
  setExtends = (repo: string, ext: LayerRef | null) =>
    this.call<{ repo: Repo; violations: Violation[] }>('PUT', `/repos/${enc(repo)}/settings`, { extends: ext })
  layers = (repo: string, ref: string) =>
    this.call<Effective>('GET', `/repos/${enc(repo)}/layers?ref=${encodeURIComponent(ref)}&content=true`)
  auditPage = (repo: string, after = 0) => this.call<AuditPage>('GET', `/repos/${enc(repo)}/audit?after=${after}&limit=200`)
  subscribers = (layer: string) =>
    this.call<{ subscribers: Subscriber[] }>('GET', `/repos/${enc(layer)}/subscribers`).then((r) => r.subscribers)
  repo = (repo: string) => this.call<Repo>('GET', `/repos/${enc(repo)}`)
  refs = (repo: string) => this.call<{ refs: Ref[] }>('GET', `/repos/${enc(repo)}/refs`).then((r) => r.refs)
  log = (repo: string, ref: string, limit = 50) =>
    this.call<{ changes: Change[] }>('GET', `/repos/${enc(repo)}/log?ref=${encodeURIComponent(ref)}&limit=${limit}`).then(
      (r) => r.changes,
    )
  change = (repo: string, rev: Hash) => this.call<ChangeDetail>('GET', `/repos/${enc(repo)}/changes/${rev}`)
  tree = (repo: string, rev: Hash, path: string) =>
    this.call<TreeResult>('GET', `/repos/${enc(repo)}/tree/${rev}?path=${encodeURIComponent(path)}`)
  annotate = (repo: string, rev: Hash, note: string, metadata: Metadata) =>
    this.call<Annotation>('POST', `/repos/${enc(repo)}/changes/${rev}/annotations`, { note, metadata })
  findWorkItem = (w: WorkItem) =>
    this.call<{ workItem: WorkItem; changes: ChangeSummary[] }>('GET', `/work-items/${enc(w.system)}/${enc(w.key)}`)
  findLabel = (label: string) =>
    this.call<{ label: string; changes: ChangeSummary[] }>('GET', `/labels/${label.split('/').map(enc).join('/')}`)
}

const enc = encodeURIComponent

export interface AuditRecord {
  repo: string
  seq: number
  prev: Hash
  hash: Hash
  at: string
  actor: string
  action: string
  detail: Record<string, unknown>
}

export interface AuditPage {
  records: AuditRecord[]
  verified: boolean
  error?: string
  next?: number
}

