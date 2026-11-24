const TOKEN_KEY = 'multivless_token'

export class ApiError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.status = status
  }
}

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? ''
}

export function setToken(token: string) {
  if (token) localStorage.setItem(TOKEN_KEY, token)
  else localStorage.removeItem(TOKEN_KEY)
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const token = getToken()
  const res = await fetch(path, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(init?.headers ?? {}),
    },
  })
  if (!res.ok) {
    let message = res.statusText
    try {
      const body = await res.json()
      if (body?.error) message = body.error
    } catch {
      /* keep status text */
    }
    throw new ApiError(message, res.status)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

export type Counts = {
  total: number
  online: number
  degraded: number
  offline: number
  unknown: number
  disabled: number
  error: number
}

export type ConnSummary = {
  active: number
  total: number
  bytes_in: number
  bytes_out: number
}

export type Status = {
  version: string
  uptime_seconds: number
  engine_running: boolean
  servers: Counts
  balancer_strategy: string
  connections: ConnSummary
}

export type ServerSnapshot = {
  id: string
  name: string
  host: string
  port: number
  status: string
  enabled: boolean
  weight: number
  security: string
  network: string
  latency_ms: number
  active_connections: number
  throughput_score: number
  last_check: string
  last_error?: string
  bytes_in: number
  bytes_out: number
  source_id?: string
}

export type Subscription = {
  id: string
  name: string
  url_masked: string
  interval_seconds: number
  server_check_seconds: number
  enabled: boolean
  last_checked_at: string
  last_success_at: string
  last_error: string
  server_count: number
  created_at: string
}

export type UpdateResult = {
  subscription_id: string
  name: string
  server_count: number
  added: number
  removed: number
  updated: number
  not_modified: boolean
  error?: string
}

export type User = {
  id: string
  username: string
  enabled: boolean
  max_connections: number
  bypass_torrent: boolean
  created_at: string
  last_login_at: string
  bytes_in: number
  bytes_out: number
}

export type Connection = {
  id: string
  user: string
  target: string
  server_id: string
  server_name: string
  started_at: string
  ended_at?: string
  active: boolean
  duration_ms: number
  bytes_in: number
  bytes_out: number
}

export type ConnectionsResponse = {
  connections: Connection[]
  recent: Connection[]
}

export type LogEntry = {
  id: number
  time: string
  kind: 'connection_start' | 'connection_end' | 'dns'
  user?: string
  target?: string
  server?: string
  domain?: string
  ip?: string
  country?: string
  bytes_in?: number
  bytes_out?: number
  duration_ms?: number
}

export type Stats = {
  servers: Counts
  connections: ConnSummary
  subscriptions: number
  users: number
  uptime_seconds: number
}

export function formatBytes(value: number): string {
  if (!value) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let v = value
  let unit = 0
  while (v >= 1024 && unit < units.length - 1) {
    v /= 1024
    unit++
  }
  return `${v.toFixed(v >= 100 || unit === 0 ? 0 : 1)} ${units[unit]}`
}

export function formatDuration(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (d) return `${d}д ${h}ч`
  if (h) return `${h}ч ${m}м`
  if (m) return `${m}м`
  return `${s}с`
}

export function formatTime(value: string): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime()) || date.getFullYear() < 2000) return '—'
  return date.toLocaleString('ru-RU')
}

export function latency(value: number): string {
  return value > 0 ? `${value.toFixed(0)} мс` : '—'
}

// flag turns an ISO 3166 alpha-2 code into a regional-indicator emoji.
export function flag(code?: string): string {
  const value = (code ?? '').trim().toUpperCase()
  if (value.length !== 2 || !/^[A-Z]{2}$/.test(value)) return '🌐'
  return String.fromCodePoint(...[...value].map((c) => 0x1f1a5 + c.charCodeAt(0)))
}
