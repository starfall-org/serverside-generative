import * as React from 'react'
import {
  CircleAlert,
  Globe2,
  KeyRound,
  Laptop,
  RefreshCw,
  Server,
  ShieldCheck,
  Terminal,
  Upload,
  Wrench,
} from 'lucide-react'
import { useConnection } from '../connection/ConnectionProvider'
import { useChat } from './ChatProvider'
import { usePreferences } from '../../app/preferences'
import { useToast } from '../../components/ui/toast'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '../../components/ui/dialog'
import { Button } from '../../components/ui/button'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../../components/ui/select'
import { Skeleton } from '../../components/ui/skeleton'
import { errorMessageKey } from '../../i18n/errors'
import { formatDate } from '../../lib/format'
import { normalizeError, type AppError } from '../../lib/errors'

type Tab = 'general' | 'data' | 'connection' | 'models' | 'mcp' | 'tools' | 'advanced'

interface MCPItem {
  id: string
  name: string
  protocol?: string
}
interface ToolItem {
  name: string
  execution_supported?: boolean
  configuration_supported?: boolean
}

export function SettingsDialog({
  open,
  onOpenChange,
  onOpenModel,
  onDisconnect,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onOpenModel: () => void
  onDisconnect: () => void
}) {
  const { t, preferences, setTheme, setLanguage, language, locale } = usePreferences()
  const { toast } = useToast()
  const connection = useConnection()
  const { catalog, catalogLoading, refreshCatalog, selection, importChat } = useChat()
  const [tab, setTab] = React.useState<Tab>('general')
  const fileRef = React.useRef<HTMLInputElement>(null)
  const [mcp, setMcp] = React.useState<MCPItem[] | null>(null)
  const [tools, setTools] = React.useState<ToolItem[] | null>(null)
  const [inventoryError, setInventoryError] = React.useState<AppError | null>(null)
  const [inventoryLoading, setInventoryLoading] = React.useState(false)

  React.useEffect(() => {
    if (!open || (tab !== 'mcp' && tab !== 'tools')) return
    if ((tab === 'mcp' && mcp) || (tab === 'tools' && tools)) return
    const controller = new AbortController()
    setInventoryLoading(true)
    setInventoryError(null)
    const op = tab === 'mcp' ? 'mcp.catalog' : 'tool.list'
    connection.client
      .rpc<unknown>(op, {}, '', { signal: controller.signal })
      .then((result) => {
        if (tab === 'mcp') setMcp(Array.isArray(result) ? (result as MCPItem[]) : [])
        else {
          const list =
            result && typeof result === 'object' && 'tools' in result
              ? (result as { tools: ToolItem[] }).tools
              : []
          setTools(Array.isArray(list) ? list : [])
        }
      })
      .catch((raw) => {
        if (!controller.signal.aborted) setInventoryError(normalizeError(raw))
      })
      .finally(() => {
        if (!controller.signal.aborted) setInventoryLoading(false)
      })
    return () => controller.abort()
  }, [open, tab, mcp, tools, connection.client])

  const protocol = connection.capabilities?.protocol
  const nativeTerminal = connection.capabilities?.native_terminal === true
  const nativeAgent = connection.capabilities?.native_agent === true

  async function importFile(file: File | undefined) {
    if (!file) return
    try {
      await importChat(file)
      toast(t('chat.imported'), 'success')
      onOpenChange(false)
    } catch (raw) {
      toast(t(errorMessageKey(normalizeError(raw))), 'error')
    } finally {
      if (fileRef.current) fileRef.current.value = ''
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="dialog-settings">
        <DialogHeader>
          <DialogTitle>{t('settings.title')}</DialogTitle>
          <DialogDescription>{t('settings.noSecrets')}</DialogDescription>
        </DialogHeader>

        <div className="settings-layout">
          <nav className="settings-nav" aria-label={t('settings.title')}>
            {(
              [
                ['general', 'settings.general'],
                ['data', 'settings.data'],
                ['connection', 'settings.connection'],
                ['models', 'settings.models'],
                ['mcp', 'settings.mcp'],
                ['tools', 'settings.tools'],
                ['advanced', 'settings.advanced'],
              ] as [Tab, Parameters<typeof t>[0]][]
            ).map(([id, label]) => (
              <button
                key={id}
                type="button"
                className={tab === id ? 'active' : ''}
                aria-current={tab === id ? 'page' : undefined}
                onClick={() => setTab(id)}
              >
                {t(label)}
              </button>
            ))}
          </nav>

          <div className="settings-panel">
            {tab === 'general' && (
              <div className="settings-section">
                <h3>{t('settings.appearance')}</h3>
                <div className="setting-row">
                  <div>
                    <strong>{t('settings.theme')}</strong>
                  </div>
                  <Select
                    value={preferences.theme}
                    onValueChange={(value) => setTheme(value as typeof preferences.theme)}
                  >
                    <SelectTrigger aria-label={t('settings.theme')} className="select-narrow">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="system">{t('settings.theme.system')}</SelectItem>
                      <SelectItem value="light">{t('settings.theme.light')}</SelectItem>
                      <SelectItem value="dark">{t('settings.theme.dark')}</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <div className="setting-row">
                  <div>
                    <strong>{t('settings.language')}</strong>
                  </div>
                  <Select value={language} onValueChange={(value) => setLanguage(value as typeof language)}>
                    <SelectTrigger aria-label={t('settings.language')} className="select-narrow">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="vi">{t('settings.language.vi')}</SelectItem>
                      <SelectItem value="en">{t('settings.language.en')}</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <div className="setting-row">
                  <div>
                    <strong>{t('settings.defaultModel')}</strong>
                    <p>{t('settings.defaultModelHint')}</p>
                    <code className="inline-code">
                      {selection.providerId ? `${selection.providerId} / ` : ''}
                      {selection.modelId || t('chat.noModel')}
                    </code>
                  </div>
                  <Button variant="outline" size="sm" onClick={onOpenModel}>
                    {t('common.apply')}
                  </Button>
                </div>
              </div>
            )}

            {tab === 'data' && (
              <div className="settings-section">
                <h3>{t('settings.dataTitle')}</h3>
                <p className="field-hint">{t('settings.dataHint')}</p>
                <div className="setting-row">
                  <div>
                    <strong>{t('settings.importChat')}</strong>
                    <p>{t('settings.importNote')}</p>
                  </div>
                  <Button variant="outline" size="sm" onClick={() => fileRef.current?.click()}>
                    <Upload size={14} /> {t('common.import')}
                  </Button>
                </div>
                <input
                  ref={fileRef}
                  type="file"
                  accept="application/json,.json"
                  hidden
                  onChange={(event) => void importFile(event.target.files?.[0])}
                />
              </div>
            )}

            {tab === 'connection' && (
              <div className="settings-section">
                <h3>{t('settings.diagnostics')}</h3>
                <dl className="diagnostics">
                  <div>
                    <dt>{t('settings.origin')}</dt>
                    <dd>{window.location.origin}</dd>
                  </div>
                  <div>
                    <dt>{t('settings.protocol')}</dt>
                    <dd>{protocol ?? t('common.unknown')}</dd>
                  </div>
                  <div>
                    <dt>{t('settings.transport')}</dt>
                    <dd>
                      {connection.transport === 'unknown'
                        ? t('common.unknown')
                        : connection.transport.toUpperCase()}
                    </dd>
                  </div>
                  <div>
                    <dt>{t('settings.lastConnected')}</dt>
                    <dd>
                      {connection.lastConnectedAt
                        ? formatDate(connection.lastConnectedAt, locale)
                        : t('common.none')}
                    </dd>
                  </div>
                  <div>
                    <dt>{t('settings.security')}</dt>
                    <dd>
                      {t(connection.phase === 'connected' ? 'common.connected' : 'common.reconnecting')}
                    </dd>
                  </div>
                </dl>
                <p className="field-hint">{t(connection.configured ? 'login.admin' : 'login.setup')}</p>
                {connection.configured && (
                  <Button variant="outline" size="sm" onClick={onDisconnect}>
                    {t('login.logout')}
                  </Button>
                )}
              </div>
            )}

            {tab === 'models' && (
              <div className="settings-section">
                <div className="section-head">
                  <h3>{t('settings.providerCatalog')}</h3>
                  <Button variant="ghost" size="sm" onClick={() => void refreshCatalog()}>
                    <RefreshCw size={13} /> {t('common.refresh')}
                  </Button>
                </div>
                {catalogLoading ? (
                  <Skeleton className="skeleton-block" />
                ) : !catalog || catalog.providers.length === 0 ? (
                  <p className="muted">{t('settings.emptyCatalog')}</p>
                ) : (
                  <>
                    <p className="field-hint">
                      {t('settings.catalogRevision', { revision: catalog.revision })}
                    </p>
                    <ul className="inventory-list">
                      {catalog.providers.map((provider) => (
                        <li key={provider.id}>
                          <div>
                            <strong>{provider.name || provider.id}</strong>
                            <small>
                              {provider.type} · {provider.models.length} models
                              {provider.key_selection_required ? ` · ${t('chat.keyRequired')}` : ''}
                            </small>
                          </div>
                          <span className="badge">{provider.id}</span>
                        </li>
                      ))}
                    </ul>
                  </>
                )}
                <p className="field-hint">{t('settings.managementUnavailable')}</p>
              </div>
            )}

            {(tab === 'mcp' || tab === 'tools') && (
              <div className="settings-section">
                <h3>{tab === 'mcp' ? t('settings.mcpInventory') : t('settings.toolsInventory')}</h3>
                {inventoryLoading ? (
                  <Skeleton className="skeleton-block" />
                ) : inventoryError ? (
                  <div className="banner banner-error">
                    <CircleAlert size={15} />
                    <span>{t(errorMessageKey(inventoryError))}</span>
                  </div>
                ) : tab === 'mcp' ? (
                  mcp && mcp.length > 0 ? (
                    <ul className="inventory-list">
                      {mcp.map((item) => (
                        <li key={item.id}>
                          <div>
                            <strong>{item.name || item.id}</strong>
                            <small>{item.protocol || ''}</small>
                          </div>
                          <span className="badge">{item.id}</span>
                        </li>
                      ))}
                    </ul>
                  ) : (
                    <p className="muted">{t('settings.emptyMcp')}</p>
                  )
                ) : tools && tools.length > 0 ? (
                  <ul className="inventory-list">
                    {tools.map((item) => (
                      <li key={item.name}>
                        <div>
                          <strong>{item.name}</strong>
                          <small>
                            {item.execution_supported
                              ? t('settings.nativeEnabled')
                              : t('settings.nativeDisabled')}
                          </small>
                        </div>
                        <Wrench size={14} />
                      </li>
                    ))}
                  </ul>
                ) : (
                  <p className="muted">{t('settings.emptyTools')}</p>
                )}
                <p className="field-hint">{t('settings.managementUnavailable')}</p>
              </div>
            )}

            {tab === 'advanced' && (
              <div className="settings-section">
                <h3>{t('settings.nativeFeatures')}</h3>
                <div className="feature-grid">
                  <div className="feature-card">
                    <Terminal size={16} />
                    <strong>{t('settings.terminal')}</strong>
                    <small>
                      {nativeTerminal ? t('settings.nativeEnabled') : t('settings.nativeDisabled')}
                    </small>
                  </div>
                  <div className="feature-card">
                    <Laptop size={16} />
                    <strong>{t('settings.agent')}</strong>
                    <small>{nativeAgent ? t('settings.nativeEnabled') : t('settings.nativeDisabled')}</small>
                  </div>
                </div>
                <p className="field-hint">{t('settings.nativeUnavailable')}</p>

                <h3>{t('settings.capabilities')}</h3>
                <dl className="diagnostics">
                  <div>
                    <dt>{t('settings.operations')}</dt>
                    <dd>{connection.capabilities?.operations.length ?? 0}</dd>
                  </div>
                  <div>
                    <dt>{t('settings.transports')}</dt>
                    <dd>{connection.capabilities?.transports.join(', ') || t('common.unknown')}</dd>
                  </div>
                </dl>

                <h3>{t('settings.secretManagementUnavailable')}</h3>
                <p className="field-hint">{t('settings.secretManagementHint')}</p>
                <div className="feature-grid">
                  <div className="feature-card">
                    <ShieldCheck size={16} />
                    <strong>{t('settings.noSecrets')}</strong>
                    <small>{t('login.runtimeMemory')}</small>
                  </div>
                </div>
                <div className="feature-grid">
                  <div className="feature-card">
                    <Globe2 size={16} />
                    <strong>{t('settings.origin')}</strong>
                    <small>{window.location.origin}</small>
                  </div>
                  <div className="feature-card">
                    <Server size={16} />
                    <strong>{t('settings.protocol')}</strong>
                    <small>{protocol ?? t('common.unknown')}</small>
                  </div>
                  <div className="feature-card">
                    <KeyRound size={16} />
                    <strong>{t('chat.temporary')}</strong>
                    <small>{t('chat.temporaryHint')}</small>
                  </div>
                </div>
              </div>
            )}
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}
