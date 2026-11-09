<script setup lang="ts">
import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import { useAuthStore } from '@/stores/auth'
import { useToast } from '@/stores/toast'
import { onClickOutside } from '@vueuse/core'
import { availableFlagCountryCodes, getLocalizedCountryName } from '@/utils/countryDisplay'
import { buildHostApiUrl, validateIdentifier, validateName, validateHostAddress, validateIpAddress } from '@/utils/validation'
import FlagIcon from '@/components/FlagIcon.vue'
import { Badge, Button, Card, Input, PageHeader, Select } from '@/components/ui'
import {
  Server,
  Network,
  Radio,
  Globe,
  Cpu,
  Info,
  Check,
  Copy,
  AlertCircle,
  Search,
  ChevronDown,
  X
} from 'lucide-vue-next'

const props = withDefaults(
  defineProps<{
    show?: boolean
  }>(),
  {
    show: false
  }
)

const emit = defineEmits<{
  (e: 'update:show', value: boolean): void
  (e: 'close'): void
  (e: 'created', host?: any): void
}>()

const { t, locale } = useI18n()
const toast = useToast()
const authStore = useAuthStore()

const isAdmin = computed(() => authStore.user?.role === 'admin')

// 节点名称前缀，仅普通用户需要
const hostNamePrefix = computed(() => `PEER${authStore.user?.id}-`)

const saving = ref(false)
const showInstallScript = ref(false)
const installCommand = ref('')

const hostId = ref<number>(0)
const installStatus = ref<'waiting' | 'verifying' | 'success' | 'error'>('waiting')
const verifyError = ref('')

const form = ref({
  name: '',
  nameSuffix: '',
  tunnelEnabled: true,
  hostAddress: '',
  apiPort: 8443,
  targetHost: '127.0.0.1',
  targetPort: 8443,
  location: '',
  countryCode: 'us',
  cpuAllowanceMax: 0,
  memoryMax: 0,
  instanceType: 'container' as 'container' | 'vm' | 'both',
  networkMode: 'nat' as 'nat' | 'nat_ipv6' | 'ipv6_only',
  ipv6Subnet: '',
  ipv6ParentInterface: '',
  natPublicIp: '',
  natPublicIpv6: '',
  natBindIp: '',
  natBindIpv6: '',
  portRangeStart: 10000 as number | null,
  portRangeEnd: 65000 as number | null
})

const showCountryDropdown = ref(false)
const countrySearch = ref('')
const countryDropdownRef = ref<HTMLElement | null>(null)
onClickOutside(countryDropdownRef, () => closeCountryDropdown())

const countries = computed(() => {
  return availableFlagCountryCodes
    .map(code => ({
      code,
      name: getLocalizedCountryName(code, locale.value, (key, fallback) => t(key, fallback))
    }))
    .sort((left, right) => left.name.localeCompare(right.name, locale.value))
})

const selectedCountry = computed(() => {
  return countries.value.find(country => country.code === form.value.countryCode) || {
    code: form.value.countryCode,
    name: getLocalizedCountryName(form.value.countryCode, locale.value, (key, fallback) => t(key, fallback))
  }
})

const filteredCountries = computed(() => {
  const keyword = countrySearch.value.trim().toLowerCase()
  if (!keyword) return countries.value

  return countries.value.filter(country =>
    country.name.toLowerCase().includes(keyword) ||
    country.code.toLowerCase().includes(keyword)
  )
})

function toggleCountryDropdown() {
  showCountryDropdown.value = !showCountryDropdown.value
  if (!showCountryDropdown.value) {
    countrySearch.value = ''
  }
}

function closeCountryDropdown() {
  showCountryDropdown.value = false
  countrySearch.value = ''
}

function selectCountry(code: string) {
  form.value.countryCode = code
  closeCountryDropdown()
}

// 验证并连接宿主机
async function verifyHost() {
  if (!hostId.value) return
  installStatus.value = 'verifying'
  verifyError.value = ''
  
  try {
    const res = await api.hosts.verify(hostId.value)
    if (res.success) {
      installStatus.value = 'success'
      toast.success(t('admin.hosts.verifySuccess'))
    }
  } catch (err: any) {
    installStatus.value = 'error'
    verifyError.value = err?.message || String(err)
    toast.error(t('admin.hosts.verifyFailed') + ': ' + verifyError.value)
  }
}

async function createHost() {
  // 根据用户角色确定节点名称
  let fullName: string
  if (isAdmin.value) {
    // 管理员直接使用完整名称
    if (!form.value.name.trim()) {
      toast.error(t('admin.hosts.hostNameRequired'))
      return
    }
    fullName = form.value.name
  } else {
    // 普通用户：前缀 + 后缀
    if (!form.value.nameSuffix.trim()) {
      toast.error(t('resources.hosts.nameSuffixRequired'))
      return
    }
    fullName = hostNamePrefix.value + form.value.nameSuffix
  }
  
  // 验证完整名称
  const nameValidation = validateIdentifier(fullName, t('admin.hosts.hostName'), 2, 64)
  if (!nameValidation.valid) { toast.error(nameValidation.message || t('admin.hosts.hostNameHint')); return }
  
  // 验证服务器地址
  if (!form.value.tunnelEnabled) {
    if (!form.value.hostAddress) { toast.error(t('admin.hosts.ipAddressRequired')); return }
    const ipValidation = validateHostAddress(form.value.hostAddress, t('admin.hosts.ipAddress'))
    if (!ipValidation.valid) { toast.error(ipValidation.message || ''); return }
  } else if (form.value.targetHost) {
    const targetHostValidation = validateHostAddress(form.value.targetHost, t('admin.hosts.targetHost'))
    if (!targetHostValidation.valid) { toast.error(targetHostValidation.message || ''); return }
  }
  
  // 验证描述（如果提供）
  if (form.value.location) {
    const locationValidation = validateName(form.value.location, t('admin.hosts.hostDesc'), 1, 100)
    if (!locationValidation.valid) { toast.error(locationValidation.message || ''); return }
  }
  
  // 网络模式场景映射：前端选项 -> 后端参数
  const networkModeMap: Record<string, { networkMode: string; ipv6Mode: number; needsIpv6Subnet: boolean; needsNatConfig: boolean }> = {
    'nat':          { networkMode: 'nat',      ipv6Mode: 3, needsIpv6Subnet: false, needsNatConfig: true },
    'nat_ipv6':     { networkMode: 'nat_ipv6', ipv6Mode: 1, needsIpv6Subnet: true,  needsNatConfig: true },
    'ipv6_only':    { networkMode: 'nat_ipv6', ipv6Mode: 1, needsIpv6Subnet: true,  needsNatConfig: false },
  }
  const modeConfig = networkModeMap[form.value.networkMode] || networkModeMap['nat']

  // 验证 IPv6 网段。支持先创建节点，执行脚本后再回来补充。
  if (modeConfig.needsIpv6Subnet) {
    const hasSubnet = form.value.ipv6Subnet && form.value.ipv6Subnet.trim()
    const hasInterface = form.value.ipv6ParentInterface && form.value.ipv6ParentInterface.trim()
    // 如果填了其中一个，则两个都必须填写
    if (hasSubnet && !hasInterface) {
      toast.error(t('admin.hosts.ipv6ParentInterfaceRequired'))
      return
    }
    if (!hasSubnet && hasInterface) {
      toast.error(t('admin.hosts.ipv6SubnetRequired'))
      return
    }
    // 如果填写了，校验格式
    if (hasSubnet && !form.value.ipv6Subnet.includes('/')) {
      toast.error(t('admin.hosts.ipv6SubnetInvalid'))
      return
    }
  }
  
  // 验证端口范围：结束端口不能小于起始端口
  if (form.value.portRangeStart != null && form.value.portRangeEnd != null) {
    if (form.value.portRangeEnd < form.value.portRangeStart) {
      toast.error(t('admin.hosts.portRangeEndMustBeGreater'))
      return
    }
  }

  // 验证 NAT 网卡 IP，支持 IPv4 / IPv6
  if (form.value.natPublicIp) {
    const natIpValidation = validateIpAddress(form.value.natPublicIp, t('admin.hosts.natPublicIpv4'))
    if (!natIpValidation.valid) {
      toast.error(natIpValidation.message || '')
      return
    }
  }
  if (form.value.natPublicIpv6) {
    const publicIpv6Validation = validateIpAddress(form.value.natPublicIpv6, t('admin.hosts.natPublicIpv6'))
    if (!publicIpv6Validation.valid || !(publicIpv6Validation.sanitized || form.value.natPublicIpv6).includes(':') ) {
      toast.error(t('admin.hosts.natPublicIpv6Invalid'))
      return
    }
  }
  if (form.value.natBindIp) {
    const bindIpValidation = validateIpAddress(form.value.natBindIp, t('admin.hosts.natBindIpv4'))
    if (!bindIpValidation.valid) {
      toast.error(bindIpValidation.message || '')
      return
    }
  }
  if (form.value.natBindIpv6) {
    const bindIpv6Validation = validateIpAddress(form.value.natBindIpv6, t('admin.hosts.natBindIpv6'))
    if (!bindIpv6Validation.valid || !(bindIpv6Validation.sanitized || form.value.natBindIpv6).includes(':') ) {
      toast.error(t('admin.hosts.natBindIpv6Invalid'))
      return
    }
  }

  saving.value = true
  try {
    const address = form.value.tunnelEnabled ? (form.value.targetHost?.trim() || '127.0.0.1') : form.value.hostAddress
    const port = form.value.tunnelEnabled ? (form.value.targetPort || 8443) : (form.value.apiPort || 8443)
    const url = buildHostApiUrl(address, port)
    
    const formData: any = {
      name: fullName,
      url: url,
      tunnelEnabled: form.value.tunnelEnabled,
      targetHost: form.value.tunnelEnabled ? (form.value.targetHost?.trim() || '127.0.0.1') : undefined,
      targetPort: form.value.tunnelEnabled ? (form.value.targetPort || 8443) : undefined,
      location: form.value.location || undefined,
      countryCode: form.value.countryCode,
      cpuAllowanceMax: form.value.cpuAllowanceMax || undefined,
      memoryMax: form.value.memoryMax || undefined,
      instanceType: form.value.instanceType,
      ipv6Mode: modeConfig.ipv6Mode,
      ipv6Subnet: modeConfig.needsIpv6Subnet ? form.value.ipv6Subnet.trim() : undefined,
      ipv6ParentInterface: modeConfig.needsIpv6Subnet ? form.value.ipv6ParentInterface.trim() : undefined,
      natConfig: modeConfig.needsNatConfig && (form.value.natPublicIp || form.value.natPublicIpv6 || form.value.natBindIp || form.value.natBindIpv6 || form.value.portRangeStart || form.value.portRangeEnd) ? {
        publicIp: form.value.natPublicIp || undefined,
        publicIpv6: form.value.natPublicIpv6 || undefined,
        bindIp: form.value.natBindIp || undefined,
        bindIpv6: form.value.natBindIpv6 || undefined,
        portRangeStart: form.value.portRangeStart || undefined,
        portRangeEnd: form.value.portRangeEnd || undefined
      } : undefined
    }

    const response = await api.hosts.create(formData)
    const command = (response as any).installCommand

    hostId.value = (response as any).host?.id
    toast.success(t('admin.hosts.hostAdded'))
    emit('created', (response as any).host)
    if (command) {
      installCommand.value = command
      showInstallScript.value = true
      installStatus.value = 'waiting'
    } else {
      closeAndGoBack()
    }
  } catch (err: any) {
    const errorCode = err?.code
    const errorMessage = errorCode ? t(`errors.${errorCode}`) : (err?.message || String(err))
    const details = err?.details ? ` (${err.details})` : ''
    toast.error(`${t('admin.hosts.addFailed')}: ${errorMessage}${details}`)
  } finally {
    saving.value = false
  }
}

function copyCommand() {
  navigator.clipboard.writeText(installCommand.value)
  toast.success(t('common.copied'))
}

function closeAndGoBack() {
  showInstallScript.value = false
  emit('update:show', false)
  emit('close')
}
</script>

<template>
  <DrawerModal
    :show="props.show"
    max-width="max-w-3xl lg:!max-w-4xl"
    raw
    @close="closeAndGoBack"
  >
    <!-- Header with Apple PageHeader -->
    <div class="modal-header border-b border-[var(--border-color)] bg-[var(--bg-surface)] px-6 py-4 shrink-0">
      <PageHeader
        :title="showInstallScript ? t('admin.hosts.installScript') : t('resources.hosts.create')"
        :description="showInstallScript ? t('admin.hosts.runOnHost') : t('resources.hosts.installHintTitle')"
        :back="true"
        :back-label="t('common.close')"
        class="!mb-0 w-full"
        @back="closeAndGoBack"
      >
        <template #actions>
          <button
            type="button"
            class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-tertiary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
            :aria-label="t('common.close')"
            @click="closeAndGoBack"
          >
            <X class="w-5 h-5" />
          </button>
        </template>
      </PageHeader>
    </div>

    <!-- Body -->
    <div class="modal-body p-6 space-y-6 overflow-y-auto flex-1 bg-[var(--bg-primary)]">
      <!-- If install script returned, show install steps -->
      <div v-if="showInstallScript" class="space-y-6">
        <!-- 步骤 1: 执行安装命令 -->
        <Card padding="md">
          <template #header>
            <div class="flex items-center justify-between gap-3 w-full">
              <div class="flex items-center gap-3">
                <span
                  class="flex items-center justify-center w-7 h-7 rounded-full text-xs font-semibold text-white transition-colors shrink-0"
                  :class="installStatus === 'success' ? 'bg-[var(--success)]' : 'bg-[var(--accent)]'"
                >
                  1
                </span>
                <div>
                  <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                    {{ t('admin.hosts.step1RunScript') }}
                  </h3>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                    {{ t('admin.hosts.runOnHost') }}
                  </p>
                </div>
              </div>
              <Button variant="secondary" size="sm" type="button" @click="copyCommand">
                <template #icon><Copy class="w-3.5 h-3.5" /></template>
                {{ t('admin.hosts.copyCommand') }}
              </Button>
            </div>
          </template>

          <div class="space-y-3">
            <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 overflow-x-auto">
              <code class="block break-all whitespace-pre-wrap font-mono text-xs text-[var(--text-primary)] leading-relaxed select-all">{{ installCommand }}</code>
            </div>
          </div>
        </Card>

        <!-- 步骤 2：验证并连接 -->
        <Card padding="md">
          <template #header>
            <div class="flex items-center justify-between gap-3 w-full">
              <div class="flex items-center gap-3">
                <span
                  class="flex items-center justify-center w-7 h-7 rounded-full text-xs font-semibold transition-colors shrink-0"
                  :class="installStatus === 'success' ? 'bg-[var(--success)] text-white' : 'bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] border border-[var(--border-color)]'"
                >
                  2
                </span>
                <div>
                  <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                    {{ t('admin.hosts.step2Verify') }}
                  </h3>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                    {{ t('admin.hosts.verifyHint') }}
                  </p>
                </div>
              </div>

              <div class="flex items-center gap-2">
                <Badge v-if="installStatus === 'success'" variant="success" size="md" dot>
                  {{ t('admin.hosts.verifySuccess') }}
                </Badge>
              </div>
            </div>
          </template>

          <div class="space-y-4">
            <div class="flex flex-wrap items-center gap-3">
              <Button
                variant="primary"
                size="md"
                type="button"
                :disabled="installStatus === 'verifying' || installStatus === 'success'"
                :loading="installStatus === 'verifying'"
                @click="verifyHost"
              >
                {{ installStatus === 'verifying' ? t('admin.hosts.verifying') : t('admin.hosts.verifyAndConnect') }}
              </Button>
            </div>

            <!-- 错误提示 -->
            <div
              v-if="installStatus === 'error' && verifyError"
              class="flex items-start gap-2.5 rounded-xl border border-[var(--error)]/30 bg-[var(--error)]/10 p-3.5 text-sm text-[var(--error)]"
            >
              <AlertCircle class="w-4 h-4 shrink-0 mt-0.5 text-[var(--error)]" />
              <span class="break-words">{{ verifyError }}</span>
            </div>
          </div>
        </Card>
      </div>

      <!-- Form view -->
      <template v-else>
        <!-- 部署说明 Hint banner -->
        <div class="flex items-start gap-3.5 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 sm:p-5">
          <div class="flex h-8 w-8 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface)] text-[var(--accent)]">
            <Info class="w-4 h-4 stroke-[2]" />
          </div>
          <div class="space-y-1.5 text-sm text-[var(--text-secondary)]">
            <p class="font-medium text-[var(--text-primary)]">{{ t('resources.hosts.ubuntuOnlyHint') }}</p>
            <p>{{ t('resources.hosts.installHintTitle') }}</p>
            <p class="text-xs text-[var(--text-tertiary)]">{{ t('resources.hosts.installHintIpv6') }}</p>
          </div>
        </div>

        <!-- 表单 Form Content -->
        <form id="host-create-form" class="space-y-6" @submit.prevent="createHost">
          <!-- 1. 基本信息 Basic Info -->
          <Card padding="md">
            <template #header>
              <div class="flex items-center gap-3">
                <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                  <Server class="w-5 h-5 stroke-[2]" />
                </div>
                <div>
                  <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                    {{ t('admin.hosts.basicInfo') }}
                  </h3>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                    {{ t('admin.hosts.basicInfoHint') }}
                  </p>
                </div>
              </div>
            </template>

            <div class="grid grid-cols-1 gap-5 md:grid-cols-2">
              <div class="space-y-1.5">
                <!-- 管理员输入完整节点名称 -->
                <template v-if="isAdmin">
                  <Input
                    v-model="form.name"
                    type="text"
                    :label="t('admin.hosts.hostName')"
                    placeholder="node-01"
                    class="font-mono"
                    :hint="t('admin.hosts.hostNameHint')"
                    @update:model-value="form.name = String($event).replace(/[^a-zA-Z0-9_-]/g, '')"
                  />
                </template>
                <!-- 普通用户输入节点名称后缀 -->
                <template v-else>
                  <Input
                    v-model="form.nameSuffix"
                    type="text"
                    :label="t('admin.hosts.hostName')"
                    placeholder="myhost"
                    class="font-mono"
                    :hint="t('resources.hosts.nameHint')"
                    @update:model-value="form.nameSuffix = String($event).replace(/[^a-zA-Z0-9_-]/g, '')"
                  >
                    <template #prefix>
                      <span class="font-mono text-xs font-semibold px-2 py-0.5 rounded-md bg-[var(--bg-surface)] text-[var(--text-secondary)] border border-[var(--border-color)] select-none mr-1 shrink-0">
                        {{ hostNamePrefix }}
                      </span>
                    </template>
                  </Input>
                </template>
                <p class="text-xs text-[var(--warning)] font-medium mt-1">{{ t('common.noIncudalHint') }}</p>
              </div>

              <div>
                <Input
                  v-model="form.location"
                  type="text"
                  :label="t('admin.hosts.hostDesc')"
                  placeholder="Hong Kong DC1"
                />
              </div>
            </div>
          </Card>

          <!-- 2. 连接与网络 Connection & Network -->
          <Card padding="md" class="relative z-20 !overflow-visible">
            <template #header>
              <div class="flex items-center gap-3">
                <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                  <Network class="w-5 h-5 stroke-[2]" />
                </div>
                <div>
                  <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                    {{ t('admin.hosts.connectionConfig') }}
                  </h3>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                    {{ t('admin.hosts.connectionConfigHint') }}
                  </p>
                </div>
              </div>
            </template>

            <div class="space-y-5">
              <!-- 连接方式选择 Connection Mode -->
              <div class="space-y-2">
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                  {{ t('admin.hosts.connectionMode') }}
                </label>
                <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
                  <div
                    class="group relative rounded-2xl border p-4 transition-all duration-300 cursor-pointer active:scale-[0.98]"
                    :class="form.tunnelEnabled
                      ? 'border-[var(--accent)] bg-[var(--bg-surface-soft)] ring-1 ring-[var(--accent)] shadow-sm'
                      : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface-soft)]/50'"
                    @click="form.tunnelEnabled = true"
                  >
                    <div class="flex items-center justify-between gap-2">
                      <div class="flex items-center gap-2.5">
                        <div
                          class="flex h-8 w-8 items-center justify-center rounded-xl bg-[var(--bg-surface)] transition-colors"
                          :class="form.tunnelEnabled ? 'text-[var(--accent)]' : 'text-[var(--text-tertiary)]'"
                        >
                          <Radio class="w-4 h-4" />
                        </div>
                        <span
                          class="text-sm font-semibold tracking-tight"
                          :class="form.tunnelEnabled ? 'text-[var(--accent)]' : 'text-[var(--text-primary)]'"
                        >
                          {{ t('admin.hosts.tunnelMode') }}
                        </span>
                      </div>
                      <Badge v-if="form.tunnelEnabled" variant="info" size="sm">
                        <Check class="w-3.5 h-3.5 stroke-[2.5]" />
                      </Badge>
                    </div>
                  </div>

                  <div
                    class="group relative rounded-2xl border p-4 transition-all duration-300 cursor-pointer active:scale-[0.98]"
                    :class="!form.tunnelEnabled
                      ? 'border-[var(--accent)] bg-[var(--bg-surface-soft)] ring-1 ring-[var(--accent)] shadow-sm'
                      : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface-soft)]/50'"
                    @click="form.tunnelEnabled = false"
                  >
                    <div class="flex items-center justify-between gap-2">
                      <div class="flex items-center gap-2.5">
                        <div
                          class="flex h-8 w-8 items-center justify-center rounded-xl bg-[var(--bg-surface)] transition-colors"
                          :class="!form.tunnelEnabled ? 'text-[var(--accent)]' : 'text-[var(--text-tertiary)]'"
                        >
                          <Globe class="w-4 h-4" />
                        </div>
                        <span
                          class="text-sm font-semibold tracking-tight"
                          :class="!form.tunnelEnabled ? 'text-[var(--accent)]' : 'text-[var(--text-primary)]'"
                        >
                          {{ t('admin.hosts.directMode') }}
                        </span>
                      </div>
                      <Badge v-if="!form.tunnelEnabled" variant="info" size="sm">
                        <Check class="w-3.5 h-3.5 stroke-[2.5]" />
                      </Badge>
                    </div>
                  </div>
                </div>
              </div>

              <!-- 直连模式输入框 Direct Mode Inputs -->
              <div v-if="!form.tunnelEnabled" class="grid grid-cols-1 gap-4 md:grid-cols-[1fr_140px]">
                <div>
                  <Input
                    v-model="form.hostAddress"
                    type="text"
                    :label="t('admin.hosts.ipAddress')"
                    placeholder="10.0.0.1 / 2001:db8::1 / node.example.com"
                    class="font-mono"
                    :hint="t('admin.hosts.ipAddressHint')"
                    :required="!form.tunnelEnabled"
                  />
                </div>
                <div>
                  <Input
                    v-model.number="form.apiPort"
                    type="number"
                    :label="t('admin.hosts.apiPort')"
                    placeholder="8443"
                    min="1"
                    max="65535"
                    class="font-mono tabular-nums"
                    :hint="t('admin.hosts.apiPortHint')"
                    :required="!form.tunnelEnabled"
                  />
                </div>
              </div>

              <!-- 穿透模式输入框与提示 Tunnel Mode Inputs -->
              <div v-else class="space-y-4">
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 text-xs text-[var(--text-secondary)] leading-relaxed">
                  {{ t('admin.hosts.tunnelNotice') }}
                </div>
                <div class="grid grid-cols-1 gap-4 md:grid-cols-[140px_1fr]">
                  <div>
                    <Input
                      v-model.number="form.targetPort"
                      type="number"
                      :label="t('admin.hosts.targetPort')"
                      placeholder="8443"
                      min="1"
                      max="65535"
                      class="font-mono tabular-nums"
                      :hint="t('admin.hosts.targetPortHint')"
                      required
                    />
                  </div>
                  <div>
                    <Input
                      v-model="form.targetHost"
                      type="text"
                      :label="t('admin.hosts.targetHost')"
                      placeholder="127.0.0.1"
                      class="font-mono"
                      :hint="t('admin.hosts.targetHostHint')"
                    />
                  </div>
                </div>
              </div>

              <!-- 国家与网络模式 Country & Network Mode -->
              <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
                <!-- 国家选择 Country Dropdown -->
                <div ref="countryDropdownRef" class="relative z-30 space-y-1.5">
                  <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none block">
                    {{ t('admin.hosts.country') }}
                  </label>
                  <button
                    type="button"
                    class="flex w-full items-center justify-between rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3.5 min-h-[44px] text-sm text-[var(--text-primary)] hover:border-[var(--border-hover)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all active:scale-[0.99]"
                    @click="toggleCountryDropdown"
                  >
                    <span class="flex items-center gap-2.5 font-medium truncate">
                      <FlagIcon :code="form.countryCode" size="sm" class="shrink-0" />
                      <span class="truncate">{{ selectedCountry?.name }}</span>
                    </span>
                    <ChevronDown
                      class="w-4 h-4 text-[var(--text-tertiary)] shrink-0 transition-transform duration-200"
                      :class="showCountryDropdown ? 'rotate-180' : ''"
                    />
                  </button>

                  <div
                    v-if="showCountryDropdown"
                    class="dropdown-menu absolute z-50 mt-1.5 w-full overflow-hidden rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-[0_8px_24px_rgba(0,0,0,0.12)]"
                  >
                    <div class="p-2 border-b border-[var(--border-color)] bg-[var(--bg-surface)]">
                      <div class="relative flex items-center">
                        <Search class="absolute left-3 w-4 h-4 text-[var(--text-tertiary)] pointer-events-none" />
                        <input
                          v-model="countrySearch"
                          type="search"
                          class="w-full pl-9 pr-3 py-2 text-sm rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all"
                          :placeholder="t('common.searchPlaceholder')"
                          autocomplete="off"
                          autofocus
                          @keydown.enter.prevent.stop
                          @keydown.stop
                        />
                      </div>
                    </div>
                    <div class="max-h-60 overflow-y-auto p-1.5 space-y-0.5 bg-[var(--bg-surface)]">
                      <button
                        v-for="c in filteredCountries"
                        :key="c.code"
                        type="button"
                        class="flex w-full items-center gap-2.5 px-3 py-2 rounded-xl text-left text-sm transition-colors duration-150 active:scale-[0.99]"
                        :class="form.countryCode === c.code ? 'bg-[var(--bg-surface-soft)] text-[var(--accent)] font-semibold' : 'text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)]'"
                        @click="selectCountry(c.code)"
                      >
                        <FlagIcon :code="c.code" size="sm" class="shrink-0" />
                        <span class="truncate flex-1">{{ c.name }}</span>
                        <Check v-if="form.countryCode === c.code" class="w-4 h-4 text-[var(--accent)] shrink-0 stroke-[2.5]" />
                      </button>
                      <div v-if="filteredCountries.length === 0" class="px-3 py-6 text-center text-sm text-[var(--text-tertiary)]">
                        {{ t('common.noSearchResults') }}
                      </div>
                    </div>
                  </div>
                </div>

                <!-- 网络模式选择 Network Mode -->
                <div>
                  <Select
                    v-model="form.networkMode"
                    :label="t('admin.hosts.networkModeLabel')"
                  >
                    <option value="nat">{{ t('admin.hosts.networkModeNat') }}</option>
                    <option value="nat_ipv6">{{ t('admin.hosts.networkModeNatIpv6') }}</option>
                    <option value="ipv6_only">{{ t('admin.hosts.networkModeIpv6Only') }}</option>
                  </Select>
                </div>
              </div>

              <!-- IPv6 网段输入框 (仅 nat_ipv6 / ipv6_only) -->
              <Transition name="fade">
                <div
                  v-if="form.networkMode === 'nat_ipv6' || form.networkMode === 'ipv6_only'"
                  class="space-y-4 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 sm:p-5"
                >
                  <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
                    <div>
                      <Input
                        v-model="form.ipv6Subnet"
                        type="text"
                        :label="t('admin.hosts.ipv6Subnet')"
                        placeholder="2001:db8::/48"
                        class="font-mono"
                        :hint="t('admin.hosts.ipv6SubnetHint')"
                      />
                    </div>
                    <div>
                      <Input
                        v-model="form.ipv6ParentInterface"
                        type="text"
                        :label="t('admin.hosts.ipv6ParentInterface')"
                        placeholder="eth0"
                        class="font-mono"
                        :hint="t('admin.hosts.ipv6ParentInterfaceHint')"
                      />
                    </div>
                  </div>
                  <p class="text-xs text-[var(--warning)] font-medium">
                    {{ t('resources.hosts.ipv6OptionalHint') }}
                  </p>
                </div>
              </Transition>
            </div>
          </Card>

          <!-- 3. NAT 端口映射配置 (仅 IPv4 相关模式显示) -->
          <Card v-if="form.networkMode !== 'ipv6_only'" padding="md" class="relative z-10">
            <template #header>
              <div class="flex items-center gap-3">
                <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                  <Globe class="w-5 h-5 stroke-[2]" />
                </div>
                <div>
                  <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                    {{ t('admin.hosts.natConfig') }}
                  </h3>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                    {{ t('admin.hosts.natConfigHint') }}
                  </p>
                </div>
              </div>
            </template>

            <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
              <div>
                <Input
                  v-model="form.natPublicIp"
                  type="text"
                  :label="t('admin.hosts.natPublicIpv4')"
                  :placeholder="t('admin.hosts.natPublicIpv4Placeholder')"
                  :hint="t('admin.hosts.natPublicIpv4Desc')"
                  class="font-mono"
                />
              </div>
              <div>
                <Input
                  v-model="form.natPublicIpv6"
                  type="text"
                  :label="t('admin.hosts.natPublicIpv6')"
                  :placeholder="t('admin.hosts.natPublicIpv6Placeholder')"
                  :hint="t('admin.hosts.natPublicIpv6Desc')"
                  class="font-mono"
                />
              </div>
              <div>
                <Input
                  v-model="form.natBindIp"
                  type="text"
                  :label="t('admin.hosts.natBindIpv4')"
                  :placeholder="t('admin.hosts.natBindIpv4Placeholder')"
                  :hint="t('admin.hosts.natBindIpv4Desc')"
                  class="font-mono"
                />
              </div>
              <div>
                <Input
                  v-model="form.natBindIpv6"
                  type="text"
                  :label="t('admin.hosts.natBindIpv6')"
                  :placeholder="t('admin.hosts.natBindIpv6Placeholder')"
                  :hint="t('admin.hosts.natBindIpv6Desc')"
                  class="font-mono"
                />
              </div>
              <div>
                <Input
                  :model-value="form.portRangeStart ?? ''"
                  type="number"
                  :label="t('admin.hosts.portRangeStart')"
                  placeholder="10000"
                  class="font-mono tabular-nums"
                  @update:model-value="form.portRangeStart = $event === '' || $event == null ? null : Number($event)"
                />
              </div>
              <div>
                <Input
                  :model-value="form.portRangeEnd ?? ''"
                  type="number"
                  :label="t('admin.hosts.portRangeEnd')"
                  placeholder="65000"
                  class="font-mono tabular-nums"
                  @update:model-value="form.portRangeEnd = $event === '' || $event == null ? null : Number($event)"
                />
              </div>
            </div>
          </Card>

          <!-- 4. 资源限制 Resource Limits -->
          <Card padding="md">
            <template #header>
              <div class="flex items-center gap-3">
                <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                  <Cpu class="w-5 h-5 stroke-[2]" />
                </div>
                <div>
                  <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                    {{ t('admin.hosts.resourceTitle') }}
                  </h3>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                    {{ t('admin.hosts.resourceTitleHint') }}
                  </p>
                </div>
              </div>
            </template>

            <div class="grid grid-cols-1 gap-4 md:grid-cols-3">
              <div>
                <Input
                  v-model.number="form.cpuAllowanceMax"
                  type="number"
                  :label="t('admin.hosts.cpuAllowanceMax')"
                  placeholder="0"
                  class="font-mono tabular-nums"
                />
              </div>
              <div>
                <Input
                  v-model.number="form.memoryMax"
                  type="number"
                  :label="`${t('admin.hosts.memoryMax')} (MB)`"
                  placeholder="0"
                  class="font-mono tabular-nums"
                />
              </div>
              <div>
                <Select
                  v-model="form.instanceType"
                  :label="t('admin.hosts.instanceTypeLabel')"
                >
                  <option value="container">{{ t('admin.hosts.typeContainer') }}</option>
                  <option value="vm">{{ t('admin.hosts.typeVm') }}</option>
                  <option value="both">{{ t('admin.hosts.typeBoth') }}</option>
                </Select>
              </div>
            </div>
          </Card>

          <!-- 存储池提示 Storage Pool Note -->
          <div class="flex items-start gap-3.5 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 sm:p-5">
            <div class="flex h-8 w-8 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface)] text-[var(--accent)]">
              <Info class="w-4 h-4 stroke-[2]" />
            </div>
            <p class="text-xs leading-relaxed text-[var(--text-secondary)] mt-1">
              {{ t('resources.hosts.storagePoolAfterConnectHint') }}
            </p>
          </div>
        </form>
      </template>
    </div>

    <!-- Footer -->
    <div class="modal-footer border-t border-[var(--border-color)] bg-[var(--bg-surface)] px-6 py-4 flex items-center justify-end gap-3 shrink-0">
      <template v-if="showInstallScript">
        <Button variant="secondary" size="md" type="button" @click="closeAndGoBack">
          {{ t('common.close') }}
        </Button>
        <Button v-if="installStatus === 'success'" variant="primary" size="md" type="button" @click="closeAndGoBack">
          {{ t('common.done') }}
        </Button>
      </template>
      <template v-else>
        <Button variant="secondary" size="md" type="button" @click="closeAndGoBack">
          {{ t('common.cancel') }}
        </Button>
        <Button
          variant="primary"
          size="md"
          type="submit"
          form="host-create-form"
          :disabled="saving || (isAdmin ? !form.name : !form.nameSuffix) || (!form.tunnelEnabled && !form.hostAddress)"
          :loading="saving"
          @click="createHost"
        >
          {{ t('common.create') }}
        </Button>
      </template>
    </div>
  </DrawerModal>
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  *,
  *::before,
  *::after {
    transition: none !important;
    animation: none !important;
    transform: none !important;
  }
}
</style>