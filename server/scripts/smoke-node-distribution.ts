import '../src/config/env.js'
import assert from 'node:assert/strict'

type JsonObject = Record<string, unknown>

interface FetchResult {
  response: Response
  data: JsonObject
  text: string
}

const apiBaseUrl = trimSlash(
  process.env.SMOKE_API_BASE_URL ||
    process.env.SMOKE_FRONTEND_URL ||
    process.env.FRONTEND_URL ||
    process.env.BACKEND_URL ||
    'http://127.0.0.1:3001'
)

function trimSlash(value: string): string {
  return value.replace(/\/+$/, '')
}

async function fetchText(path: string): Promise<FetchResult> {
  const response = await fetch(`${apiBaseUrl}${path}`, {
    headers: {
      Accept: '*/*',
      'User-Agent': 'incus-node-distribution-smoke/1.0'
    }
  })
  const text = await response.text()
  let data: JsonObject = {}
  try {
    data = text ? JSON.parse(text) as JsonObject : {}
  } catch {
    data = {}
  }
  return { response, data, text }
}

function assertJsonError(result: FetchResult, status: number, code: string, label: string): void {
  assert.equal(result.response.status, status, `${label} expected ${status}, got ${result.response.status}: ${result.text}`)
  assert.equal(result.data.code, code, `${label} must return ${code}`)
}

async function main(): Promise<void> {
  const health = await fetchText('/api/health')
  assert.equal(health.response.status, 200, `backend health expected 200, got ${health.response.status}: ${health.text}`)

  const installScript = await fetchText('/api/agent/install.sh')
  assert.equal(installScript.response.status, 200, `agent install script expected 200, got ${installScript.response.status}: ${installScript.text}`)
  assert.match(
    installScript.response.headers.get('content-type') || '',
    /text\/x-shellscript/i,
    'agent install script must be served as shellscript text'
  )
  assert.match(installScript.text, /Rain-kl\/incus/)
  assert.doesNotMatch(installScript.text, /incudal-agent-linux|agent-release|systemctl|ExecStart/)
  const distribution = await fetchText('/api/agent/distribution')
  assert.equal(distribution.response.status, 200)
  assert.equal(distribution.data.repository, 'Rain-kl/incus')
  assert.equal(distribution.data.managedBy, 'apt')
  assert.equal((await fetchText('/api/agent/manifest.json')).response.status, 404)
  assert.equal((await fetchText('/api/agent/binary/incudal-agent-linux-amd64')).response.status, 404)
  console.log('[smoke-node-distribution] passed')

}

main()
  .then(() => {
    process.exit(0)
  })
  .catch(error => {
    console.error(error instanceof Error ? error.message : error)
    process.exit(1)
  })
