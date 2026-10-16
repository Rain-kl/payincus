import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import type { LookupAddress } from 'node:dns'
import { revalidatingLookup, OutboundTargetValidationError } from '../src/lib/outbound-security.js'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)

function readSource(relativePath: string): string {
  return readFileSync(resolve(__dirname, relativePath), 'utf8')
}

const outboundSecuritySource = readSource('../src/lib/outbound-security.ts')
const serverPackageSource = readSource('../package.json')
const rootPackageSource = readSource('../../package.json')
const safeFetchBody = outboundSecuritySource.match(
  /export async function safeFetch\([\s\S]*?\n}\n\nexport async function assertSafeStorageTarget/
)?.[0] ?? ''

assert.ok(safeFetchBody, 'outbound security must export safeFetch')
assert.ok(
  safeFetchBody.includes('await assertSafeHttpUrl(url, label)'),
  'safeFetch must validate the URL before making the request'
)
assert.ok(
  safeFetchBody.includes('dispatcher: safeOutboundDispatcher'),
  'safeFetch must use the connection-time revalidating dispatcher'
)
assert.ok(
  serverPackageSource.includes('"test:safe-outbound-fetch-guards"'),
  'server package must expose the safe outbound fetch guard script'
)
assert.ok(
  rootPackageSource.includes('pnpm --filter server test:safe-outbound-fetch-guards'),
  'root test chain must run the safe outbound fetch guard'
)

for (const [label, relativePath] of [
  ['Epay order query', '../src/lib/epay.ts'],
  ['Heleket API client', '../src/lib/heleket.ts'],
  ['notification webhooks', '../src/lib/notifier.ts'],
  ['Lsky client', '../src/lib/lsky.ts'],
  ['ticket image proxy', '../src/routes/tickets.ts'],
  ['CraneMail client', '../src/services/cranemail.ts'],
  ['SmarterMail client', '../src/services/smartermail.ts'],
  ['system monitor webhook', '../src/services/system-monitor.ts']
] as const) {
  assert.ok(readSource(relativePath).includes('safeFetch('), `${label} must use safeFetch`)
}

function lookup(hostname: string, all: boolean): Promise<{ address: string | LookupAddress[]; family?: number }> {
  return new Promise((resolve, reject) => revalidatingLookup(hostname, { all }, (error, address, family) => {
    if (error) reject(error)
    else resolve({ address, family })
  }))
}
// Literal addresses exercise the real Node DNS callback without external DNS.
assert.deepEqual(await lookup('8.8.8.8', true), { address: [{ address: '8.8.8.8', family: 4 }], family: undefined })
assert.deepEqual(await lookup('8.8.8.8', false), { address: '8.8.8.8', family: 4 })
assert.deepEqual(await lookup('2606:4700:4700::1111', true), { address: [{ address: '2606:4700:4700::1111', family: 6 }], family: undefined })
for (const all of [true, false]) {
  for (const address of ['127.0.0.1', '169.254.169.254', '::1', 'fc00::1']) {
    await assert.rejects(lookup(address, all), OutboundTargetValidationError)
  }
}
console.log('safe outbound fetch guard tests passed')
