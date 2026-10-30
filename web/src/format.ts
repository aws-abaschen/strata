import type { Metadata, WorkItem } from './api'

export const short = (h: string) => h.slice(0, 12)

export const subject = (message: string) => message.split('\n', 1)[0]

export function when(iso: string, now = new Date()): string {
  const d = new Date(iso)
  const s = Math.round((now.getTime() - d.getTime()) / 1000)
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.floor(s / 60)} min ago`
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`
  if (s < 30 * 86400) return `${Math.floor(s / 86400)} d ago`
  return d.toISOString().slice(0, 10)
}

export function bytes(n?: number): string {
  if (n === undefined) return ''
  if (n < 1024) return `${n} B`
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(1)} MB`
  return `${(n / 1024 ** 3).toFixed(1)} GB`
}

/** Parses "PAY-42" (Jira by default) or "system:KEY", like the CLI. */
export function parseWorkItem(s: string): WorkItem | undefined {
  const t = s.trim()
  if (!t) return undefined
  const i = t.indexOf(':')
  const item = i < 0 ? { system: 'jira', key: t } : { system: t.slice(0, i).toLowerCase(), key: t.slice(i + 1) }
  if (!/^[a-z][a-z0-9-]{0,31}$/.test(item.system) || !/^[A-Za-z0-9][A-Za-z0-9._#-]{0,63}$/.test(item.key)) return undefined
  if (item.system === 'jira') item.key = item.key.toUpperCase()
  return item
}

/** Splits "a, b c" into a clean list. */
export const splitList = (s: string) =>
  s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean)

export const hasMetadata = (m: Metadata) =>
  (m.workItems?.length ?? 0) + (m.labels?.length ?? 0) + Object.keys(m.attributes ?? {}).length > 0
