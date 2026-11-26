import type { AppError } from '../lib/errors'
import type { MessageKey } from './strings'

const codeKeys: Record<string, MessageKey> = {
  web_password_required: 'login.setup',
  web_origin_not_allowed: 'login.origin',
  unauthorized: 'error.unauthorized',
  auth_required: 'error.unauthorized',
  forbidden: 'error.forbidden',
  permission_denied: 'error.forbidden',
  not_found: 'error.notFound',
  revision_conflict: 'error.revisionConflict',
  session_busy: 'error.sessionBusy',
  request_already_recorded: 'error.requestRecorded',
  payload_too_large: 'error.payloadTooLarge',
  resource_exhausted: 'error.payloadTooLarge',
  event_gap: 'error.eventGap',
  unsupported_feature: 'error.unsupported',
  unsupported_provider_adapter: 'error.runtimeUnsupported',
  missing_runtime_config: 'error.modelRequired',
  rate_limited: 'error.rateLimited',
  secret_leak_blocked: 'error.forbidden',
  invalid_request: 'error.invalid',
}

const kindKeys: Record<AppError['kind'], MessageKey> = {
  network: 'error.network',
  timeout: 'error.timeout',
  unauthorized: 'error.unauthorized',
  forbidden: 'error.forbidden',
  not_found: 'error.notFound',
  conflict: 'error.conflict',
  invalid: 'error.invalid',
  rate_limited: 'error.rateLimited',
  server: 'error.server',
  unsupported: 'error.unsupported',
  aborted: 'error.aborted',
  unknown: 'error.unknown',
}

export function errorMessageKey(error: AppError): MessageKey {
  return (error.code && codeKeys[error.code]) || kindKeys[error.kind] || 'error.unknown'
}

export function inviteErrorKey(message: string): MessageKey {
  switch (message) {
    case 'invite.empty':
      return 'error.inviteEmpty'
    case 'invite.invalidUrl':
      return 'error.inviteInvalidUrl'
    case 'invite.invalidPath':
      return 'error.inviteInvalidPath'
    case 'invite.missingCode':
      return 'error.inviteMissingCode'
    case 'invite.unsupportedVersion':
      return 'error.inviteVersion'
    case 'invite.crossOrigin':
      return 'error.inviteCrossOrigin'
    case 'invite.insecure':
      return 'error.inviteInsecure'
    default:
      return 'error.unknown'
  }
}
