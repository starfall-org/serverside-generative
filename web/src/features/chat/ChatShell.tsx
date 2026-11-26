import * as React from 'react'
import {
  AlertTriangle,
  ChevronDown,
  Menu,
  MessageSquare,
  Moon,
  MoreHorizontal,
  Pencil,
  Plus,
  Sun,
  Trash2,
} from 'lucide-react'
import { useConnection } from '../connection/ConnectionProvider'
import { useChat } from './ChatProvider'
import { usePreferences } from '../../app/preferences'
import { Sidebar } from './Sidebar'
import { MessageList } from './MessageList'
import { Composer } from './Composer'
import { ModelPicker } from './ModelPicker'
import { SettingsDialog } from './SettingsDialog'
import { Button } from '../../components/ui/button'
import { Input } from '../../components/ui/input'
import { Sheet, SheetContent, SheetTitle } from '../../components/ui/sheet'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogTitle,
} from '../../components/ui/alert-dialog'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '../../components/ui/dropdown-menu'
import { useToast } from '../../components/ui/toast'
import { errorMessageKey } from '../../i18n/errors'
import { normalizeError } from '../../lib/errors'

export function ChatShell() {
  const { t, preferences, setTheme } = usePreferences()
  const { toast } = useToast()
  const connection = useConnection()
  const chat = useChat()
  const [sidebarCollapsed, setSidebarCollapsed] = React.useState(false)
  const [mobileOpen, setMobileOpen] = React.useState(false)
  const [settingsOpen, setSettingsOpen] = React.useState(false)
  const [modelOpen, setModelOpen] = React.useState(false)
  const [rename, setRename] = React.useState<{ id: string; title: string } | null>(null)
  const [renameValue, setRenameValue] = React.useState('')
  const [deleteId, setDeleteId] = React.useState('')
  const [confirmDisconnect, setConfirmDisconnect] = React.useState(false)

  const activeSummary = chat.chats.find((item) => item.id === chat.activeId)
  const title = activeSummary?.title || chat.active.conversation?.title || t('chat.newChatTitle')
  const backgroundRuns = chat.chats.filter((item) => {
    const run = chat.getChat(item.id).run
    return run && ['submitting', 'streaming', 'cancelling'].includes(run.phase) && item.id !== chat.activeId
  }).length

  function beginRename(id: string, current: string) {
    setRename({ id, title: current })
    setRenameValue(current)
  }

  async function saveRename() {
    if (!rename) return
    try {
      await chat.renameChat(rename.id, renameValue)
      toast(t('chat.renamed'), 'success')
      setRename(null)
    } catch (raw) {
      const error = normalizeError(raw)
      if (error.code === 'revision_conflict') {
        setRename(null)
        return
      }
      toast(t(errorMessageKey(error)), 'error')
    }
  }

  async function createChat() {
    try {
      await chat.newChat()
    } catch (raw) {
      toast(t(errorMessageKey(normalizeError(raw))), 'error')
    }
  }

  async function exportChat(id: string, format: 'markdown' | 'json') {
    try {
      await chat.exportChat(id, format)
      toast(t('chat.exported'), 'success')
    } catch (raw) {
      toast(t(errorMessageKey(normalizeError(raw))), 'error')
    }
  }

  async function confirmDelete() {
    if (!deleteId) return
    try {
      await chat.deleteChat(deleteId)
      toast(t('chat.deleted'), 'success')
    } catch (raw) {
      toast(t(errorMessageKey(normalizeError(raw))), 'error')
    } finally {
      setDeleteId('')
    }
  }

  async function disconnect() {
    try {
      await connection.logout()
    } catch (raw) {
      toast(t(errorMessageKey(normalizeError(raw))), 'error')
    } finally {
      setConfirmDisconnect(false)
    }
  }

  const themeIcon =
    preferences.theme === 'dark' ? (
      <Moon size={16} />
    ) : preferences.theme === 'light' ? (
      <Sun size={16} />
    ) : (
      <Sun size={16} />
    )

  return (
    <div className={`app ${sidebarCollapsed ? 'sidebar-collapsed' : ''}`}>
      <aside className="sidebar">
        <Sidebar
          onOpenSettings={() => setSettingsOpen(true)}
          onOpenModel={() => setModelOpen(true)}
          onRename={beginRename}
          onDelete={setDeleteId}
          onDisconnect={() => setConfirmDisconnect(true)}
        />
      </aside>

      <main className="main">
        <header className="topbar">
          <div className="topbar-left">
            <button
              type="button"
              className="icon-button mobile-menu"
              onClick={() => setMobileOpen(true)}
              aria-label={t('sidebar.openMenu')}
            >
              <Menu size={18} />
            </button>
            <button
              type="button"
              className="icon-button collapse-button"
              onClick={() => setSidebarCollapsed((value) => !value)}
              aria-label={sidebarCollapsed ? t('sidebar.expand') : t('sidebar.collapse')}
            >
              <Menu size={18} />
            </button>
            <div className="topbar-title">
              <MessageSquare size={15} />
              <span>{title}</span>
              {backgroundRuns > 0 && (
                <span className="background-badge" title={t('chat.backgroundRun')}>
                  <span className="run-dot" />
                  {backgroundRuns}
                </span>
              )}
            </div>
            {chat.activeId && (
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <button type="button" className="icon-button" aria-label={t('sidebar.more')}>
                    <MoreHorizontal size={16} />
                  </button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="start">
                  <DropdownMenuItem onSelect={() => beginRename(chat.activeId, title)}>
                    <Pencil size={14} /> {t('sidebar.rename')}
                  </DropdownMenuItem>
                  <DropdownMenuItem onSelect={() => void exportChat(chat.activeId, 'markdown')}>
                    {t('sidebar.exportMarkdown')}
                  </DropdownMenuItem>
                  <DropdownMenuItem onSelect={() => void exportChat(chat.activeId, 'json')}>
                    {t('sidebar.exportJson')}
                  </DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem danger onSelect={() => setDeleteId(chat.activeId)}>
                    <Trash2 size={14} /> {t('sidebar.deleteChat')}
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            )}
          </div>

          <div className="topbar-right">
            <span className={`status ${connection.phase}`}>
              <i aria-hidden="true" />
              {connection.phase === 'connected'
                ? t('common.connected')
                : connection.phase === 'offline'
                  ? t('common.offline')
                  : t('common.reconnecting')}
            </span>
            <button type="button" className="model-pill" onClick={() => setModelOpen(true)}>
              <span className="model-dot" />
              <span>{chat.selection.modelId || t('chat.chooseModel')}</span>
              <ChevronDown size={13} />
            </button>
            <button
              type="button"
              className="icon-button"
              onClick={() => setTheme(preferences.theme === 'dark' ? 'light' : 'dark')}
              aria-label={t('settings.theme')}
            >
              {themeIcon}
            </button>
            <Button
              variant="outline"
              size="icon"
              onClick={() => void createChat()}
              aria-label={t('sidebar.newChat')}
            >
              <Plus size={16} />
            </Button>
          </div>
        </header>

        {(connection.phase === 'offline' || connection.phase === 'reconnecting') && (
          <div className="connection-banner" role="status">
            <AlertTriangle size={15} />
            <span>{t('error.network')}</span>
            <Button variant="ghost" size="sm" onClick={connection.retry}>
              {t('common.retry')}
            </Button>
          </div>
        )}
        {connection.configured === false && (
          <div className="connection-banner" role="status">
            <AlertTriangle size={15} />
            <span>{t('login.noPassword')}</span>
          </div>
        )}
        <MessageList chatId={chat.activeId} chat={chat.active} />
        <Composer onOpenModel={() => setModelOpen(true)} />
      </main>

      <Sheet open={mobileOpen} onOpenChange={setMobileOpen}>
        <SheetContent side="left" className="mobile-sidebar" aria-describedby={undefined}>
          <SheetTitle className="sr-only">{t('app.name')}</SheetTitle>
          <Sidebar
            onOpenSettings={() => {
              setMobileOpen(false)
              setSettingsOpen(true)
            }}
            onOpenModel={() => {
              setMobileOpen(false)
              setModelOpen(true)
            }}
            onRename={beginRename}
            onDelete={setDeleteId}
            onDisconnect={() => {
              setMobileOpen(false)
              setConfirmDisconnect(true)
            }}
            onNavigate={() => setMobileOpen(false)}
            onClose={() => setMobileOpen(false)}
          />
        </SheetContent>
      </Sheet>

      <ModelPicker open={modelOpen} onOpenChange={setModelOpen} />
      <SettingsDialog
        open={settingsOpen}
        onOpenChange={setSettingsOpen}
        onOpenModel={() => {
          setSettingsOpen(false)
          setModelOpen(true)
        }}
        onDisconnect={() => {
          setSettingsOpen(false)
          setConfirmDisconnect(true)
        }}
      />

      <AlertDialog open={Boolean(rename)} onOpenChange={(open) => !open && setRename(null)}>
        <AlertDialogContent>
          <AlertDialogTitle>{t('sidebar.rename')}</AlertDialogTitle>
          <Input
            value={renameValue}
            onChange={(event) => setRenameValue(event.target.value)}
            aria-label={t('chat.renamePrompt')}
            autoFocus
            onKeyDown={(event) => {
              if (event.key === 'Enter') void saveRename()
            }}
          />
          <div className="dialog-footer">
            <AlertDialogCancel>{t('common.cancel')}</AlertDialogCancel>
            <AlertDialogAction onClick={() => void saveRename()} disabled={!renameValue.trim()}>
              {t('common.save')}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={Boolean(deleteId)} onOpenChange={(open) => !open && setDeleteId('')}>
        <AlertDialogContent>
          <AlertDialogTitle>{t('chat.deleteConfirmTitle')}</AlertDialogTitle>
          <AlertDialogDescription>{t('chat.deleteConfirmBody')}</AlertDialogDescription>
          <div className="dialog-footer">
            <AlertDialogCancel>{t('common.cancel')}</AlertDialogCancel>
            <AlertDialogAction variant="danger" onClick={() => void confirmDelete()}>
              {t('common.delete')}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={confirmDisconnect} onOpenChange={setConfirmDisconnect}>
        <AlertDialogContent>
          <AlertDialogTitle>{t('login.logoutTitle')}</AlertDialogTitle>
          <AlertDialogDescription>{t('login.logoutBody')}</AlertDialogDescription>
          <div className="dialog-footer">
            <AlertDialogCancel>{t('common.cancel')}</AlertDialogCancel>
            <AlertDialogAction onClick={() => void disconnect()}>{t('login.logout')}</AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={Boolean(chat.conflict)} onOpenChange={(open) => !open && chat.dismissConflict()}>
        <AlertDialogContent>
          <AlertDialogTitle>{t('chat.conflictTitle')}</AlertDialogTitle>
          <AlertDialogDescription>{t('chat.conflictBody')}</AlertDialogDescription>
          <div className="dialog-footer">
            <AlertDialogCancel>{t('common.cancel')}</AlertDialogCancel>
            <AlertDialogAction onClick={() => void chat.resolveConflict()}>
              {t('common.reload')}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}
