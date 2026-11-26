import * as React from 'react'
import {
  AlertTriangle,
  ArrowDown,
  ChevronLeft,
  ChevronRight,
  CircleAlert,
  Copy,
  MessageSquare,
  Sparkles,
  Wrench,
} from 'lucide-react'
import type { ChatMessage, MessageVersion, ToolActivity } from '../../api/types'
import { useChat, type ChatState } from './ChatProvider'
import { usePreferences } from '../../app/preferences'
import { Markdown } from './Markdown'
import { Button } from '../../components/ui/button'
import { Skeleton } from '../../components/ui/skeleton'
import { formatTime } from '../../lib/format'
import { useToast } from '../../components/ui/toast'

const PAGE_SIZE = 150

function mergeTools(persisted: ToolActivity[] | undefined, live: ToolActivity[] | undefined): ToolActivity[] {
  const map = new Map<string, ToolActivity>()
  for (const tool of persisted ?? []) map.set(tool.id, tool)
  for (const tool of live ?? []) map.set(tool.id, { ...map.get(tool.id), ...tool })
  return Array.from(map.values())
}

function bounded(text: string | undefined, limit = 6000): string {
  if (!text) return ''
  return text.length > limit ? `${text.slice(0, limit)}\n…` : text
}

function ReasoningPanel({ text, label }: { text: string; label: string }) {
  if (!text) return null
  return (
    <details className="reasoning">
      <summary>{label}</summary>
      <div className="reasoning-body">{text}</div>
    </details>
  )
}

function ToolPanel({ tools, labels }: { tools: ToolActivity[]; labels: Record<string, string> }) {
  if (tools.length === 0) return null
  return (
    <div className="tool-list">
      {tools.map((tool) => {
        const status = tool.status || 'unknown'
        return (
          <details key={tool.id} className={`tool tool-${status}`}>
            <summary>
              <Wrench size={13} aria-hidden="true" />
              <span className="tool-name">{tool.name}</span>
              <span className="tool-status">{labels[status] || status}</span>
            </summary>
            {tool.arguments && (
              <div className="tool-payload">
                <span>arguments</span>
                <pre>{bounded(tool.arguments)}</pre>
              </div>
            )}
            {tool.response && (
              <div className="tool-payload">
                <span>response</span>
                <pre>{bounded(tool.response)}</pre>
              </div>
            )}
          </details>
        )
      })}
    </div>
  )
}

function MessageArticle({
  message,
  streaming,
  run,
  labels,
}: {
  message: ChatMessage
  streaming: boolean
  run: ChatState['run']
  labels: Record<string, string>
}) {
  const { t, locale } = usePreferences()
  const { toast } = useToast()
  const [viewed, setViewed] = React.useState(message.active_version_index || 0)
  const versions = message.versions
  const version: MessageVersion | undefined = versions[viewed] ?? versions[0]
  if (!version) return null

  let content = version.content || ''
  let reasoning = version.reasoning_content || ''
  let tools = version.toolActivity ?? []
  if (streaming && run) {
    content += run.streamText
    if (run.reasoning) reasoning = reasoning ? `${reasoning}\n${run.reasoning}` : run.reasoning
    tools = mergeTools(tools, run.tools)
  }
  const timestamp = Number(version.timestamp || 0)
  const model = version.model_display_name || version.model_id

  async function copy() {
    try {
      await navigator.clipboard.writeText(content)
      toast(t('chat.messageCopied'), 'success')
    } catch {
      /* ignore */
    }
  }

  return (
    <article className={`message ${message.role === 'user' ? 'from-user' : 'from-ai'}`}>
      <div className="avatar" aria-hidden="true">
        {message.role === 'user' ? t('chat.roleUser').slice(0, 1) : <Sparkles size={15} />}
      </div>
      <div className="message-body">
        <div className="message-meta">
          <span className="message-label">
            {message.role === 'user'
              ? t('chat.roleUser')
              : message.role === 'system'
                ? 'System'
                : t('chat.roleAssistant')}
          </span>
          {timestamp > 0 && <span className="message-time">{formatTime(timestamp, locale)}</span>}
          {model && <span className="message-model">{model}</span>}
          {version.generation_interrupted && !streaming && (
            <span className="message-badge">{t('chat.interrupted')}</span>
          )}
          {versions.length > 1 && (
            <span className="version-nav">
              <button
                type="button"
                className="icon-button tiny"
                aria-label="Previous version"
                disabled={viewed <= 0}
                onClick={() => setViewed((value) => Math.max(0, value - 1))}
              >
                <ChevronLeft size={13} />
              </button>
              <span>{t('chat.version', { current: viewed + 1, total: versions.length })}</span>
              <button
                type="button"
                className="icon-button tiny"
                aria-label="Next version"
                disabled={viewed >= versions.length - 1}
                onClick={() => setViewed((value) => Math.min(versions.length - 1, value + 1))}
              >
                <ChevronRight size={13} />
              </button>
            </span>
          )}
        </div>
        <ReasoningPanel text={reasoning} label={t('chat.reasoning')} />
        {content ? (
          <Markdown content={content} />
        ) : streaming ? (
          <div className="thinking" role="status">
            <span className="typing-dot" />
            <span className="typing-dot" />
            <span className="typing-dot" />
            <span>{t('chat.thinking')}</span>
          </div>
        ) : null}
        <ToolPanel tools={tools} labels={labels} />
        {content && (
          <div className="message-actions">
            <button type="button" className="ghost-action" onClick={copy}>
              <Copy size={13} />
              <span>{t('common.copy')}</span>
            </button>
          </div>
        )}
      </div>
    </article>
  )
}

function ProvisionalMessage({
  run,
  labels,
}: {
  run: NonNullable<ChatState['run']>
  labels: Record<string, string>
}) {
  const { t } = usePreferences()
  return (
    <article className="message from-ai">
      <div className="avatar" aria-hidden="true">
        <Sparkles size={15} />
      </div>
      <div className="message-body">
        <div className="message-meta">
          <span className="message-label">{t('chat.roleAssistant')}</span>
          {run.phase === 'cancelling' && <span className="message-badge">{t('chat.cancelling')}</span>}
        </div>
        <ReasoningPanel text={run.reasoning} label={t('chat.reasoning')} />
        {run.streamText ? (
          <Markdown content={run.streamText} />
        ) : (
          <div className="thinking" role="status">
            <span className="typing-dot" />
            <span className="typing-dot" />
            <span className="typing-dot" />
            <span>{t('chat.thinking')}</span>
          </div>
        )}
        <ToolPanel tools={run.tools} labels={labels} />
      </div>
    </article>
  )
}

export function MessageList({ chatId, chat }: { chatId: string; chat: ChatState }) {
  const { t } = usePreferences()
  const { setDraft, reloadChat } = useChat()
  const scrollRef = React.useRef<HTMLDivElement>(null)
  const bottomRef = React.useRef<HTMLDivElement>(null)
  const nearBottomRef = React.useRef(true)
  const [showJump, setShowJump] = React.useState(false)
  const [limit, setLimit] = React.useState(PAGE_SIZE)

  const messages = chat.conversation?.messages ?? []
  const run = chat.run
  const streaming = Boolean(run && ['submitting', 'streaming', 'cancelling', 'unknown'].includes(run.phase))
  const lastMessage = messages[messages.length - 1]
  const showProvisional = streaming && lastMessage?.role !== 'model'
  const visibleStart = Math.max(0, messages.length - limit)
  const visible = messages.slice(visibleStart)

  React.useEffect(() => {
    setLimit(PAGE_SIZE)
    nearBottomRef.current = true
    const node = scrollRef.current
    if (node) node.scrollTop = node.scrollHeight
  }, [chatId])

  React.useEffect(() => {
    if (!nearBottomRef.current) return
    const frame = requestAnimationFrame(() => {
      const node = scrollRef.current
      if (node) node.scrollTop = node.scrollHeight
    })
    return () => cancelAnimationFrame(frame)
  }, [messages.length, run?.streamText, run?.reasoning, run?.tools.length])

  function onScroll() {
    const node = scrollRef.current
    if (!node) return
    const distance = node.scrollHeight - node.scrollTop - node.clientHeight
    nearBottomRef.current = distance < 120
    setShowJump(distance > 240)
  }

  function jump() {
    nearBottomRef.current = true
    setShowJump(false)
    const node = scrollRef.current
    if (node) node.scrollTo({ top: node.scrollHeight, behavior: 'smooth' })
  }

  const labels: Record<string, string> = {
    running: t('chat.toolRunning'),
    completed: t('chat.toolCompleted'),
    error: t('chat.toolError'),
  }

  if (chat.loading && !chat.conversation) {
    return (
      <div className="thread-scroll">
        <div className="thread loading-thread" aria-busy="true" aria-label={t('common.loading')}>
          <Skeleton className="skeleton-title" />
          <Skeleton className="skeleton-line" />
          <Skeleton className="skeleton-line short" />
          <Skeleton className="skeleton-block" />
        </div>
      </div>
    )
  }

  return (
    <div className="thread-wrap">
      <div className="thread-scroll" ref={scrollRef} onScroll={onScroll}>
        <div className="thread">
          {messages.length === 0 && !run ? (
            <div className="welcome">
              <div className="welcome-icon">
                <MessageSquare size={24} />
              </div>
              <span className="eyebrow">{t('chat.emptyEyebrow')}</span>
              <h1>{t('chat.emptyTitle')}</h1>
              <p>{t('chat.emptySubtitle')}</p>
              <div className="suggestions">
                {(['chat.suggestion1', 'chat.suggestion2', 'chat.suggestion3'] as const).map((key) => (
                  <button key={key} type="button" className="suggestion" onClick={() => setDraft(t(key))}>
                    {t(key)}
                  </button>
                ))}
              </div>
            </div>
          ) : (
            <>
              {visibleStart > 0 && (
                <button
                  type="button"
                  className="load-earlier"
                  onClick={() => setLimit((value) => value + PAGE_SIZE)}
                >
                  {t('chat.showEarlier')}
                </button>
              )}
              {visible.map((message, index) => (
                <MessageArticle
                  key={message.id}
                  message={message}
                  streaming={streaming && visibleStart + index === messages.length - 1}
                  run={run}
                  labels={labels}
                />
              ))}
              {showProvisional && run && <ProvisionalMessage run={run} labels={labels} />}
            </>
          )}
          <div ref={bottomRef} />
        </div>
      </div>
      {showJump && (
        <button type="button" className="jump-bottom" onClick={jump} aria-label={t('chat.scrollToBottom')}>
          <ArrowDown size={16} />
        </button>
      )}
      <div className="thread-banners" aria-live="polite">
        {chat.error && (
          <div className="banner banner-error" role="alert">
            <CircleAlert size={15} />
            <span>{chat.error.message}</span>
            <Button variant="ghost" size="sm" onClick={() => void reloadChat(chatId)}>
              {t('common.retry')}
            </Button>
          </div>
        )}
        {run?.phase === 'unknown' && (
          <div className="banner banner-warning">
            <AlertTriangle size={15} />
            <span>{t('error.eventGap')}</span>
            <Button variant="ghost" size="sm" onClick={() => void reloadChat(chatId)}>
              {t('common.reload')}
            </Button>
          </div>
        )}
        {run?.phase === 'interrupted' && (
          <div className="banner banner-info">
            <AlertTriangle size={15} />
            <span>{t('chat.generationInterrupted')}</span>
          </div>
        )}
        {chat.stale && (
          <div className="banner banner-warning">
            <AlertTriangle size={15} />
            <span>{t('error.revisionConflict')}</span>
            <Button variant="ghost" size="sm" onClick={() => void reloadChat(chatId)}>
              {t('common.reload')}
            </Button>
          </div>
        )}
      </div>
    </div>
  )
}
