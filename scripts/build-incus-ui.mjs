#!/usr/bin/env node

import { execSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import {
  cpSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync
} from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)
const repoRoot = resolve(__dirname, '..')
const incusUiRoot = resolve(repoRoot, 'incus-ui')
const incusUiDist = resolve(incusUiRoot, 'dist')
const hashFile = resolve(incusUiDist, '.build-hash')
const targetAdminIncusUiDir = resolve(repoRoot, 'client/dist/admin/incus-ui')

function updateHashFromPath(hash, targetPath) {
  if (!existsSync(targetPath)) return
  const st = statSync(targetPath)
  if (st.isDirectory()) {
    const entries = readdirSync(targetPath).sort()
    for (const entry of entries) {
      if (entry === 'node_modules' || entry === 'dist' || entry.startsWith('.')) continue
      updateHashFromPath(hash, join(targetPath, entry))
    }
  } else if (st.isFile()) {
    hash.update(targetPath.slice(incusUiRoot.length))
    hash.update(readFileSync(targetPath))
  }
}

function computeSourceHash() {
  const hash = createHash('sha256')
  updateHashFromPath(hash, resolve(incusUiRoot, 'src'))
  updateHashFromPath(hash, resolve(incusUiRoot, 'public'))
  updateHashFromPath(hash, resolve(incusUiRoot, 'index.html'))
  updateHashFromPath(hash, resolve(incusUiRoot, 'package.json'))
  updateHashFromPath(hash, resolve(incusUiRoot, 'vite.config.ts'))
  updateHashFromPath(hash, resolve(incusUiRoot, 'tsconfig.json'))
  return hash.digest('hex')
}

const currentHash = computeSourceHash()
const cachedHash = existsSync(hashFile) ? readFileSync(hashFile, 'utf8').trim() : ''
const indexHtmlExists = existsSync(resolve(incusUiDist, 'index.html'))

if (currentHash !== cachedHash || !indexHtmlExists) {
  console.log('[incus-ui] Building @payincus/incus-ui from source...')
  execSync('pnpm --filter @payincus/incus-ui build', {
    cwd: repoRoot,
    stdio: 'inherit'
  })
  writeFileSync(hashFile, currentHash, 'utf8')
} else {
  console.log('[incus-ui] Source unchanged, reusing cached build output.')
}

if (existsSync(resolve(repoRoot, 'client/dist/admin'))) {
  rmSync(targetAdminIncusUiDir, { recursive: true, force: true })
  mkdirSync(targetAdminIncusUiDir, { recursive: true })
  cpSync(incusUiDist, targetAdminIncusUiDir, {
    recursive: true,
    filter: (src) => !src.endsWith('.build-hash')
  })
  console.log('[incus-ui] Synced build output to client/dist/admin/incus-ui')
}
