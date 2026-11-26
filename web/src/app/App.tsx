import { Loader2, ShieldAlert } from 'lucide-react'
import { useConnection } from '../features/connection/ConnectionProvider'
import { ChatProvider } from '../features/chat/ChatProvider'
import { ChatShell } from '../features/chat/ChatShell'
import { LoginPage } from '../features/connection/LoginPage'
import { usePreferences } from './preferences'
import { Skeleton } from '../components/ui/skeleton'
import { ToastProvider } from '../components/ui/toast'

function Splash() {
  const { t } = usePreferences()
  return (
    <div className="splash" role="status" aria-label={t('common.restoring')}>
      <div className="brandmark large" aria-hidden="true">
        <span>G</span>
      </div>
      <div className="splash-lines">
        <Skeleton className="skeleton-title" />
        <Skeleton className="skeleton-line" />
      </div>
      <Loader2 size={18} className="spin" />
    </div>
  )
}

function IncompatiblePage() {
  const { t } = usePreferences()
  const { error, capabilities } = useConnection()
  return (
    <div className="status-page">
      <div className="status-icon danger">
        <ShieldAlert size={24} />
      </div>
      <h1>{t('common.incompatible')}</h1>
      <p>{t('error.incompatible', { protocol: capabilities?.protocol ?? '?' })}</p>
      {error && <code>{error.code || error.message}</code>}
      <a href="/">{t('common.retry')}</a>
    </div>
  )
}

export function App() {
  const connection = useConnection()

  if (connection.phase === 'restoring') return <Splash />
  if (connection.phase === 'incompatible') return <IncompatiblePage />

  if (connection.authenticated) {
    return (
      <ToastProvider key={connection.epoch}>
        <ChatProvider>
          <ChatShell />
        </ChatProvider>
      </ToastProvider>
    )
  }

  return <LoginPage />
}
