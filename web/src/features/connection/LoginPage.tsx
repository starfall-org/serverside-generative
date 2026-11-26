import * as React from 'react'
import { ArrowRight, CircleAlert, Lock } from 'lucide-react'
import { useConnection } from './ConnectionProvider'
import { usePreferences } from '../../app/preferences'
import { Button } from '../../components/ui/button'
import { Input } from '../../components/ui/input'
import { errorMessageKey } from '../../i18n/errors'

export function LoginPage() {
  const { t } = usePreferences()
  const { login, phase, error, configured, retry } = useConnection()
  const [password, setPassword] = React.useState('')
  const pending = phase === 'authenticating'

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    if (!password || pending) return
    const value = password
    setPassword('')
    await login(value)
  }

  return (
    <main className="pair-page">
      <div className="pair-aside">
        <div className="brand">
          <div className="brandmark" aria-hidden="true">
            <span>G</span>
          </div>
          <strong>{t('app.name')}</strong>
        </div>
        <div className="pair-hero">
          <h1>{t('app.tagline')}</h1>
        </div>
      </div>
      <div className="pair-main">
        <div className="login-card">
          <div className="login-icon" aria-hidden="true">
            <Lock size={22} />
          </div>
          <h2>{t('login.title')}</h2>
          {configured === false && !error && <p role="status">{t('common.restoring')}</p>}
          {error?.code === 'web_password_required' && (
            <div className="login-note">
              <p>{t('login.setup')}</p>
              <code>generative web password set</code>
            </div>
          )}
          {configured === true && (
            <form onSubmit={submit} className="pair-form">
              <label htmlFor="web-password">{t('login.password')}</label>
              <Input
                id="web-password"
                value={password}
                type="password"
                autoComplete="current-password"
                onChange={(event) => setPassword(event.target.value)}
                maxLength={512}
                autoFocus
                required
              />
              <Button type="submit" disabled={!password || pending}>
                {pending ? t('common.loading') : t('login.title')} <ArrowRight size={15} />
              </Button>
            </form>
          )}
          {phase === 'expired' && (
            <div className="warning" role="status">
              {t('common.expired')}
            </div>
          )}
          {error && (
            <div className="error" role="alert">
              <CircleAlert size={15} />
              <span>{t(error.kind === 'unauthorized' ? 'login.invalid' : errorMessageKey(error))}</span>
            </div>
          )}
          {phase === 'offline' && (
            <Button variant="outline" onClick={retry}>
              {t('common.retry')}
            </Button>
          )}
        </div>
      </div>
    </main>
  )
}
