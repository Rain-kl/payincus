import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)

const hostInstallScript = readFileSync(resolve(__dirname, '../templates/install.sh'), 'utf8')
const caddyInstallScript = readFileSync(resolve(__dirname, '../templates/caddy.sh'), 'utf8')
const hostRoutes = readFileSync(resolve(__dirname, '../src/routes/hosts.ts'), 'utf8')
const zhCnLocale = readFileSync(resolve(__dirname, '../../client/src/locales/zh-CN.ts'), 'utf8')
const zhTwLocale = readFileSync(resolve(__dirname, '../../client/src/locales/zh-TW.ts'), 'utf8')
const enLocale = readFileSync(resolve(__dirname, '../../client/src/locales/en.ts'), 'utf8')
const installPanel = readFileSync(resolve(__dirname, '../../scripts/install-panel.sh'), 'utf8')
const systemctlWrapper = readFileSync(resolve(__dirname, '../../deploy/incudal-systemctl-wrapper.sh.example'), 'utf8')

assert.ok(
  installPanel.includes('install_verified_root_helpers_from_archive') &&
    installPanel.includes('install -o root -g root -m 0755 "$tmp" /usr/local/libexec/incudal/incudal-online-task') &&
    installPanel.includes('chown "root:${RUN_USER}" "$ENV_FILE"') &&
    installPanel.includes('chmod 640 "$ENV_FILE"'),
  'panel installer must install the fixed OTA entry from the verified archive and keep root unit env service-read-only'
)
assert.ok(
  systemctlWrapper.includes('[[ "$#" -ne 3 || "$1" != "start" || "$2" != "--no-block" ]]') &&
    systemctlWrapper.includes('^incudal-online-(update|rollback)@[1-9][0-9]*\\.service$') &&
    systemctlWrapper.includes('exec /usr/bin/systemctl start --no-block "$UNIT_NAME"'),
  'sudo systemctl wrapper must reject extra arguments and non-OTA unit names'
)

assert.ok(
  caddyInstallScript.includes('auto_https disable_redirects') &&
    caddyInstallScript.indexOf('auto_https disable_redirects') > caddyInstallScript.indexOf('admin localhost:2019') &&
    !caddyInstallScript.includes(':${CADDY_PORT} {') &&
    !caddyInstallScript.includes('basicauth'),
  'Caddy install script must keep admin on loopback only, disable HTTP->HTTPS redirects, and never expose a public listener with credentials'
)

assert.ok(
  hostRoutes.includes('function getPanelCertificatePaths') &&
    hostRoutes.includes("appDir.endsWith('/current') ? dirname(appDir) : null") &&
    hostRoutes.includes("join(installDir, 'server/certs/client.crt')") &&
    hostRoutes.includes("join(installDir, 'server/certs/client.key')") &&
    hostRoutes.includes('existsSync(stableCertPath)') &&
    hostRoutes.includes('existsSync(stableKeyPath)'),
  'host routes must prefer stable install-level panel client certificate paths over release-local paths'
)

assert.ok(
  zhCnLocale.includes('支持 Debian 13、Ubuntu 24.04，通过 Incus 节点包安装并使用 incus-tui 配置。') &&
    zhTwLocale.includes('支援 Debian 13、Ubuntu 24.04，透過 Incus 節點套件安裝並使用 incus-tui 設定。') &&
    enLocale.includes('Supports Debian 13 and Ubuntu 24.04 through the Incus node package and incus-tui.'),
  'host create page copy must explicitly show Debian 12/13 support in all enabled locales'
)

for (const staleCopy of [
  '目前仅支持 Ubuntu 22.04+ 和 Debian 11+ 系统。',
  '目前僅支援 Ubuntu 22.04+ 和 Debian 11+ 系統。',
  'Currently supports Ubuntu 22.04+ and Debian 11+ only.'
]) {
  assert.ok(
    !zhCnLocale.includes(staleCopy) && !zhTwLocale.includes(staleCopy) && !enLocale.includes(staleCopy),
    `host create page must not keep stale Debian support copy: ${staleCopy}`
  )
}

console.log('host install script guard tests passed')

assert.match(hostInstallScript,/Rain-kl\/incus/)
assert.doesNotMatch(hostInstallScript,/apt-get|zpool|incus admin init|systemctl/)
