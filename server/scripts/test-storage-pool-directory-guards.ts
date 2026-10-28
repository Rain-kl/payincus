import assert from 'node:assert/strict'
import { isValidPoolDirectory } from '../src/lib/storage/pool-directory.js'

const cases: Record<string, boolean> = {
  '/home/docker_data/incus': true, '/mnt/data': true, '/devices': true,
  '/': false, '/a//b': false, '/a/./b': false, '/a/../b': false, '/mnt/': false,
  '/dev': false, '/dev/shm': false, '/proc/x': false, '/sys': false, rel: false, '/a b': false
}
for (const [dir, expected] of Object.entries(cases)) assert.equal(isValidPoolDirectory(dir), expected, `isValidPoolDirectory(${dir})`)
console.log('storage pool directory guards passed')
