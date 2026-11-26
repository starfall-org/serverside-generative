import * as React from 'react'
import { ApiClient } from '../../api/client'
import type { ConnectionPhase, PublicCapabilities, TransportKind } from '../../api/types'
import { AppError, normalizeError } from '../../lib/errors'

interface ConnectionContextValue {
  phase: ConnectionPhase
  configured: boolean | null
  authenticated: boolean
  error: AppError | null
  capabilities: PublicCapabilities | null
  transport: TransportKind
  lastConnectedAt: number | null
  client: ApiClient
  epoch: number
  login: (password: string) => Promise<void>
  logout: () => Promise<void>
  retry: () => void
  setTransport: (transport: TransportKind) => void
}

const ConnectionContext = React.createContext<ConnectionContextValue | null>(null)
const sessionEvent = 'generative.web.session-change'

export function ConnectionProvider({ children }: { children: React.ReactNode }) {
  const [control] = React.useState(() => new ApiClient(window.location.origin))
  const [client, setClient] = React.useState(control)
  const [configured, setConfigured] = React.useState<boolean | null>(null)
  const [phase, setPhase] = React.useState<ConnectionPhase>('restoring')
  const [capabilities, setCapabilities] = React.useState<PublicCapabilities | null>(null)
  const [error, setError] = React.useState<AppError | null>(null)
  const [transport, setTransport] = React.useState<TransportKind>('unknown')
  const [lastConnectedAt, setLastConnectedAt] = React.useState<number | null>(null)
  const [epoch, setEpoch] = React.useState(0)
  const current = React.useRef({ version: '', client: control })
  const check = React.useRef<AbortController | null>(null)
  const busy = React.useRef(false)
  const healthy = React.useRef(false)
  const retryRef = React.useRef<() => void>(() => {})

  const switchSession = React.useCallback(
    (version: string) => {
      if (current.current.version === version) return current.current.client
      if (current.current.client !== control) current.current.client.close()
      const next = version
        ? new ApiClient(window.location.origin, version, () => retryRef.current())
        : control
      current.current = { version, client: next }
      setClient(next)
      setCapabilities(null)
      setTransport('unknown')
      setEpoch((value) => value + 1)
      return next
    },
    [control],
  )

  const restore = React.useCallback(
    async (force = false) => {
      if (busy.current && !force) return
      check.current?.abort()
      const controller = new AbortController()
      check.current = controller
      try {
        const status = await control.status(controller.signal)
        if (controller.signal.aborted) return
        setConfigured(status.configured)
        let version = status.sessionVersion
        if (!status.authenticated && !status.configured) {
          version = await control.bootstrap(controller.signal)
          if (controller.signal.aborted) return
        }
        if (!version) {
          healthy.current = false
          setError((prior) =>
            status.configured && (prior?.kind === 'unauthorized' || prior?.kind === 'rate_limited')
              ? prior
              : null,
          )
          const expired = Boolean(current.current.version)
          switchSession('')
          setPhase((prior) =>
            expired ? 'expired' : prior === 'restoring' || prior === 'offline' ? 'login' : prior,
          )
          return
        }
        if (healthy.current && current.current.version === version) return
        const active = switchSession(version)
        const caps = await active.capabilities(controller.signal)
        if (controller.signal.aborted) return
        healthy.current = true
        setCapabilities(caps)
        setPhase(caps.protocol === 5 ? 'connected' : 'incompatible')
        setLastConnectedAt(Date.now())
        setError(null)
      } catch (raw) {
        if (controller.signal.aborted) return
        healthy.current = false
        setError(normalizeError(raw))
        setPhase((prior) =>
          prior === 'reconnecting' || !current.current.version ? 'offline' : 'reconnecting',
        )
      } finally {
        if (check.current === controller) check.current = null
      }
    },
    [control, switchSession],
  )

  const retry = React.useCallback(() => {
    if (!check.current) void restore()
  }, [restore])

  React.useEffect(() => {
    retryRef.current = retry
    function onStorage(event: StorageEvent) {
      if (event.key === sessionEvent) retry()
    }
    const interval = window.setInterval(retry, 5_000)
    window.addEventListener('online', retry)
    window.addEventListener('storage', onStorage)
    retry()
    return () => {
      check.current?.abort()
      check.current = null
      window.clearInterval(interval)
      window.removeEventListener('online', retry)
      window.removeEventListener('storage', onStorage)
    }
  }, [retry])

  function announceSessionChange() {
    try {
      window.localStorage.setItem(sessionEvent, crypto.randomUUID())
    } catch {
      // Status polling also observes changes when cross-tab storage is unavailable.
    }
  }

  async function login(password: string) {
    if (busy.current) return
    busy.current = true
    check.current?.abort()
    setPhase('authenticating')
    setError(null)
    try {
      await control.login(password)
      announceSessionChange()
      await restore(true)
    } catch (raw) {
      setError(normalizeError(raw))
      setPhase('login')
    } finally {
      busy.current = false
    }
  }

  async function logout() {
    if (busy.current) return
    busy.current = true
    check.current?.abort()
    try {
      await current.current.client.logout()
      switchSession('')
      setError(null)
      setPhase('login')
      announceSessionChange()
    } finally {
      busy.current = false
    }
  }

  return (
    <ConnectionContext.Provider
      value={{
        phase,
        configured,
        authenticated: client !== control,
        error,
        capabilities,
        transport,
        lastConnectedAt,
        client,
        epoch,
        login,
        logout,
        retry,
        setTransport,
      }}
    >
      {children}
    </ConnectionContext.Provider>
  )
}

export function useConnection(): ConnectionContextValue {
  const context = React.useContext(ConnectionContext)
  if (!context) throw new Error('useConnection must be used inside ConnectionProvider')
  return context
}
