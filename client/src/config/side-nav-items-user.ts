export interface MenuItem {
  name?: string
  path?: string
  icon?: string
  label?: string
  labelText?: string
  divider?: boolean
}

export const isAdminEntry = false

export const navMenuItems: MenuItem[] = [
  { divider: true, label: 'nav.main' },
  { name: 'dashboard', path: '/dashboard', icon: 'home', label: 'nav.userCenter' },
  { name: 'instances', path: '/instances', icon: 'server', label: 'nav.instanceManage' },
  { name: 'instance-create', path: '/instances/create', icon: 'feather', label: 'nav.createInstance' },
  { name: 'terminal', path: '/terminal', icon: 'terminal', label: 'nav.instanceTerminal' },
  { name: 'mail', path: '/mail', icon: 'mail', label: 'nav.domainMail' },
  { divider: true, label: 'nav.billing' },
  { name: 'wallet', path: '/wallet', icon: 'wallet', label: 'nav.myWallet' },
  { name: 'orders', path: '/orders', icon: 'card', label: 'nav.myOrders' },
  { name: 'gift-cards', path: '/gift-cards', icon: 'gift', label: 'nav.giftCardsTicket' },
  { name: 'invites', path: '/invites', icon: 'key', label: 'nav.inviteFriends' },
  { name: 'friends', path: '/friends', icon: 'users', label: 'nav.myFriends' },
  { name: 'transfers', path: '/transfers', icon: 'transfer', label: 'nav.transferInstance' },
  { divider: true, label: 'nav.support' },
  { name: 'tickets', path: '/tickets', icon: 'ticket', label: 'nav.ticketSupport' },
  { name: 'help', path: '/help', icon: 'help', label: 'nav.helpCenter' },
  { name: 'logs', path: '/logs', icon: 'logs', label: 'nav.operationLogs' },
  { divider: true, label: 'nav.system' },
  { name: 'extensions', path: '/extensions', icon: 'puzzle', label: 'nav.extensionCenter' },
  { name: 'entertainment', path: '/entertainment', icon: 'gift', label: 'nav.welfareCenter' },
  { name: 'profile', path: '/profile', icon: 'settings', label: 'nav.personalSettings' },
]
