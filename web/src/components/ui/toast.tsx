import * as React from 'react'
import { createPortal } from 'react-dom'
import { CheckCircle2, CircleAlert, Info, X } from 'lucide-react'
import { cn } from './utils'

type ToastTone = 'info' | 'success' | 'error'
interface ToastItem {
  id: number
  message: string
  tone: ToastTone
}

interface ToastContextValue {
  toast: (message: string, tone?: ToastTone) => void
}

const ToastContext = React.createContext<ToastContextValue | null>(null)

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [items, setItems] = React.useState<ToastItem[]>([])
  const counter = React.useRef(0)

  const dismiss = React.useCallback((id: number) => {
    setItems((current) => current.filter((item) => item.id !== id))
  }, [])

  const toast = React.useCallback(
    (message: string, tone: ToastTone = 'info') => {
      const id = ++counter.current
      setItems((current) => [...current.slice(-3), { id, message, tone }])
      window.setTimeout(() => dismiss(id), 5000)
    },
    [dismiss],
  )

  return (
    <ToastContext.Provider value={{ toast }}>
      {children}
      {createPortal(
        <div className="toast-viewport" role="region" aria-live="polite" aria-label="Notifications">
          {items.map((item) => (
            <div key={item.id} className={cn('toast', `toast-${item.tone}`)}>
              <span className="toast-icon" aria-hidden="true">
                {item.tone === 'success' ? (
                  <CheckCircle2 size={16} />
                ) : item.tone === 'error' ? (
                  <CircleAlert size={16} />
                ) : (
                  <Info size={16} />
                )}
              </span>
              <span className="toast-message">{item.message}</span>
              <button
                className="icon-button toast-close"
                onClick={() => dismiss(item.id)}
                aria-label="Dismiss"
              >
                <X size={14} />
              </button>
            </div>
          ))}
        </div>,
        document.body,
      )}
    </ToastContext.Provider>
  )
}

export function useToast(): ToastContextValue {
  const context = React.useContext(ToastContext)
  if (!context) throw new Error('useToast must be used inside ToastProvider')
  return context
}
