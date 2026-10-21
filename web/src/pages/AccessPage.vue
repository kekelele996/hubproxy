<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  AlertTriangle,
  ArrowDownToLine,
  ArrowUpFromLine,
  ChevronLeft,
  ChevronRight,
  KeyRound,
  Loader2,
  RefreshCw,
  Search,
  ShieldCheck,
} from 'lucide-vue-next'
import {
  clearAdminToken,
  fetchAccessLogs,
  fetchAccessSummary,
  getAdminToken,
  saveAdminToken,
  type AccessQueryParams,
  type AccessRecord,
  type AccessSummaryResponse,
} from '@/api'
import { errorMessage } from '@/lib/utils'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'
import PageHero from '@/components/PageHero.vue'

const TYPE_OPTIONS = [
  { value: '', label: '全部类型' },
  { value: 'docker', label: 'Docker 镜像' },
  { value: 'github', label: 'GitHub / 资源站' },
  { value: 'token', label: '认证请求' },
  { value: 'image_tar', label: '离线镜像包' },
  { value: 'api', label: '其他 API' },
] as const

const GROUP_OPTIONS = [
  { value: 'ip', label: '按来源 IP' },
  { value: 'day', label: '按天' },
  { value: 'hour', label: '按小时' },
  { value: 'target', label: '按目标资源' },
] as const

const PAGE_SIZE = 50
const MAX_RENDER_GROUPS = 200

const hasToken = ref(getAdminToken() !== '')
const tokenDraft = ref(getAdminToken())
const needToken = ref(false)
const tokenError = ref('')

const loading = ref(false)
const loadError = ref('')
const groupBy = ref('ip')
const summary = ref<AccessSummaryResponse | null>(null)

const logs = ref<AccessRecord[]>([])
const logsTotal = ref(0)
const page = ref(1)

const filterIP = ref('')
const filterTarget = ref('')
const filterType = ref('')
const filterStart = ref('')
const filterEnd = ref('')
const applied = ref<AccessQueryParams>({})

const totalPages = computed(() => Math.max(1, Math.ceil(logsTotal.value / PAGE_SIZE)))
const visibleGroups = computed(() => summary.value?.groups.slice(0, MAX_RENDER_GROUPS) ?? [])
const hiddenGroupsCount = computed(() => Math.max(0, (summary.value?.groups.length ?? 0) - MAX_RENDER_GROUPS))

function buildQuery(): AccessQueryParams {
  const q: AccessQueryParams = {}
  if (filterIP.value.trim()) q.ip = filterIP.value.trim()
  if (filterTarget.value.trim()) q.target = filterTarget.value.trim()
  if (filterType.value) q.type = filterType.value
  if (filterStart.value) q.start = Math.floor(new Date(filterStart.value).getTime() / 1000)
  if (filterEnd.value) q.end = Math.floor(new Date(filterEnd.value).getTime() / 1000)
  return q
}

async function loadSummary() {
  const data = await fetchAccessSummary(groupBy.value, applied.value)
  summary.value = data
  return data
}

async function loadLogs(targetPage = page.value) {
  const data = await fetchAccessLogs(applied.value, targetPage, PAGE_SIZE)
  logs.value = data.items
  logsTotal.value = data.total
  page.value = targetPage
}

async function refresh() {
  loading.value = true
  loadError.value = ''
  try {
    const [nextSummary] = await Promise.all([loadSummary(), loadLogs(page.value)])
    summary.value = nextSummary
    needToken.value = false
  } catch (e) {
    if (e instanceof Error && 'status' in e && (e.status === 401 || e.status === 403)) {
      needToken.value = true
      tokenError.value = errorMessage(e, '无权访问')
    } else {
      loadError.value = errorMessage(e, '加载访问统计失败')
    }
  } finally {
    loading.value = false
  }
}

function applyFilters() {
  applied.value = buildQuery()
  void refresh()
}

function resetFilters() {
  filterIP.value = ''
  filterTarget.value = ''
  filterType.value = ''
  filterStart.value = ''
  filterEnd.value = ''
  applied.value = {}
  void refresh()
}

function switchGroup(value: string) {
  groupBy.value = value
  void loadSummary().catch((e) => {
    loadError.value = errorMessage(e, '加载汇总失败')
  })
}

function drillGroup(key: string) {
  if (groupBy.value === 'ip') {
    filterIP.value = key
  } else if (groupBy.value === 'target') {
    filterTarget.value = key
  } else {
    return
  }
  applyFilters()
}

async function gotoPage(next: number) {
  if (next < 1 || next > totalPages.value || loading.value) return
  loading.value = true
  loadError.value = ''
  try {
    await loadLogs(next)
  } catch (e) {
    loadError.value = errorMessage(e, '加载明细失败')
  } finally {
    loading.value = false
  }
}

async function submitToken() {
  if (!tokenDraft.value.trim()) return
  saveAdminToken(tokenDraft.value)
  hasToken.value = true
  needToken.value = false
  tokenError.value = ''
  await refresh()
}

function forgetToken() {
  clearAdminToken()
  hasToken.value = false
  tokenDraft.value = ''
}

function formatBytes(bytes: number): string {
  if (!bytes || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let size = bytes
  let i = 0
  while (size >= 1024 && i < units.length - 1) {
    size /= 1024
    i++
  }
  return `${size.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

function formatDateTime(value: string): string {
  if (!value) return '-'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return '-'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(2)}s`
}

function typeLabel(type: string): string {
  return TYPE_OPTIONS.find((t) => t.value === type)?.label ?? type
}

onMounted(() => {
  void refresh()
})
</script>

<template>
  <div>
    <PageHero
      eyebrow="Admin"
      title="访问统计"
      subtitle="记录每次代理访问的来源、目标、时间与流量，按来源或时间排查异常拉取。"
    />

    <!-- 令牌面板 -->
    <div
      v-if="needToken"
      class="mx-auto mt-8 max-w-xl space-y-4 rounded-2xl border border-border bg-card p-6"
    >
      <div class="flex items-center gap-2 text-lg font-medium">
        <ShieldCheck class="size-5 text-primary" />
        管理员验证
      </div>
      <p class="text-sm text-muted-foreground">
        访问统计接口需要管理员令牌。请在服务端配置
        <code class="rounded bg-muted px-1.5 py-0.5 text-[13px]">admin.token</code>
        （或
        <code class="rounded bg-muted px-1.5 py-0.5 text-[13px]">ADMIN_TOKEN</code>
        ），并在此填写。
      </p>
      <form class="flex gap-2" @submit.prevent="submitToken">
        <Input
          v-model="tokenDraft"
          type="password"
          placeholder="请输入管理员令牌"
          class="flex-1"
        />
        <Button :disabled="!tokenDraft.trim()" @click="submitToken">
          <KeyRound class="size-4" />
          确认
        </Button>
      </form>
      <p v-if="tokenError" class="text-sm text-destructive">{{ tokenError }}</p>
    </div>

    <div v-else class="mx-auto mt-8 max-w-6xl space-y-6">
      <!-- 工具栏 -->
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div class="flex flex-wrap gap-1.5">
          <button
            v-for="opt in GROUP_OPTIONS"
            :key="opt.value"
            type="button"
            class="rounded-full px-4 py-2 text-sm transition-colors duration-150"
            :class="
              groupBy === opt.value
                ? 'bg-primary text-primary-foreground'
                : 'text-muted-foreground hover:bg-accent hover:text-foreground'
            "
            @click="switchGroup(opt.value)"
          >
            {{ opt.label }}
          </button>
        </div>
        <div class="flex items-center gap-1.5">
          <Button v-if="hasToken" variant="ghost" size="sm" @click="forgetToken">
            清除令牌
          </Button>
          <Button variant="outline" size="sm" :disabled="loading" @click="refresh">
            <Loader2 v-if="loading" class="size-4 animate-spin" />
            <RefreshCw v-else class="size-4" />
            刷新
          </Button>
        </div>
      </div>

      <!-- 筛选 -->
      <div class="grid gap-2.5 sm:grid-cols-2 lg:grid-cols-5">
        <Input v-model="filterIP" placeholder="来源 IP（精确）" />
        <Input v-model="filterTarget" placeholder="目标关键字" />
        <select
          v-model="filterType"
          class="h-11 rounded-lg border border-input bg-transparent px-3 text-base outline-none focus-visible:border-ring dark:[color-scheme:dark]"
        >
          <option v-for="opt in TYPE_OPTIONS" :key="opt.value" :value="opt.value">
            {{ opt.label }}
          </option>
        </select>
        <input
          v-model="filterStart"
          type="datetime-local"
          class="h-11 rounded-lg border border-input bg-transparent px-3 text-base outline-none focus-visible:border-ring dark:[color-scheme:dark]"
        >
        <input
          v-model="filterEnd"
          type="datetime-local"
          class="h-11 rounded-lg border border-input bg-transparent px-3 text-base outline-none focus-visible:border-ring dark:[color-scheme:dark]"
        >
      </div>
      <div class="flex justify-end gap-2">
        <Button variant="outline" size="sm" @click="resetFilters">重置</Button>
        <Button size="sm" @click="applyFilters">
          <Search class="size-4" />
          查询
        </Button>
      </div>

      <p v-if="loadError" class="text-center text-destructive">{{ loadError }}</p>

      <template v-if="summary">
        <!-- 总览卡片 -->
        <div class="grid grid-cols-2 gap-3 lg:grid-cols-4">
          <div class="rounded-2xl border border-border bg-card p-5">
            <p class="text-sm text-muted-foreground">总请求数</p>
            <p class="mt-2 text-2xl font-semibold tracking-tight">
              {{ summary.totals.requests.toLocaleString() }}
            </p>
          </div>
          <div class="rounded-2xl border border-border bg-card p-5">
            <p class="flex items-center gap-1.5 text-sm text-muted-foreground">
              <ArrowDownToLine class="size-3.5" /> 下行流量
            </p>
            <p class="mt-2 text-2xl font-semibold tracking-tight">
              {{ formatBytes(summary.totals.bytes_out) }}
            </p>
          </div>
          <div class="rounded-2xl border border-border bg-card p-5">
            <p class="flex items-center gap-1.5 text-sm text-muted-foreground">
              <ArrowUpFromLine class="size-3.5" /> 上行流量
            </p>
            <p class="mt-2 text-2xl font-semibold tracking-tight">
              {{ formatBytes(summary.totals.bytes_in) }}
            </p>
          </div>
          <div class="rounded-2xl border border-border bg-card p-5">
            <p class="flex items-center gap-1.5 text-sm text-muted-foreground">
              <AlertTriangle class="size-3.5" /> 错误请求
            </p>
            <p
              class="mt-2 text-2xl font-semibold tracking-tight"
              :class="summary.totals.errors > 0 ? 'text-destructive' : ''"
            >
              {{ summary.totals.errors.toLocaleString() }}
            </p>
          </div>
        </div>

        <!-- 汇总分组 -->
        <section class="space-y-3">
          <h2 class="text-lg font-medium">
            {{ GROUP_OPTIONS.find((g) => g.value === groupBy)?.label }}汇总
          </h2>
          <div class="overflow-x-auto rounded-2xl border border-border">
            <table class="w-full min-w-[640px] text-sm">
              <thead class="bg-muted/60 text-left text-muted-foreground">
                <tr>
                  <th class="px-4 py-3 font-medium">分组</th>
                  <th class="px-4 py-3 text-right font-medium">请求数</th>
                  <th class="px-4 py-3 text-right font-medium">下行流量</th>
                  <th class="px-4 py-3 text-right font-medium">上行流量</th>
                  <th class="px-4 py-3 text-right font-medium">错误</th>
                  <th class="px-4 py-3 text-right font-medium">最近访问</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-border">
                <tr v-if="visibleGroups.length === 0">
                  <td colspan="6" class="px-4 py-8 text-center text-muted-foreground">
                    当前筛选条件下暂无记录
                  </td>
                </tr>
                <tr
                  v-for="g in visibleGroups"
                  :key="g.key"
                  class="transition-colors hover:bg-accent/50"
                  :class="groupBy === 'ip' || groupBy === 'target' ? 'cursor-pointer' : ''"
                  :title="
                    groupBy === 'ip' || groupBy === 'target' ? '点击查看该分组的访问明细' : ''
                  "
                  @click="drillGroup(g.key)"
                >
                  <td class="max-w-[280px] truncate px-4 py-3 font-mono text-[13px]">{{ g.key || '-' }}</td>
                  <td class="px-4 py-3 text-right">{{ g.requests.toLocaleString() }}</td>
                  <td class="px-4 py-3 text-right font-medium">{{ formatBytes(g.bytes_out) }}</td>
                  <td class="px-4 py-3 text-right text-muted-foreground">{{ formatBytes(g.bytes_in) }}</td>
                  <td
                    class="px-4 py-3 text-right"
                    :class="g.errors > 0 ? 'text-destructive' : 'text-muted-foreground'"
                  >
                    {{ g.errors }}
                  </td>
                  <td class="whitespace-nowrap px-4 py-3 text-right text-muted-foreground">
                    {{ formatDateTime(g.last_at) }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-if="hiddenGroupsCount > 0" class="text-center text-xs text-muted-foreground">
            仅展示流量最大的前 {{ MAX_RENDER_GROUPS }} 个分组，其余 {{ hiddenGroupsCount }} 个可通过筛选缩小范围查看
          </p>
        </section>

        <!-- 访问明细 -->
        <section class="space-y-3">
          <div class="flex items-baseline justify-between">
            <h2 class="text-lg font-medium">访问明细</h2>
            <span class="text-sm text-muted-foreground">共 {{ logsTotal.toLocaleString() }} 条</span>
          </div>
          <div class="overflow-x-auto rounded-2xl border border-border">
            <table class="w-full min-w-[900px] text-sm">
              <thead class="bg-muted/60 text-left text-muted-foreground">
                <tr>
                  <th class="whitespace-nowrap px-4 py-3 font-medium">时间</th>
                  <th class="px-4 py-3 font-medium">来源</th>
                  <th class="px-4 py-3 font-medium">类型</th>
                  <th class="px-4 py-3 font-medium">目标</th>
                  <th class="px-4 py-3 font-medium">方法</th>
                  <th class="px-4 py-3 text-right font-medium">状态</th>
                  <th class="px-4 py-3 text-right font-medium">下行</th>
                  <th class="px-4 py-3 text-right font-medium">耗时</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-border">
                <tr v-if="logs.length === 0">
                  <td colspan="8" class="px-4 py-8 text-center text-muted-foreground">
                    暂无访问记录
                  </td>
                </tr>
                <tr v-for="r in logs" :key="r.id" class="hover:bg-accent/50">
                  <td class="whitespace-nowrap px-4 py-2.5 text-muted-foreground">
                    {{ formatDateTime(r.time) }}
                  </td>
                  <td class="whitespace-nowrap px-4 py-2.5 font-mono text-[13px]">{{ r.ip }}</td>
                  <td class="whitespace-nowrap px-4 py-2.5">
                    <span class="rounded-full bg-primary/10 px-2 py-0.5 text-[11px] text-primary">
                      {{ typeLabel(r.type) }}
                    </span>
                  </td>
                  <td class="max-w-[300px] truncate px-4 py-2.5 font-mono text-[12px]" :title="r.target || r.path">
                    {{ r.target || r.path || '-' }}
                  </td>
                  <td class="whitespace-nowrap px-4 py-2.5 text-muted-foreground">{{ r.method }}</td>
                  <td
                    class="whitespace-nowrap px-4 py-2.5 text-right font-mono text-[13px]"
                    :class="r.status_code >= 400 ? 'text-destructive' : ''"
                  >
                    {{ r.status_code }}
                  </td>
                  <td class="whitespace-nowrap px-4 py-2.5 text-right">{{ formatBytes(r.bytes_out) }}</td>
                  <td class="whitespace-nowrap px-4 py-2.5 text-right text-muted-foreground">
                    {{ formatDuration(r.duration_ms) }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <div v-if="totalPages > 1" class="flex items-center justify-center gap-1.5">
            <Button variant="outline" size="sm" :disabled="loading || page <= 1" @click="gotoPage(page - 1)">
              <ChevronLeft class="size-4" />
            </Button>
            <span class="min-w-24 text-center text-sm text-muted-foreground">
              第 {{ page }} / {{ totalPages }} 页
            </span>
            <Button
              variant="outline"
              size="sm"
              :disabled="loading || page >= totalPages"
              @click="gotoPage(page + 1)"
            >
              <ChevronRight class="size-4" />
            </Button>
          </div>
        </section>
      </template>
    </div>
  </div>
</template>
