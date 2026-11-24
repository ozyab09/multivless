import { useCallback, useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'
import {
  ApiError,
  api,
  flag,
  formatBytes,
  formatDuration,
  formatTime,
  getToken,
  latency,
  setToken,
  type Connection,
  type ConnectionsResponse,
  type LogEntry,
  type ServerSnapshot,
  type Stats,
  type Status,
  type Subscription,
  type UpdateResult,
  type User,
} from './api'

type Tab = 'dashboard' | 'servers' | 'subscriptions' | 'users' | 'connections' | 'logs' | 'settings'

const TABS: { id: Tab; label: string }[] = [
  { id: 'dashboard', label: 'Обзор' },
  { id: 'servers', label: 'Серверы' },
  { id: 'subscriptions', label: 'Подписки' },
  { id: 'users', label: 'Пользователи' },
  { id: 'connections', label: 'Соединения' },
  { id: 'logs', label: 'Логи' },
  { id: 'settings', label: 'Настройки' },
]

const STRATEGIES = [
  ['adaptive', 'Adaptive (по умолчанию)'],
  ['round-robin', 'Round Robin'],
  ['random', 'Random'],
  ['least-latency', 'Least Latency'],
  ['least-connections', 'Least Connections'],
  ['weighted', 'Weighted'],
]

function errorText(err: unknown): string {
  if (err instanceof ApiError) return err.message
  if (err instanceof Error) return err.message
  return String(err)
}

export default function App() {
  const [tokenInput, setTokenInput] = useState(getToken())
  const [authed, setAuthed] = useState(false)
  const [authError, setAuthError] = useState('')
  const [tab, setTab] = useState<Tab>('dashboard')
  const [status, setStatus] = useState<Status | null>(null)
  const [stats, setStats] = useState<Stats | null>(null)
  const [servers, setServers] = useState<ServerSnapshot[]>([])
  const [subs, setSubs] = useState<Subscription[]>([])
  const [users, setUsers] = useState<User[]>([])
  const [conns, setConns] = useState<Connection[]>([])
  const [recent, setRecent] = useState<Connection[]>([])
  const [logs, setLogs] = useState<LogEntry[]>([])
  const [strategy, setStrategy] = useState('adaptive')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [subPrefill, setSubPrefill] = useState('')

  const loadAll = useCallback(async () => {
    try {
      const st = await api<Status>('/api/v1/status')
      const [sv, sb, us, cn, se, stat] = await Promise.all([
        api<{ servers: ServerSnapshot[] }>('/api/v1/servers'),
        api<{ subscriptions: Subscription[] }>('/api/v1/subscriptions'),
        api<{ users: User[] }>('/api/v1/users'),
        api<ConnectionsResponse>('/api/v1/connections'),
        api<{ balancer_strategy: string }>('/api/v1/settings'),
        api<Stats>('/api/v1/stats'),
      ])
      setStatus(st)
      setServers(sv.servers ?? [])
      setSubs(sb.subscriptions ?? [])
      setUsers(us.users ?? [])
      setConns(cn.connections ?? [])
      setRecent(cn.recent ?? [])
      setStrategy(se.balancer_strategy)
      setStats(stat)
      setAuthed(true)
      setAuthError('')
      setError('')
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setAuthed(false)
        setAuthError('Неверный токен или API недоступен')
      } else {
        setError(errorText(err))
      }
    }
  }, [])

  useEffect(() => {
    if (getToken()) void loadAll()
  }, [loadAll])

  const refreshConnections = useCallback(async () => {
    const cn = await api<ConnectionsResponse>('/api/v1/connections')
    setConns(cn.connections ?? [])
    setRecent(cn.recent ?? [])
  }, [])

  // The full status snapshot (including the server list) is refreshed every
  // 30 seconds; live connections and logs poll faster while their tab is open.
  useEffect(() => {
    if (!authed) return
    const tick = () => {
      if (document.visibilityState === 'visible') void loadAll()
    }
    const id = setInterval(tick, 30000)
    document.addEventListener('visibilitychange', tick)
    return () => {
      clearInterval(id)
      document.removeEventListener('visibilitychange', tick)
    }
  }, [authed, loadAll])

  useEffect(() => {
    if (!authed || (tab !== 'connections' && tab !== 'dashboard')) return
    const tick = () => {
      if (document.visibilityState !== 'visible') return
      void refreshConnections().catch(() => undefined)
    }
    tick()
    const id = setInterval(tick, 5000)
    document.addEventListener('visibilitychange', tick)
    return () => {
      clearInterval(id)
      document.removeEventListener('visibilitychange', tick)
    }
  }, [authed, tab, refreshConnections])

  useEffect(() => {
    if (!authed || tab !== 'logs') return
    let cancelled = false
    const tick = async () => {
      if (document.visibilityState !== 'visible') return
      try {
        const result = await api<{ logs: LogEntry[] }>('/api/v1/logs?limit=300')
        if (!cancelled) setLogs(result.logs ?? [])
      } catch {
        /* keep the previous log page on transient errors */
      }
    }
    const onVisible = () => {
      if (document.visibilityState === 'visible') void tick()
    }
    void tick()
    const id = setInterval(() => void tick(), 3000)
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      cancelled = true
      clearInterval(id)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [authed, tab])

  const act = async (fn: () => Promise<unknown>, message = 'Готово') => {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      const result = await fn()
      setNotice(typeof result === 'string' ? result : message)
      await loadAll()
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  const login = async (event: FormEvent) => {
    event.preventDefault()
    setToken(tokenInput.trim())
    setAuthError('')
    await loadAll()
  }

  const logout = () => {
    setToken('')
    setTokenInput('')
    setAuthed(false)
    setStatus(null)
  }

  if (!authed) {
    return (
      <div className="login-wrap">
        <form className="card login" onSubmit={login}>
          <h1>
            multi<span>vless</span>
          </h1>
          <p className="muted">Введите API-токен администратора (api.token в config.yaml).</p>
          <input
            type="password"
            placeholder="mvl_…"
            value={tokenInput}
            onChange={(e) => setTokenInput(e.target.value)}
            autoFocus
          />
          {authError && <div className="alert error">{authError}</div>}
          <button className="primary" type="submit">
            Войти
          </button>
        </form>
      </div>
    )
  }

  return (
    <div className="shell">
      <header>
        <div className="brand">
          multi<span>vless</span>
          <small>Multi-Exit VLESS Gateway</small>
        </div>
        <nav>
          {TABS.map((item) => (
            <button
              key={item.id}
              className={tab === item.id ? 'tab active' : 'tab'}
              onClick={() => setTab(item.id)}
            >
              {item.label}
            </button>
          ))}
        </nav>
        <div className="header-right">
          <span className={status?.engine_running ? 'dot ok' : 'dot bad'} />
          <span className="muted">sing-box</span>
          <button className="ghost" onClick={logout}>
            Выйти
          </button>
        </div>
      </header>

      {error && <div className="alert error">{error}</div>}
      {notice && <div className="alert ok">{notice}</div>}

      <main className={busy ? 'busy' : ''}>
        {tab === 'dashboard' && <Dashboard status={status} stats={stats} conns={conns} />}
        {tab === 'servers' && (
          <Servers
            servers={servers}
            busy={busy}
            act={act}
            onAddSubscription={(url) => {
              setSubPrefill(url)
              setTab('subscriptions')
            }}
          />
        )}
        {tab === 'subscriptions' && (
          <Subscriptions
            subs={subs}
            busy={busy}
            act={act}
            prefill={subPrefill}
            onPrefill={() => setSubPrefill('')}
          />
        )}
        {tab === 'users' && <Users users={users} busy={busy} act={act} />}
        {tab === 'connections' && (
          <Connections conns={conns} recent={recent} busy={busy} act={act} />
        )}
        {tab === 'logs' && <Logs logs={logs} />}
        {tab === 'settings' && (
          <Settings strategy={strategy} setStrategy={setStrategy} busy={busy} act={act} />
        )}
      </main>
    </div>
  )
}

function Card({ title, children, hint }: { title: string; children: ReactNode; hint?: string }) {
  return (
    <section className="card">
      <div className="card-head">
        <h2>{title}</h2>
        {hint && <span className="muted small">{hint}</span>}
      </div>
      {children}
    </section>
  )
}

function StatusBadge({ status }: { status: string }) {
  return <span className={'badge ' + status.toLowerCase()}>{status}</span>
}

function Dashboard({
  status,
  stats,
  conns,
}: {
  status: Status | null
  stats: Stats | null
  conns: Connection[]
}) {
  const counts = status?.servers
  const summary = status?.connections
  return (
    <>
      <div className="grid cards">
        <div className="card stat">
          <span className="label">Серверы онлайн</span>
          <strong>
            {(counts?.online ?? 0) + (counts?.degraded ?? 0)}
            <small> / {counts?.total ?? 0}</small>
          </strong>
          <span className="muted small">
            оффлайн: {counts?.offline ?? 0} · ошибок конфига: {counts?.error ?? 0} · выкл:{' '}
            {counts?.disabled ?? 0} · неизвестно: {counts?.unknown ?? 0}
          </span>
        </div>
        <div className="card stat">
          <span className="label">Активные соединения</span>
          <strong>{summary?.active ?? 0}</strong>
          <span className="muted small">всего с запуска: {summary?.total ?? 0}</span>
        </div>
        <div className="card stat">
          <span className="label">Трафик</span>
          <strong>{formatBytes((summary?.bytes_in ?? 0) + (summary?.bytes_out ?? 0))}</strong>
          <span className="muted small">
            ↓ {formatBytes(summary?.bytes_in ?? 0)} · ↑ {formatBytes(summary?.bytes_out ?? 0)}
          </span>
        </div>
        <div className="card stat">
          <span className="label">Uptime</span>
          <strong>{formatDuration(status?.uptime_seconds ?? 0)}</strong>
          <span className="muted small">
            подписки: {stats?.subscriptions ?? 0} · пользователи: {stats?.users ?? 0}
          </span>
        </div>
      </div>

      <TrafficChart />

      <Card title="Последние соединения" hint="обновляется каждые 5 секунд">
        <ConnectionTable conns={conns.slice(0, 8)} />
      </Card>
    </>
  )
}

type TrafficSample = { at: number; up: number; down: number }

function rateText(value: number): string {
  return `${formatBytes(value)}/с`
}

function TrafficChart() {
  const [samples, setSamples] = useState<TrafficSample[]>([])
  const [rate, setRate] = useState({ up: 0, down: 0 })
  const previous = useRef<{ in: number; out: number; at: number } | null>(null)

  // Poll every 3 seconds, but only while the dashboard tab is visible and the
  // browser tab is not in the background.
  useEffect(() => {
    let cancelled = false
    const tick = async () => {
      if (document.visibilityState !== 'visible') return
      try {
        const stats = await api<Stats>('/api/v1/stats')
        if (cancelled) return
        const now = Date.now()
        const bytesIn = stats.connections.bytes_in
        const bytesOut = stats.connections.bytes_out
        let down = 0
        let up = 0
        const prev = previous.current
        if (prev) {
          const seconds = (now - prev.at) / 1000
          if (seconds > 0) {
            down = Math.max(0, (bytesIn - prev.in) / seconds)
            up = Math.max(0, (bytesOut - prev.out) / seconds)
          }
        }
        previous.current = { in: bytesIn, out: bytesOut, at: now }
        setRate({ up, down })
        setSamples((list) => [...list, { at: now, up, down }].slice(-40))
      } catch {
        /* keep the chart on transient API errors */
      }
    }
    void tick()
    const id = setInterval(() => void tick(), 3000)
    const onVisible = () => {
      if (document.visibilityState === 'visible') {
        previous.current = null
        void tick()
      }
    }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      cancelled = true
      clearInterval(id)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [])

  const width = 640
  const height = 150
  const max = Math.max(1024, ...samples.map((s) => Math.max(s.up, s.down)))
  const points = (key: 'up' | 'down') =>
    samples
      .map((sample, index) => {
        const x = samples.length > 1 ? (index / (samples.length - 1)) * width : 0
        const y = height - (sample[key] / max) * (height - 10)
        return `${x.toFixed(1)},${y.toFixed(1)}`
      })
      .join(' ')

  return (
    <Card title="Проходящий трафик" hint="каждые 3 секунды, только при открытой вкладке">
      <div className="chart-legend">
        <span className="chart-down">↓ {rateText(rate.down)}</span>
        <span className="chart-up">↑ {rateText(rate.up)}</span>
        <span className="muted small">максимум: {rateText(max)}</span>
      </div>
      <svg className="chart" viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" role="img"
        aria-label="График скорости проходящего трафика">
        {[0.25, 0.5, 0.75].map((part) => (
          <line key={part} x1="0" x2={width} y1={height * part} y2={height * part} className="chart-grid" />
        ))}
        <polyline className="chart-line down" points={points('down')} />
        <polyline className="chart-line up" points={points('up')} />
      </svg>
      <p className="muted small">
        ↓ входящий (от цели) · ↑ исходящий (к цели). История — 40 замеров.{' '}
        {samples.length < 2 ? 'Ждём данные…' : ''}
      </p>
    </Card>
  )
}

function Servers({
  servers,
  busy,
  act,
  onAddSubscription,
}: {
  servers: ServerSnapshot[]
  busy: boolean
  act: (fn: () => Promise<unknown>, message?: string) => Promise<void>
  onAddSubscription: (url: string) => void
}) {
  const [uri, setUri] = useState('')
  const isSubscription = /^https?:\/\//i.test(uri.trim())
  const add = async (event: FormEvent) => {
    event.preventDefault()
    if (!uri.trim()) return
    if (isSubscription) {
      onAddSubscription(uri.trim())
      setUri('')
      return
    }
    await act(
      async () => {
        await api('/api/v1/servers', { method: 'POST', body: JSON.stringify({ uri: uri.trim() }) })
        setUri('')
      },
      'Сервер добавлен',
    )
  }

  return (
    <>
      <Card title="Добавить VLESS-ссылку" hint="vless://UUID@host:port?...#name">
        <form className="row" onSubmit={add}>
          <input
            className="grow"
            placeholder="vless://… (подписку добавляйте на вкладке «Подписки»)"
            value={uri}
            onChange={(e) => setUri(e.target.value)}
          />
          <button className="primary" disabled={busy}>
            {isSubscription ? 'Добавить как подписку' : 'Добавить'}
          </button>
        </form>
        {isSubscription && (
          <p className="muted small">
            Это http(s)-ссылка подписки, а не одиночный сервер. Нажмите «Добавить как
            подписку» — откроется вкладка «Подписки» с заполненным URL.
          </p>
        )}
      </Card>

      <Card title="Серверы" hint={`${servers.length} шт.`}>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Название</th>
                <th>Адрес</th>
                <th>Транспорт</th>
                <th>Статус</th>
                <th>Задержка</th>
                <th>Соед.</th>
                <th>Трафик</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {servers.map((srv) => (
                <tr key={srv.id}>
                  <td>
                    <div className="name">{srv.name}</div>
                    <div className="muted small">{srv.source_id ? 'подписка' : 'вручную'}</div>
                  </td>
                  <td className="mono">
                    {srv.host}:{srv.port}
                  </td>
                  <td>
                    {srv.security}/{srv.network}
                  </td>
                  <td>
                    <StatusBadge status={srv.status} />
                    {srv.last_error && (
                      <div className="muted small" title={srv.last_error}>
                        {srv.last_error.slice(0, 48)}
                      </div>
                    )}
                  </td>
                  <td>{latency(srv.latency_ms)}</td>
                  <td>{srv.active_connections}</td>
                  <td className="mono small">
                    ↓{formatBytes(srv.bytes_in)} ↑{formatBytes(srv.bytes_out)}
                  </td>
                  <td className="actions">
                    <button
                      className="ghost"
                      disabled={busy}
                      onClick={() =>
                        void act(
                          () =>
                            api(`/api/v1/servers/${srv.id}/check`, { method: 'POST' }),
                          'Проверка выполнена',
                        )
                      }
                    >
                      Проверить
                    </button>
                    <button
                      className="ghost"
                      disabled={busy}
                      onClick={() =>
                        void act(
                          () =>
                            api(`/api/v1/servers/${srv.id}`, {
                              method: 'PATCH',
                              body: JSON.stringify({ enabled: !srv.enabled }),
                            }),
                          srv.enabled ? 'Сервер отключён' : 'Сервер включён',
                        )
                      }
                    >
                      {srv.enabled ? 'Отключить' : 'Включить'}
                    </button>
                    <button
                      className="danger"
                      disabled={busy}
                      onClick={() => {
                        if (!window.confirm(`Удалить ${srv.name}?`)) return
                        void act(
                          () => api(`/api/v1/servers/${srv.id}`, { method: 'DELETE' }),
                          'Сервер удалён',
                        )
                      }}
                    >
                      Удалить
                    </button>
                  </td>
                </tr>
              ))}
              {servers.length === 0 && (
                <tr>
                  <td colSpan={8} className="empty">
                    Серверов пока нет. Добавьте подписку или ссылку.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </Card>
      <p className="muted small">
        Добавление и удаление серверов перезапускает sing-box: активные соединения
        прерываются, новые подхватываются автоматически.
      </p>
    </>
  )
}

function Subscriptions({
  subs,
  busy,
  act,
  prefill,
  onPrefill,
}: {
  subs: Subscription[]
  busy: boolean
  act: (fn: () => Promise<unknown>, message?: string) => Promise<void>
  prefill: string
  onPrefill: () => void
}) {
  const [name, setName] = useState('')
  const [url, setUrl] = useState('')
  const [hours, setHours] = useState('6')
  const [checkSeconds, setCheckSeconds] = useState('30')
  const [editingId, setEditingId] = useState('')
  const [editName, setEditName] = useState('')
  const [editUrl, setEditUrl] = useState('')
  const [editHours, setEditHours] = useState('6')
  const [editCheckSeconds, setEditCheckSeconds] = useState('30')

  const startEditing = (sub: Subscription) => {
    setEditingId(sub.id)
    setEditName(sub.name)
    setEditUrl('')
    setEditHours((sub.interval_seconds / 3600).toFixed(2).replace(/\.?0+$/, ''))
    setEditCheckSeconds(String(sub.server_check_seconds || 30))
  }

  const saveEditing = async (event: FormEvent) => {
    event.preventDefault()
    if (!editingId || !editName.trim()) return
    await act(async () => {
      const body: Record<string, unknown> = {
        name: editName.trim(),
        interval_seconds: Math.max(10, Math.round(Number(editHours || '6') * 3600)),
        server_check_seconds: Math.max(5, Math.round(Number(editCheckSeconds || '30'))),
      }
      // The current URL is masked in the UI; only send it when replaced.
      if (editUrl.trim()) body.url = editUrl.trim()
      await api(`/api/v1/subscriptions/${editingId}`, {
        method: 'PATCH',
        body: JSON.stringify(body),
      })
      setEditingId('')
    }, 'Подписка обновлена')
  }

  useEffect(() => {
    if (!prefill) return
    setUrl(prefill)
    try {
      setName(new URL(prefill).hostname)
    } catch {
      /* keep the current name */
    }
    onPrefill()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [prefill])

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (!name.trim() || !url.trim()) return
    await act(async () => {
      const result = await api<{ update: UpdateResult }>('/api/v1/subscriptions', {
        method: 'POST',
        body: JSON.stringify({
          name: name.trim(),
          url: url.trim(),
          interval_seconds: Math.max(10, Math.round(Number(hours || '6') * 3600)),
          server_check_seconds: Math.max(5, Math.round(Number(checkSeconds || '30'))),
        }),
      })
      setName('')
      setUrl('')
      return result.update
        ? `Подписка добавлена: серверов ${result.update.server_count}, новых ${result.update.added}`
        : 'Подписка добавлена'
    })
  }

  return (
    <>
      <Card title="Добавить подписку" hint="HTTP(S) URL с vless-ссылками или base64">
        <form className="row wrap" onSubmit={submit}>
          <input
            placeholder="Название"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <input
            className="grow"
            placeholder="https://example.com/sub/…"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
          />
          <input
            className="narrow"
            type="number"
            min="1"
            title="Интервал обновления подписки, часы"
            value={hours}
            onChange={(e) => setHours(e.target.value)}
          />
          <input
            className="narrow"
            type="number"
            min="5"
            title="Проверка серверов, секунды"
            value={checkSeconds}
            onChange={(e) => setCheckSeconds(e.target.value)}
          />
          <button className="primary" disabled={busy}>
            Добавить и загрузить
          </button>
        </form>
        <p className="muted small">
          Интервал подписки — часы; проверка серверов — секунды (по умолчанию 30).
        </p>
      </Card>

      {editingId && (
        <Card title={`Изменить подписку «${editName || editingId}»`}>
          <form className="row wrap" onSubmit={saveEditing}>
            <input
              placeholder="Название"
              value={editName}
              onChange={(e) => setEditName(e.target.value)}
            />
            <input
              className="grow"
              placeholder="Новый URL (оставьте пустым, чтобы не менять)"
              value={editUrl}
              onChange={(e) => setEditUrl(e.target.value)}
            />
            <input
              className="narrow"
              type="number"
              min="0.01"
              step="0.5"
              title="Интервал обновления подписки, часы"
              value={editHours}
              onChange={(e) => setEditHours(e.target.value)}
            />
            <input
              className="narrow"
              type="number"
              min="5"
              title="Проверка серверов, секунды"
              value={editCheckSeconds}
              onChange={(e) => setEditCheckSeconds(e.target.value)}
            />
            <button className="primary" disabled={busy}>
              Сохранить
            </button>
            <button type="button" className="ghost" onClick={() => setEditingId('')}>
              Отмена
            </button>
          </form>
        </Card>
      )}

      <Card title="Подписки" hint={`${subs.length} шт.`}>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Название</th>
                <th>URL</th>
                <th>Серверов</th>
                <th>Интервал</th>
                <th>Проверка серверов</th>
                <th>Обновлено</th>
                <th>Статус</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {subs.map((sub) => (
                <tr key={sub.id}>
                  <td>
                    <div className="name">{sub.name}</div>
                    <div className="muted small">{sub.enabled ? 'активна' : 'выключена'}</div>
                  </td>
                  <td className="mono small">{sub.url_masked}</td>
                  <td>{sub.server_count}</td>
                  <td>{Math.round(sub.interval_seconds / 3600)} ч</td>
                  <td>{sub.server_check_seconds || 30} с</td>
                  <td className="small">{formatTime(sub.last_checked_at)}</td>
                  <td>
                    {sub.last_error ? (
                      <span className="badge offline" title={sub.last_error}>
                        ошибка
                      </span>
                    ) : (
                      <span className="badge online">ok</span>
                    )}
                    {sub.last_error && (
                      <div className="muted small" title={sub.last_error}>
                        {sub.last_error.slice(0, 48)}
                      </div>
                    )}
                  </td>
                  <td className="actions">
                    <button className="ghost" disabled={busy} onClick={() => startEditing(sub)}>
                      Изменить
                    </button>
                    <button
                      className="ghost"
                      disabled={busy}
                      onClick={() =>
                        void act(async () => {
                          const result = await api<UpdateResult>(
                            `/api/v1/subscriptions/${sub.id}/update`,
                            { method: 'POST' },
                          )
                          return result.not_modified
                            ? 'Подписка не изменилась (304)'
                            : `Обновлено: ${result.server_count} серверов (+${result.added} / −${result.removed})`
                        })
                      }
                    >
                      Обновить
                    </button>
                    <button
                      className="ghost"
                      disabled={busy}
                      onClick={() =>
                        void act(
                          () =>
                            api(`/api/v1/subscriptions/${sub.id}`, {
                              method: 'PATCH',
                              body: JSON.stringify({ enabled: !sub.enabled }),
                            }),
                          sub.enabled ? 'Подписка выключена' : 'Подписка включена',
                        )
                      }
                    >
                      {sub.enabled ? 'Выключить' : 'Включить'}
                    </button>
                    <button
                      className="danger"
                      disabled={busy}
                      onClick={() => {
                        if (!window.confirm(`Удалить подписку «${sub.name}» и её серверы?`)) return
                        void act(
                          () => api(`/api/v1/subscriptions/${sub.id}`, { method: 'DELETE' }),
                          'Подписка удалена',
                        )
                      }}
                    >
                      Удалить
                    </button>
                  </td>
                </tr>
              ))}
              {subs.length === 0 && (
                <tr>
                  <td colSpan={8} className="empty">
                    Подписок пока нет.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </Card>
    </>
  )
}

function Users({
  users,
  busy,
  act,
}: {
  users: User[]
  busy: boolean
  act: (fn: () => Promise<unknown>, message?: string) => Promise<void>
}) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [limit, setLimit] = useState('0')
  const [bypass, setBypass] = useState(false)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (!username.trim() || !password) return
    await act(async () => {
      await api('/api/v1/users', {
        method: 'POST',
        body: JSON.stringify({
          username: username.trim(),
          password,
          max_connections: Math.max(0, Number(limit || '0')),
          bypass_torrent: bypass,
        }),
      })
      setUsername('')
      setPassword('')
      setLimit('0')
      setBypass(false)
    }, 'Пользователь создан')
  }

  return (
    <>
      <Card title="Новый SOCKS5-пользователь">
        <form className="row wrap" onSubmit={submit}>
          <input placeholder="Логин" value={username} onChange={(e) => setUsername(e.target.value)} />
          <input
            placeholder="Пароль"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
          <input
            className="narrow"
            type="number"
            min="0"
            title="Лимит соединений (0 = без лимита)"
            value={limit}
            onChange={(e) => setLimit(e.target.value)}
          />
          <label className="muted small" title="BitTorrent-трафик этого пользователя пойдёт напрямую, мимо VLESS">
            <input
              type="checkbox"
              checked={bypass}
              onChange={(e) => setBypass(e.target.checked)}
            />{' '}
            torrent напрямую
          </label>
          <button className="primary" disabled={busy}>
            Создать
          </button>
        </form>
        <p className="muted small">Пароли хранятся как Argon2id-хеш в SQLite.</p>
      </Card>

      <Card title="Пользователи" hint={`${users.length} шт.`}>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Логин</th>
                <th>Лимит</th>
                <th>Torrent</th>
                <th>Трафик</th>
                <th>Вход</th>
                <th>Статус</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {users.map((user) => (
                <tr key={user.id}>
                  <td className="name">{user.username}</td>
                  <td>{user.max_connections || '∞'}</td>
                  <td>
                    <label
                      className="muted small"
                      title="Отправлять BitTorrent напрямую, мимо VLESS-серверов"
                    >
                      <input
                        type="checkbox"
                        checked={user.bypass_torrent}
                        disabled={busy}
                        onChange={() =>
                          void act(
                            () =>
                              api(`/api/v1/users/${user.id}`, {
                                method: 'PATCH',
                                body: JSON.stringify({ bypass_torrent: !user.bypass_torrent }),
                              }),
                            user.bypass_torrent
                              ? 'Torrent снова идёт через VLESS'
                              : 'Torrent идёт напрямую',
                          )
                        }
                      />{' '}
                      {user.bypass_torrent ? 'напрямую' : 'через VLESS'}
                    </label>
                  </td>
                  <td className="mono small">
                    ↓{formatBytes(user.bytes_in)} ↑{formatBytes(user.bytes_out)}
                  </td>
                  <td className="small">{formatTime(user.last_login_at)}</td>
                  <td>
                    <span className={'badge ' + (user.enabled ? 'online' : 'disabled')}>
                      {user.enabled ? 'активен' : 'выключен'}
                    </span>
                  </td>
                  <td className="actions">
                    <button
                      className="ghost"
                      disabled={busy}
                      onClick={() => {
                        const value = window.prompt(`Новый пароль для ${user.username}`)
                        if (!value) return
                        void act(
                          () =>
                            api(`/api/v1/users/${user.id}`, {
                              method: 'PATCH',
                              body: JSON.stringify({ password: value }),
                            }),
                          'Пароль обновлён',
                        )
                      }}
                    >
                      Пароль
                    </button>
                    <button
                      className="ghost"
                      disabled={busy}
                      onClick={() =>
                        void act(
                          () =>
                            api(`/api/v1/users/${user.id}`, {
                              method: 'PATCH',
                              body: JSON.stringify({ enabled: !user.enabled }),
                            }),
                          user.enabled ? 'Пользователь отключён' : 'Пользователь включён',
                        )
                      }
                    >
                      {user.enabled ? 'Отключить' : 'Включить'}
                    </button>
                    <button
                      className="danger"
                      disabled={busy}
                      onClick={() => {
                        if (!window.confirm(`Удалить ${user.username}?`)) return
                        void act(
                          () => api(`/api/v1/users/${user.id}`, { method: 'DELETE' }),
                          'Пользователь удалён',
                        )
                      }}
                    >
                      Удалить
                    </button>
                  </td>
                </tr>
              ))}
              {users.length === 0 && (
                <tr>
                  <td colSpan={7} className="empty">
                    Пользователей пока нет.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </Card>
    </>
  )
}

function Connections({
  conns,
  recent,
  busy,
  act,
}: {
  conns: Connection[]
  recent: Connection[]
  busy: boolean
  act: (fn: () => Promise<unknown>, message?: string) => Promise<void>
}) {
  return (
    <>
      <Card title="Активные соединения" hint={`${conns.length} шт.`}>
        <ConnectionTable conns={conns} busy={busy} act={act} />
      </Card>
      <Card title="Недавние соединения" hint="история за последние 60 секунд">
        <ConnectionTable conns={recent} empty="Закрытых соединений за последнюю минуту нет." />
      </Card>
    </>
  )
}

function Logs({ logs }: { logs: LogEntry[] }) {
  return (
    <Card title="Логи" hint="соединения и DNS-запросы · обновление каждые 3 секунды">
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Время</th>
              <th>Тип</th>
              <th>Пользователь</th>
              <th>Цель</th>
              <th>IP</th>
              <th>Страна</th>
              <th>Сервер</th>
              <th>Детали</th>
            </tr>
          </thead>
          <tbody>
            {logs.map((entry) => {
              const kind =
                entry.kind === 'dns'
                  ? { label: 'DNS', css: 'unknown' }
                  : entry.kind === 'connection_end'
                    ? { label: 'конец', css: 'disabled' }
                    : { label: 'старт', css: 'online' }
              let details = '—'
              if (entry.kind === 'connection_end') {
                details = `${formatDuration((entry.duration_ms ?? 0) / 1000)} · ↓${formatBytes(
                  entry.bytes_in ?? 0,
                )} ↑${formatBytes(entry.bytes_out ?? 0)}`
              } else if (entry.kind === 'connection_start') {
                details = 'соединение открыто'
              } else {
                details = 'DNS-запрос'
              }
              return (
                <tr key={entry.id}>
                  <td className="small">{formatTime(entry.time)}</td>
                  <td>
                    <span className={'badge ' + kind.css}>{kind.label}</span>
                  </td>
                  <td>{entry.user || '—'}</td>
                  <td className="mono small">{entry.kind === 'dns' ? entry.domain : entry.target}</td>
                  <td className="mono small">{entry.ip || '—'}</td>
                  <td title={entry.country || 'страна неизвестна'}>
                    {entry.country ? `${flag(entry.country)} ${entry.country}` : '—'}
                  </td>
                  <td>{entry.server || '—'}</td>
                  <td className="small">{details}</td>
                </tr>
              )
            })}
            {logs.length === 0 && (
              <tr>
                <td colSpan={8} className="empty">
                  Событий пока нет. Откройте соединение через SOCKS5 — здесь появятся
                  старт, завершение и DNS-запросы.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </Card>
  )
}

function ConnectionTable({
  conns,
  busy,
  act,
  empty = 'Активных соединений нет.',
}: {
  conns: Connection[]
  busy?: boolean
  act?: (fn: () => Promise<unknown>, message?: string) => Promise<void>
  empty?: string
}) {
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>ID</th>
            <th>Пользователь</th>
            <th>Цель</th>
            <th>Сервер</th>
            <th>Длительность</th>
            <th>Трафик</th>
            <th>Статус</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {conns.map((conn) => (
            <tr key={conn.id}>
              <td className="mono small">{conn.id}</td>
              <td>{conn.user || '—'}</td>
              <td className="mono small">{conn.target}</td>
              <td>{conn.server_name}</td>
              <td>{formatDuration(conn.duration_ms / 1000)}</td>
              <td className="mono small">
                ↓{formatBytes(conn.bytes_in)} ↑{formatBytes(conn.bytes_out)}
              </td>
              <td className="small">
                {conn.active ? (
                  <span className="badge online">активно</span>
                ) : (
                  <>
                    <span className="badge disabled">закрыто</span>
                    {conn.ended_at && (
                      <div className="muted small">{formatTime(conn.ended_at)}</div>
                    )}
                  </>
                )}
              </td>
              <td className="actions">
                {act && (
                  <button
                    className="danger"
                    disabled={busy}
                    onClick={() =>
                      void act(
                        () => api(`/api/v1/connections/${conn.id}`, { method: 'DELETE' }),
                        'Соединение закрыто',
                      )
                    }
                  >
                    Закрыть
                  </button>
                )}
              </td>
            </tr>
          ))}
          {conns.length === 0 && (
            <tr>
              <td colSpan={8} className="empty">
                {empty}
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}

function Settings({
  strategy,
  setStrategy,
  busy,
  act,
}: {
  strategy: string
  setStrategy: (value: string) => void
  busy: boolean
  act: (fn: () => Promise<unknown>, message?: string) => Promise<void>
}) {
  return (
    <Card title="Балансировщик">
      <div className="row wrap">
        <select value={strategy} onChange={(e) => setStrategy(e.target.value)}>
          {STRATEGIES.map(([value, label]) => (
            <option key={value} value={value}>
              {label}
            </option>
          ))}
        </select>
        <button
          className="primary"
          disabled={busy}
          onClick={() =>
            void act(
              () =>
                api('/api/v1/settings', {
                  method: 'PATCH',
                  body: JSON.stringify({ balancer_strategy: strategy }),
                }),
              'Стратегия сохранена',
            )
          }
        >
          Сохранить
        </button>
      </div>
      <p className="muted small">
        Стратегия применяется к новым соединениям; текущие соединения сохраняют
        выбранный сервер.
      </p>
    </Card>
  )
}
