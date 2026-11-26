import * as React from 'react'
import { ArrowUp, Square, Sparkles } from 'lucide-react'
import { useChat } from './ChatProvider'
import { usePreferences } from '../../app/preferences'
import { Button } from '../../components/ui/button'
import { utf8Bytes, formatBytes } from '../../lib/bytes'
import { errorMessageKey } from '../../i18n/errors'
import { normalizeError } from '../../lib/errors'
import { useToast } from '../../components/ui/toast'

export function Composer({ onOpenModel }: { onOpenModel: () => void }) {
  const { t } = usePreferences()
  const { toast } = useToast()
  const { active, activeId, selection, setDraft, send, cancel, maxRunBytes } = useChat()
  const textareaRef = React.useRef<HTMLTextAreaElement>(null)
  const composingRef = React.useRef(false)
  const [sending, setSending] = React.useState(false)

  const run = active.run
  const busy = Boolean(run && ['submitting', 'streaming', 'cancelling'].includes(run.phase))
  const bytes = utf8Bytes(active.draft)
  const overLimit = bytes > maxRunBytes

  React.useEffect(() => {
    const node = textareaRef.current
    if (!node) return
    node.style.height = '0px'
    node.style.height = `${Math.min(220, node.scrollHeight)}px`
  }, [active.draft])

  React.useEffect(() => {
    if (!activeId) textareaRef.current?.focus()
  }, [activeId])

  async function submit() {
    const content = active.draft.trim()
    if (!content || busy || overLimit || sending) return
    setSending(true)
    try {
      await send(content)
    } catch (raw) {
      const error = normalizeError(raw)
      toast(t(errorMessageKey(error)), 'error')
    } finally {
      setSending(false)
    }
  }

  function onKeyDown(event: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key !== 'Enter' || event.shiftKey) return
    if (event.nativeEvent.isComposing || composingRef.current) return
    event.preventDefault()
    void submit()
  }

  const modelLabel = selection.modelId || t('chat.noModel')

  return (
    <div className="composer-wrap">
      <form
        className={`composer ${overLimit ? 'over-limit' : ''}`}
        onSubmit={(event) => {
          event.preventDefault()
          void submit()
        }}
      >
        <textarea
          ref={textareaRef}
          className="composer-input"
          value={active.draft}
          rows={1}
          placeholder={t('chat.composerPlaceholder')}
          aria-label={t('chat.composerPlaceholder')}
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={onKeyDown}
          onCompositionStart={() => {
            composingRef.current = true
          }}
          onCompositionEnd={() => {
            composingRef.current = false
          }}
          enterKeyHint="send"
        />
        <div className="composer-bar">
          <button type="button" className="model-chip" onClick={onOpenModel}>
            <Sparkles size={13} />
            <span>{modelLabel}</span>
          </button>
          <div className="composer-right">
            {active.draft.length > 0 && (
              <span className={`byte-count ${overLimit ? 'danger' : ''}`}>
                {overLimit
                  ? t('chat.tooLarge', { limit: formatBytes(maxRunBytes) })
                  : t('chat.inputLimit', { used: formatBytes(bytes), limit: formatBytes(maxRunBytes) })}
              </span>
            )}
            {busy ? (
              <Button
                type="button"
                variant="outline"
                size="icon"
                className="send-button stop"
                aria-label={t('chat.stop')}
                disabled={run?.phase === 'cancelling'}
                onClick={() => void cancel()}
              >
                <Square size={14} />
              </Button>
            ) : (
              <Button
                type="submit"
                size="icon"
                className="send-button"
                aria-label={t('chat.send')}
                disabled={!active.draft.trim() || overLimit || sending || !selection.modelId}
              >
                <ArrowUp size={16} />
              </Button>
            )}
          </div>
        </div>
      </form>
      <p className="composer-hint">{t('chat.composerHint')}</p>
    </div>
  )
}
