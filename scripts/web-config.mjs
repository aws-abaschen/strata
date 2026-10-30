// Writes web/public/config.json from the deployed stack's outputs, for `pnpm dev`.
import { execFileSync } from 'node:child_process'
import { writeFileSync } from 'node:fs'

const stack = process.argv[2] ?? 'Strata'
const raw = execFileSync('aws', ['cloudformation', 'describe-stacks', '--stack-name', stack, '--query', 'Stacks[0].Outputs', '--output', 'json'], {
  encoding: 'utf8',
})
const out = Object.fromEntries(JSON.parse(raw).map((o) => [o.OutputKey, o.OutputValue]))
const config = { apiUrl: out.ApiUrl, authority: out.Authority, clientId: out.ClientId, authDomain: out.AuthDomain }
writeFileSync(new URL('../web/public/config.json', import.meta.url), JSON.stringify(config, null, 2) + '\n')
console.log('wrote web/public/config.json')
