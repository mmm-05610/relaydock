// 后端 API 封装：统一注入 Bearer（面板口令）、统一 401 处理。
// 所有字段名与 gateway 的 Go json 标签一一对应。

const TOKEN_KEY = 'relaydock.token'

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? ''
}

export function setToken(token: string) {
  if (token) localStorage.setItem(TOKEN_KEY, token)
  else localStorage.removeItem(TOKEN_KEY)
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  const token = getToken()
  if (token) headers.set('Authorization', `Bearer ${token}`)
  if (init?.body) headers.set('Content-Type', 'application/json')

  const res = await fetch(path, { ...init, headers })
  if (res.status === 401) {
    setToken('')
    window.dispatchEvent(new Event('relaydock:unauthorized'))
    throw new ApiError(401, '未登录或口令已变更')
  }
  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText)
    throw new ApiError(res.status, text || res.statusText)
  }
  return (await res.json()) as T
}

const post = <T>(path: string, body?: unknown) =>
  request<T>(path, { method: 'POST', body: body === undefined ? undefined : JSON.stringify(body) })
const put = <T>(path: string, body?: unknown) =>
  request<T>(path, { method: 'PUT', body: body === undefined ? undefined : JSON.stringify(body) })
const del = <T>(path: string) => request<T>(path, { method: 'DELETE' })
const get = <T>(path: string) => request<T>(path)

// ---- 类型（对齐 Go json 标签） ----

export interface VirtualKey {
  id: number
  key_hash: string
  name: string
  owner: string
  agent_type: string
  quota_limit: number
  quota_used: number
  enabled: boolean
  allowed_models: string
}

export interface UsageLog {
  id: number
  key_id: number
  model: string
  upstream_model: string
  protocol: string
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  cost: number
  latency_ms: number
  status: number
  error: string
  request_id: string
  unmetered: boolean
  channel_id: number
  account_id: number
  attempts: number
  created_at: string
}

export interface UsageStats {
  total_cost: number
  total_tokens: number
  total_requests: number
  by_model: { model: string; cost: number; requests: number; total_tokens: number; success_rate: number }[]
  by_key: { key_id: number; name: string; owner: string; cost: number; tokens: number; requests: number }[]
}

export interface DashboardStats {
  today_cost: number
  today_tokens: number
  today_requests: number
  success_rate: number
  avg_latency_ms: number
  cache_read_tokens: number
  cache_hit_rate: number
  recent_logs: UsageLog[]
}

export interface TimeseriesPoint {
  date: string
  cost: number
  tokens: number
  requests: number
}

export interface GroupedUsage {
  group: string
  cost: number
  input_tokens: number
  output_tokens: number
  tokens: number
  requests: number
  success_rate: number
  cache_read_tokens: number
  avg_latency_ms: number
}

export interface ModelRoute {
  upstream: string
  model: string
  usage: string
}

export interface ChannelModel {
  name: string
  provider: string
  routes: Record<string, ModelRoute>
  pricing: { input_per_m: number; output_per_m: number; cache_read_per_m: number; cache_write_per_m: number }
  enabled: boolean
}

export interface Channel {
  id: number
  provider: string
  name: string
  balance_type: string
  balance_url: string
  models_url: string
  enabled: boolean
  auth_mode: string
  key_configured: boolean
  key_prefix: string
  preset: Record<string, unknown>
  models: ChannelModel[]
}

export interface UpstreamAccount {
  id: number
  name: string
  enabled: boolean
  max_concurrency: number
  key_configured: boolean
  inflight: number
  state: 'healthy' | 'cooling' | 'disabled'
  cooling_until: string | null
  cooldown_cause: string
  success_count: number
  failure_count: number
  consecutive_failures: number
  last_status_code: number
  last_error: string
  last_used_at: string | null
  usage: {
    account_id: number
    requests: number
    cost: number
    tokens: number
    avg_attempts: number
    errors: number
  } | null
}

export interface AccountsView {
  summary: { total: number; healthy: number; cooling: number; disabled: number }
  items: UpstreamAccount[]
}

// ---- keys ----

export const api = {
  verify: () => get<Record<string, boolean>>('/api/settings/upstream'),

  listKeys: () => get<VirtualKey[]>('/api/keys'),
  createKey: (body: { name: string; owner: string; agent_type: string; quota: number; allowed_models: string }) =>
    post<{ key: string; note: string }>('/api/keys', body),
  revokeKey: (key_hash: string) => post('/api/keys/revoke', { key_hash }),
  getKey: (hash: string) =>
    get<{ key: VirtualKey; cost: number; tokens: number; requests: number; recent: UsageLog[] }>(`/api/keys/${hash}`),
  updateKey: (hash: string, body: { name: string; owner: string; agent_type: string; quota: number; allowed_models: string }) =>
    put(`/api/keys/${hash}`, body),
  rotateKey: (hash: string) => post<{ key: string; note: string }>(`/api/keys/${hash}/rotate`),
  updateQuota: (hash: string, quota: number) => post(`/api/keys/${hash}/quota`, { quota }),

  usage: () => get<UsageStats>('/api/usage'),
  dashboard: () => get<DashboardStats>('/api/dashboard'),
  timeseries: (params: { days?: number; from?: string; to?: string }) =>
    get<TimeseriesPoint[]>(`/api/usage/timeseries?${new URLSearchParams(clean(params))}`),
  grouped: (params: { by: string; days?: number; from?: string; to?: string }) =>
    get<GroupedUsage[]>(`/api/usage/grouped?${new URLSearchParams(clean(params))}`),
  logs: (params: { limit?: number; offset?: number; model?: string; status?: number }) =>
    get<UsageLog[]>(`/api/logs?${new URLSearchParams(clean(params))}`),

  channels: () => get<Channel[]>('/api/channels'),
  createChannel: (body: Partial<Channel>) => post('/api/channels', body),
  updateChannel: (provider: string, body: Partial<Channel>) => put(`/api/channels/${provider}`, body),
  deleteChannel: (provider: string) => del(`/api/channels/${provider}`),
  setChannelKey: (provider: string, key: string) => post(`/api/channels/${provider}/key`, { key }),
  testChannel: (provider: string, model?: string) => post<{ ok: boolean; error?: string; status?: number; latency_ms?: number }>(`/api/channels/${provider}/test`, model ? { model } : {}),
  channelBalance: (provider: string) => get<Record<string, unknown>>(`/api/channels/${provider}/balance`),
  remoteModels: (provider: string) => get<string[] | { error: string }>(`/api/channels/${provider}/remote-models`),
  createModel: (provider: string, body: Partial<ChannelModel>) => post(`/api/channels/${provider}/models`, body),
  updateModel: (provider: string, name: string, body: Partial<ChannelModel>) => put(`/api/channels/${provider}/models/${name}`, body),
  deleteModel: (provider: string, name: string) => del(`/api/channels/${provider}/models/${name}`),

  accounts: (provider: string, withStats?: boolean) =>
    get<AccountsView>(`/api/channels/${provider}/accounts${withStats ? '?stats=1' : ''}`),
  createAccount: (provider: string, body: { name: string; key: string; max_concurrency: number }) =>
    post<{ id: number; name: string }>(`/api/channels/${provider}/accounts`, body),
  importAccounts: (provider: string, items: { name?: string; key: string; max_concurrency?: number }[]) =>
    post<{ added: number; duplicated: number }>(`/api/channels/${provider}/accounts/import`, { items }),
  batchAccounts: (provider: string, action: 'enable' | 'disable' | 'delete' | 'recover', ids: number[]) =>
    post<{ affected: number[] }>(`/api/channels/${provider}/accounts/batch`, { action, ids }),
  updateAccount: (provider: string, id: number, body: { name?: string; max_concurrency?: number; enabled?: boolean; key?: string }) =>
    put(`/api/channels/${provider}/accounts/${id}`, body),
  deleteAccount: (provider: string, id: number) => del(`/api/channels/${provider}/accounts/${id}`),
  testAccount: (provider: string, id: number, model?: string) =>
    post<{ ok: boolean; error?: string; status?: number; latency_ms?: number }>(`/api/channels/${provider}/accounts/${id}/test`, model ? { model } : {}),
  recoverAccount: (provider: string, id: number, expectedCoolingUntil?: string | null) =>
    post<{ status: string }>(`/api/channels/${provider}/accounts/${id}/recover`, {
      expected_cooling_until: expectedCoolingUntil ?? undefined,
    }),

  upstreamStatus: () => get<Record<string, boolean>>('/api/settings/upstream'),
  updatePassword: (password: string) => post('/api/settings/password', { password }),
  upstreamBalance: () => get<Record<string, Record<string, unknown>>>('/api/upstream/balance'),
}

function clean<T extends object>(params: T): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '') out[k] = String(v)
  }
  return out
}
