function shellEscape(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`
}

/** The Incus package owns both incusd and its panel connector. */
export function buildNodeInstallCommand(input: {
  panelUrl: string
  installToken: string
  channel?: 'latest' | 'canary'
}): string {
  const panelUrl = input.panelUrl.replace(/\/+$/, '')
  return `curl -fsSL ${shellEscape(`${panelUrl}/api/agent/install.sh`)} -o incus-node-install.sh && sudo env INCUDAL_PANEL_URL=${shellEscape(panelUrl)} INCUDAL_AGENT_INSTALL_TOKEN=${shellEscape(input.installToken)} INCUS_NODE_CHANNEL=${shellEscape(input.channel || 'canary')} bash incus-node-install.sh`
}
