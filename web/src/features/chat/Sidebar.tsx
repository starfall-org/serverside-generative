import { useConnection } from '../connection/ConnectionProvider'
import * as React from 'react'
import {
  ChevronDown,
  Download,
  LogOut,
  MessageSquare,
  MoreHorizontal,
  Pencil,
  Plus,
  Search,
  Settings2,
  Trash2,
  X,
} from 'lucide-react'
import { useChat } from './ChatProvider'
import { usePreferences } from '../../app/preferences'
import { Button } from '../../components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '../../components/ui/dropdown-menu'
import { errorMessageKey } from '../../i18n/errors'
import { normalizeError } from '../../lib/errors'
import { useToast } from '../../components/ui/toast'
import { cn } from '../../components/ui/utils'

interface SidebarProps {
  onOpenSettings: () => void
  onOpenModel: () => void
  onRename: (id: string, title: string) => void
  onDelete: (id: string) => void
  onDisconnect: () => void
  onNavigate?: () => void
  onClose?: () => void
}

export function Sidebar({
  onOpenSettings,
  onOpenModel,
  onRename,
  onDelete,
  onDisconnect,
  onNavigate,
  onClose,
}: SidebarProps) {
  const { configured } = useConnection()
  const { t } = usePreferences()
  const { toast } = useToast()
  const { chats, activeId, openChat, newChat, loadMoreChats, nextCursor, listLoading, exportChat, getChat } =
    useChat()
  const [query, setQuery] = React.useState('')

  const filtered = React.useMemo(() => {
    const needle = query.trim().toLowerCase()
    if (!needle) return chats
    return chats.filter(
      (chat) => (chat.title || '').toLowerCase().includes(needle) || chat.id.toLowerCase().includes(needle),
    )
  }, [chats, query])

  async function choose(id: string) {
    try {
      await openChat(id)
      onNavigate?.()
    } catch (raw) {
      toast(t(errorMessageKey(normalizeError(raw))), 'error')
    }
  }

  async function create() {
    try {
      await newChat()
      onNavigate?.()
    } catch (raw) {
      toast(t(errorMessageKey(normalizeError(raw))), 'error')
    }
  }

  async function exportChatFile(id: string, format: 'markdown' | 'json') {
    try {
      await exportChat(id, format)
      toast(t('chat.exported'), 'success')
    } catch (raw) {
      toast(t(errorMessageKey(normalizeError(raw))), 'error')
    }
  }

  return (
    <div className="sidebar-inner">
      <div className="brand">
        <div className="brandmark" aria-hidden="true">
          <span>G</span>
        </div>
        <div>
          <strong>{t('app.name')}</strong>
          <small>{t('app.tagline')}</small>
        </div>
        {onClose && (
          <button
            type="button"
            className="icon-button sidebar-close"
            onClick={onClose}
            aria-label={t('common.close')}
          >
            <X size={17} />
          </button>
        )}
      </div>

      <Button className="newchat" variant="outline" onClick={() => void create()}>
        <Plus size={16} />
        <span>{t('sidebar.newChat')}</span>
      </Button>

      <div className="side-search">
        <Search size={14} aria-hidden="true" />
        <input
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          placeholder={t('sidebar.searchChats')}
          aria-label={t('sidebar.searchChats')}
        />
        {query && (
          <button
            type="button"
            className="icon-button tiny"
            onClick={() => setQuery('')}
            aria-label={t('common.dismiss')}
          >
            <X size={12} />
          </button>
        )}
      </div>
      {query && <p className="side-scope">{t('sidebar.scopeLimited')}</p>}

      <div className="side-label">
        <span>{t('sidebar.recent')}</span>
        <span>{chats.length}</span>
      </div>

      <div className="chat-list">
        {filtered.length === 0 && (
          <p className="empty-list">{query ? t('chat.noResults') : t('sidebar.noChats')}</p>
        )}
        {filtered.map((chat) => {
          const run = getChat(chat.id).run
          const busy = run && ['submitting', 'streaming', 'cancelling'].includes(run.phase)
          return (
            <div key={chat.id} className={cn('chat-item-row', activeId === chat.id && 'selected')}>
              <button
                type="button"
                className={cn('chat-item', activeId === chat.id && 'selected')}
                onClick={() => void choose(chat.id)}
              >
                <MessageSquare size={15} aria-hidden="true" />
                <span className="chat-item-title">{chat.title || t('chat.newChatTitle')}</span>
                {busy && <span className="run-dot" aria-label={t('chat.backgroundRun')} />}
              </button>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <button
                    type="button"
                    className="icon-button chat-menu"
                    aria-label={t('sidebar.more')}
                    onClick={(event) => event.stopPropagation()}
                  >
                    <MoreHorizontal size={15} />
                  </button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onSelect={() => onRename(chat.id, chat.title || '')}>
                    <Pencil size={14} /> {t('sidebar.rename')}
                  </DropdownMenuItem>
                  <DropdownMenuItem onSelect={() => void exportChatFile(chat.id, 'markdown')}>
                    <Download size={14} /> {t('sidebar.exportMarkdown')}
                  </DropdownMenuItem>
                  <DropdownMenuItem onSelect={() => void exportChatFile(chat.id, 'json')}>
                    <Download size={14} /> {t('sidebar.exportJson')}
                  </DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem danger onSelect={() => onDelete(chat.id)}>
                    <Trash2 size={14} /> {t('sidebar.deleteChat')}
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        })}
        {nextCursor && (
          <Button variant="ghost" size="sm" className="load-more" onClick={() => void loadMoreChats()}>
            {listLoading ? t('common.loading') : t('sidebar.loadMore')}
          </Button>
        )}
      </div>

      <div className="side-bottom">
        <button type="button" onClick={onOpenModel}>
          <Settings2 size={16} />
          <span>{t('sidebar.modelSettings')}</span>
          <ChevronDown size={13} />
        </button>
        <button type="button" onClick={onOpenSettings}>
          <Settings2 size={16} />
          <span>{t('sidebar.settings')}</span>
        </button>
        {configured && (
          <button type="button" onClick={onDisconnect}>
            <LogOut size={16} />
            <span>{t('login.logout')}</span>
          </button>
        )}
      </div>
    </div>
  )
}
