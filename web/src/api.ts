class ApiError extends Error {
  status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

async function parseError(res: Response): Promise<string> {
  const contentType = res.headers.get('Content-Type') || ''
  if (contentType.includes('application/json')) {
    try {
      const data = (await res.json()) as { error?: string; message?: string }
      return data.error || data.message || `请求失败 (${res.status})`
    } catch {
      return `请求失败 (${res.status})`
    }
  }
  try {
    const text = await res.text()
    return text || `请求失败 (${res.status})`
  } catch {
    return `请求失败 (${res.status})`
  }
}

async function getJSON<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(url, {
    ...init,
    headers: {
      Accept: 'application/json',
      ...(init?.headers || {}),
    },
    cache: 'no-store',
  })
  if (!res.ok) throw new ApiError(await parseError(res), res.status)
  return (await res.json()) as T
}

export interface PrepareDownloadResponse {
  download_url: string
}

export interface ImageInfoResponse {
  success: boolean
}

export interface Repository {
  repo_name?: string
  short_description?: string
  is_official?: boolean
  star_count?: number
  pull_count?: number
  namespace?: string
}

export interface SearchResponse {
  count: number
  results: Repository[]
}

export interface TagInfo {
  name: string
  last_updated?: string
  full_size?: number
  images?: Array<{
    architecture?: string
    os?: string
    variant?: string
    size?: number
  }>
}

export interface TagPageResult {
  tags: TagInfo[]
  has_more: boolean
}

export function prepareSingleDownload(params: {
  image: string
  platform?: string
  compressed: boolean
}) {
  const q = new URLSearchParams()
  q.set('image', params.image)
  q.set('mode', 'prepare')
  q.set('compressed', String(params.compressed))
  if (params.platform?.trim()) q.set('platform', params.platform.trim())
  return getJSON<PrepareDownloadResponse>(`/api/image/download?${q}`)
}

export function fetchImageInfo(image: string) {
  const q = new URLSearchParams({ image })
  return getJSON<ImageInfoResponse>(`/api/image/info?${q}`)
}

export function prepareBatchDownload(body: {
  images: string[]
  platform?: string
  useCompressedLayers: boolean
}) {
  return getJSON<PrepareDownloadResponse>('/api/image/batch?mode=prepare', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

export function searchImages(q: string, page: number, pageSize = 25) {
  const params = new URLSearchParams({
    q,
    page: String(page),
    page_size: String(pageSize),
  })
  return getJSON<SearchResponse>(`/api/search?${params}`)
}

export function fetchTags(namespace: string, name: string, page: number, pageSize = 100) {
  const params = new URLSearchParams({
    page: String(page),
    page_size: String(pageSize),
  })
  return getJSON<TagPageResult>(
    `/api/tags/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}?${params}`,
  )
}

export function triggerDownload(url: string) {
  const link = document.createElement('a')
  link.href = url
  link.style.display = 'none'
  document.body.appendChild(link)
  link.click()
  document.body.removeChild(link)
}

// ===== 访问统计（管理接口）=====

const ADMIN_TOKEN_KEY = 'hubproxy_admin_token'

export function getAdminToken(): string {
  return localStorage.getItem(ADMIN_TOKEN_KEY) || ''
}

export function saveAdminToken(token: string) {
  localStorage.setItem(ADMIN_TOKEN_KEY, token.trim())
}

export function clearAdminToken() {
  localStorage.removeItem(ADMIN_TOKEN_KEY)
}

export interface AccessRecord {
  id: number
  time: string
  ip: string
  user_agent: string
  method: string
  path: string
  type: string
  target: string
  status_code: number
  bytes_in: number
  bytes_out: number
  duration_ms: number
}

export interface AccessSummaryGroup {
  key: string
  requests: number
  bytes_in: number
  bytes_out: number
  errors: number
  first_at: string
  last_at: string
}

export interface AccessTotals {
  requests: number
  bytes_in: number
  bytes_out: number
  errors: number
  first_at: string
  last_at: string
}

export interface AccessSummaryResponse {
  group_by: string
  totals: AccessTotals
  groups: AccessSummaryGroup[]
}

export interface AccessLogsResponse {
  total: number
  page: number
  page_size: number
  items: AccessRecord[]
}

export interface AccessQueryParams {
  ip?: string
  type?: string
  target?: string
  start?: number
  end?: number
}

function appendAccessFilters(params: URLSearchParams, q?: AccessQueryParams) {
  if (q?.ip) params.set('ip', q.ip)
  if (q?.type) params.set('type', q.type)
  if (q?.target) params.set('target', q.target)
  if (q?.start) params.set('start', String(q.start))
  if (q?.end) params.set('end', String(q.end))
}

async function adminGetJSON<T>(url: string): Promise<T> {
  const token = getAdminToken()
  const res = await fetch(url, {
    headers: {
      Accept: 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    cache: 'no-store',
  })
  if (!res.ok) throw new ApiError(await parseError(res), res.status)
  return (await res.json()) as T
}

export function fetchAccessSummary(groupBy: string, q?: AccessQueryParams) {
  const params = new URLSearchParams({ group_by: groupBy })
  appendAccessFilters(params, q)
  return adminGetJSON<AccessSummaryResponse>(`/api/admin/access/summary?${params}`)
}

export function fetchAccessLogs(q: AccessQueryParams | undefined, page: number, pageSize = 50) {
  const params = new URLSearchParams({
    page: String(page),
    page_size: String(pageSize),
  })
  appendAccessFilters(params, q)
  return adminGetJSON<AccessLogsResponse>(`/api/admin/access/logs?${params}`)
}
