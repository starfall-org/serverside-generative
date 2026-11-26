import * as React from 'react'
import { Check, ChevronsUpDown, Search } from 'lucide-react'
import { cn } from './utils'

export interface ComboboxItem {
  value: string
  label: string
  description?: string
  group?: string
  disabled?: boolean
}

interface ComboboxProps {
  items: ComboboxItem[]
  value: string
  onChange: (value: string) => void
  placeholder: string
  searchPlaceholder: string
  emptyText: string
  ariaLabel: string
  disabled?: boolean
  className?: string
}

export function Combobox({
  items,
  value,
  onChange,
  placeholder,
  searchPlaceholder,
  emptyText,
  ariaLabel,
  disabled,
  className,
}: ComboboxProps) {
  const [open, setOpen] = React.useState(false)
  const [query, setQuery] = React.useState('')
  const [active, setActive] = React.useState(0)
  const rootRef = React.useRef<HTMLDivElement>(null)
  const inputRef = React.useRef<HTMLInputElement>(null)
  const listId = React.useId()
  const selected = items.find((item) => item.value === value)
  const filtered = React.useMemo(() => {
    const needle = query.trim().toLowerCase()
    if (!needle) return items
    return items.filter(
      (item) =>
        item.label.toLowerCase().includes(needle) ||
        item.value.toLowerCase().includes(needle) ||
        item.description?.toLowerCase().includes(needle),
    )
  }, [items, query])

  React.useEffect(() => {
    if (!open) return
    const onPointer = (event: PointerEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('pointerdown', onPointer)
    return () => document.removeEventListener('pointerdown', onPointer)
  }, [open])

  React.useEffect(() => {
    if (open) {
      setQuery('')
      setActive(
        Math.max(
          0,
          filtered.findIndex((item) => item.value === value),
        ),
      )
      window.setTimeout(() => inputRef.current?.focus(), 0)
    }
  }, [open]) // eslint-disable-line react-hooks/exhaustive-deps

  function choose(item: ComboboxItem | undefined) {
    if (!item || item.disabled) return
    onChange(item.value)
    setOpen(false)
  }

  function onKeyDown(event: React.KeyboardEvent) {
    if (event.key === 'ArrowDown') {
      event.preventDefault()
      setActive((index) => Math.min(filtered.length - 1, index + 1))
    } else if (event.key === 'ArrowUp') {
      event.preventDefault()
      setActive((index) => Math.max(0, index - 1))
    } else if (event.key === 'Enter') {
      event.preventDefault()
      choose(filtered[active])
    } else if (event.key === 'Escape') {
      event.preventDefault()
      setOpen(false)
    }
  }

  return (
    <div ref={rootRef} className={cn('combobox', className)}>
      <button
        type="button"
        className="combobox-trigger"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label={ariaLabel}
        disabled={disabled}
        onClick={() => setOpen((current) => !current)}
        onKeyDown={(event) => {
          if (event.key === 'ArrowDown' || event.key === 'Enter') {
            event.preventDefault()
            setOpen(true)
          }
        }}
      >
        <span className={selected ? 'combobox-value' : 'combobox-placeholder'}>
          {selected?.label || placeholder}
        </span>
        <ChevronsUpDown size={14} aria-hidden="true" />
      </button>
      {open && (
        <div className="combobox-popover">
          <div className="combobox-search">
            <Search size={14} aria-hidden="true" />
            <input
              ref={inputRef}
              value={query}
              placeholder={searchPlaceholder}
              aria-label={searchPlaceholder}
              aria-controls={listId}
              aria-activedescendant={filtered[active] ? `${listId}-${active}` : undefined}
              onChange={(event) => {
                setQuery(event.target.value)
                setActive(0)
              }}
              onKeyDown={onKeyDown}
            />
          </div>
          <ul className="combobox-list" role="listbox" id={listId} aria-label={ariaLabel}>
            {filtered.length === 0 && <li className="combobox-empty">{emptyText}</li>}
            {filtered.map((item, index) => (
              <li
                key={item.value}
                id={`${listId}-${index}`}
                role="option"
                aria-selected={item.value === value}
                aria-disabled={item.disabled}
                className={cn(
                  'combobox-option',
                  index === active && 'active',
                  item.value === value && 'selected',
                  item.disabled && 'disabled',
                )}
                onPointerDown={(event) => {
                  event.preventDefault()
                  choose(item)
                }}
                onPointerMove={() => setActive(index)}
              >
                <span className="combobox-option-text">
                  <span>{item.label}</span>
                  {item.description && <small>{item.description}</small>}
                </span>
                {item.value === value && <Check size={14} aria-hidden="true" />}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}
