export function formatTime(ms: number | undefined, locale: string): string {
  if (!ms) return ''
  try {
    return new Intl.DateTimeFormat(locale, { hour: '2-digit', minute: '2-digit' }).format(new Date(ms))
  } catch {
    return ''
  }
}

export function formatDate(ms: number | undefined, locale: string): string {
  if (!ms) return ''
  try {
    return new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(ms))
  } catch {
    return ''
  }
}

export function relativeTime(ms: number | undefined, locale: string, now = Date.now()): string {
  if (!ms) return ''
  const diff = Math.round((ms - now) / 1000)
  const units: [Intl.RelativeTimeFormatUnit, number][] = [
    ['second', 60],
    ['minute', 60],
    ['hour', 24],
    ['day', 7],
    ['week', 4.35],
    ['month', 12],
    ['year', Number.POSITIVE_INFINITY],
  ]
  try {
    const formatter = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' })
    let value = diff
    for (const [unit, span] of units) {
      if (Math.abs(value) < span || unit === 'year') return formatter.format(Math.round(value), unit)
      value /= span
    }
  } catch {
    /* ignore */
  }
  return ''
}

export function downloadFile(name: string, content: string, type: string): void {
  const blob = new Blob([content], { type })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = name
  anchor.rel = 'noopener'
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

export function safeFilename(title: string, extension: string): string {
  const base =
    title
      .normalize('NFKD')
      .replace(/[^\p{L}\p{N}\-_. ]/gu, '')
      .trim()
      .replace(/\s+/g, '-')
      .slice(0, 60) || 'conversation'
  return `${base}.${extension}`
}
