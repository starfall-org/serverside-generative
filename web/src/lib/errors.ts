export type ErrorKind =
  | 'network'
  | 'timeout'
  | 'unauthorized'
  | 'forbidden'
  | 'not_found'
  | 'conflict'
  | 'invalid'
  | 'rate_limited'
  | 'server'
  | 'unsupported'
  | 'aborted'
  | 'unknown'

export class AppError extends Error {
  readonly kind: ErrorKind
  readonly status: number
  readonly code: string
  readonly retryable: boolean

  constructor(
    message: string,
    options: { kind?: ErrorKind; status?: number; code?: string; retryable?: boolean; cause?: unknown } = {},
  ) {
    super(message, { cause: options.cause })
    this.name = 'AppError'
    this.kind = options.kind ?? 'unknown'
    this.status = options.status ?? 0
    this.code = options.code ?? ''
    this.retryable = options.retryable ?? isRetryableKind(this.kind)
  }
}

function isRetryableKind(kind: ErrorKind): boolean {
  return kind === 'network' || kind === 'timeout' || kind === 'rate_limited' || kind === 'server'
}

export function errorFromStatus(status: number, code = '', message = ''): AppError {
  const kind: ErrorKind =
    status === 401
      ? 'unauthorized'
      : status === 403
        ? 'forbidden'
        : status === 404
          ? 'not_found'
          : status === 409
            ? 'conflict'
            : status === 400 || status === 413 || status === 422
              ? 'invalid'
              : status === 429
                ? 'rate_limited'
                : status === 501
                  ? 'unsupported'
                  : status >= 500
                    ? 'server'
                    : 'unknown'
  return new AppError(message || `Request failed (${status})`, { kind, status, code })
}

export function normalizeError(error: unknown): AppError {
  if (error instanceof AppError) return error
  if (error instanceof DOMException && error.name === 'AbortError') {
    return new AppError('Request aborted', { kind: 'aborted' })
  }
  if (error instanceof TypeError) {
    return new AppError(error.message || 'Network unavailable', { kind: 'network', cause: error })
  }
  if (error instanceof Error) return new AppError(error.message, { cause: error })
  return new AppError(String(error))
}

export function isAbort(error: unknown): boolean {
  return (
    (error instanceof AppError && error.kind === 'aborted') ||
    (error instanceof DOMException && error.name === 'AbortError')
  )
}
