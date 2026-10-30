import { describe, expect, it } from 'vitest'
import { bytes, hasMetadata, parseWorkItem, splitList, subject, when } from './format'

describe('format', () => {
  it('parses work items like the CLI', () => {
    expect(parseWorkItem('pay-42')).toEqual({ system: 'jira', key: 'PAY-42' })
    expect(parseWorkItem('ServiceNow:CHG0031337')).toEqual({ system: 'servicenow', key: 'CHG0031337' })
    expect(parseWorkItem('jira:bad key')).toBeUndefined()
    expect(parseWorkItem('  ')).toBeUndefined()
  })

  it('formats relative times and sizes', () => {
    const now = new Date('2026-10-30T12:00:00Z')
    expect(when('2026-10-30T11:59:30Z', now)).toBe('just now')
    expect(when('2026-10-30T09:00:00Z', now)).toBe('3 h ago')
    expect(when('2025-01-02T00:00:00Z', now)).toBe('2025-01-02')
    expect(bytes(2048)).toBe('2.0 KB')
  })

  it('handles messages and lists', () => {
    expect(subject('PAY-42 fix\n\nbody')).toBe('PAY-42 fix')
    expect(splitList('risk:low, release/1  team-a')).toEqual(['risk:low', 'release/1', 'team-a'])
    expect(hasMetadata({})).toBe(false)
    expect(hasMetadata({ labels: ['x'] })).toBe(true)
  })
})
