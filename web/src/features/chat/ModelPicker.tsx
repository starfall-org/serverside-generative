import * as React from 'react'
import { CircleAlert, Info, KeyRound, Server, SlidersHorizontal } from 'lucide-react'
import type { CatalogProvider, ModelSelection } from '../../api/types'
import { useChat } from './ChatProvider'
import { useConnection } from '../connection/ConnectionProvider'
import { usePreferences } from '../../app/preferences'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '../../components/ui/dialog'
import { Button } from '../../components/ui/button'
import { Input, Label, Textarea } from '../../components/ui/input'
import { Combobox } from '../../components/ui/combobox'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../../components/ui/select'
import { errorMessageKey } from '../../i18n/errors'
import { AppError } from '../../lib/errors'
import { utf8Bytes } from '../../lib/bytes'

interface FormState {
  source: 'stored' | 'mobile_ephemeral'
  providerId: string
  modelId: string
  keyId: string
  providerType: ModelSelection['providerType']
  baseUrl: string
  apiKey: string
  maxTokens: string
  temperature: string
  topP: string
  topK: string
  instructions: string
}

const ADAPTERS: ModelSelection['providerType'][] = ['openai', 'openai_responses', 'anthropic', 'google']

function availabilityLabel(t: (key: any) => string, availability: string): string {
  switch (availability) {
    case 'available':
      return t('chat.available')
    case 'credential_unavailable':
      return t('chat.credentialUnavailable')
    case 'key_selection_required':
      return t('chat.keyRequired')
    case 'unsupported':
      return t('chat.unsupported')
    default:
      return t('chat.unavailable')
  }
}

function initialForm(selection: ModelSelection, instructions: string): FormState {
  return {
    source: selection.source,
    providerId: selection.providerId,
    modelId: selection.modelId,
    keyId: selection.keyId,
    providerType: selection.providerType,
    baseUrl: selection.baseUrl,
    apiKey: '',
    maxTokens: selection.maxTokens,
    temperature: selection.temperature,
    topP: selection.topP,
    topK: selection.topK,
    instructions,
  }
}

export function ModelPicker({
  open,
  onOpenChange,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const { t } = usePreferences()
  const {
    catalog,
    catalogLoading,
    catalogError,
    selection,
    setSelection,
    active,
    setInstructions,
    refreshCatalog,
  } = useChat()
  const { capabilities } = useConnection()
  const [form, setForm] = React.useState<FormState>(() => initialForm(selection, active.instructions))
  const [error, setError] = React.useState('')

  React.useEffect(() => {
    if (open) {
      setForm(initialForm(selection, active.instructions))
      setError('')
    }
    // Reset only when the dialog opens, not on every selection/instruction edit.
  }, [open]) // eslint-disable-line react-hooks/exhaustive-deps

  const providers = catalog?.providers ?? []
  const provider = providers.find((item) => item.id === form.providerId)
  const adapterParams =
    capabilities?.chat_features?.negotiation?.adapters?.[form.providerType]?.parameters ?? []
  const bounds = capabilities?.chat_features?.negotiation?.parameters ?? {}

  function update<K extends keyof FormState>(key: K, value: FormState[K]) {
    setForm((current) => ({ ...current, [key]: value }))
  }

  function chooseProvider(id: string) {
    const next = providers.find((item) => item.id === id)
    const model = next?.models.find((item) => item.text_generation) ?? next?.models[0]
    setForm((current) => ({
      ...current,
      providerId: id,
      modelId: model?.id ?? '',
      keyId: next?.keys[0]?.id ?? '',
    }))
  }

  function validateNumber(
    value: string,
    name: string,
    min: number | undefined,
    max: number | undefined,
  ): number | undefined {
    if (!value.trim()) return undefined
    const parsed = Number(value)
    if (!Number.isFinite(parsed)) throw new AppError(`${name} must be a number`, { kind: 'invalid' })
    if (min !== undefined && parsed < min)
      throw new AppError(`${name} must be at least ${min}`, { kind: 'invalid' })
    if (max !== undefined && parsed > max)
      throw new AppError(`${name} must be at most ${max}`, { kind: 'invalid' })
    return parsed
  }

  function apply() {
    setError('')
    try {
      if (form.source === 'stored') {
        if (!form.providerId) throw new AppError(t('error.modelRequired'), { kind: 'invalid' })
        if (!form.modelId.trim()) throw new AppError(t('error.modelRequired'), { kind: 'invalid' })
        const providerInfo = providers.find((item) => item.id === form.providerId)
        if (providerInfo?.key_selection_required && !form.keyId) {
          throw new AppError(t('chat.keyRequired'), { kind: 'invalid' })
        }
      } else {
        if (!form.modelId.trim()) throw new AppError(t('error.modelRequired'), { kind: 'invalid' })
        validateNumber(
          form.temperature,
          t('chat.temperature'),
          bounds.temperature?.min,
          bounds.temperature?.max,
        )
        validateNumber(form.topP, t('chat.topP'), bounds.top_p?.min, bounds.top_p?.max)
        validateNumber(form.topK, t('chat.topK'), bounds.top_k?.min, bounds.top_k?.max)
        validateNumber(form.maxTokens, t('chat.maxTokens'), bounds.max_tokens?.min, bounds.max_tokens?.max)
        if (utf8Bytes(form.instructions) > 16 << 10) {
          throw new AppError('Instructions are too long', { kind: 'invalid' })
        }
      }
      const next: ModelSelection = {
        source: form.source,
        providerId: form.providerId,
        modelId: form.modelId.trim(),
        keyId: form.keyId,
        providerType: form.providerType,
        baseUrl: form.baseUrl,
        maxTokens: form.maxTokens,
        temperature: form.temperature,
        topP: form.topP,
        topK: form.topK,
      }
      setSelection(next, form.source === 'mobile_ephemeral' ? form.apiKey : undefined)
      setInstructions(form.instructions)
      onOpenChange(false)
    } catch (raw) {
      if (raw instanceof AppError) setError(raw.message)
      else setError(t('error.unknown'))
    }
  }

  const modelItems =
    provider?.models.map((model) => ({
      value: model.id,
      label: model.name || model.id,
      description: [
        model.id,
        availabilityLabel(t, model.availability),
        model.context_limit ? `${t('chat.contextLimit')} ${model.context_limit}` : '',
      ]
        .filter(Boolean)
        .join(' · '),
      disabled: false,
    })) ?? []

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="dialog-wide">
        <DialogHeader>
          <DialogTitle>{t('chat.chooseModel')}</DialogTitle>
          <DialogDescription>{t('chat.storedHint')}</DialogDescription>
        </DialogHeader>

        <div className="tabs" role="tablist" aria-label={t('chat.chooseModel')}>
          <button
            type="button"
            role="tab"
            aria-selected={form.source === 'stored'}
            className={form.source === 'stored' ? 'active' : ''}
            onClick={() => update('source', 'stored')}
          >
            <Server size={14} /> {t('chat.stored')}
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={form.source === 'mobile_ephemeral'}
            className={form.source === 'mobile_ephemeral' ? 'active' : ''}
            onClick={() => update('source', 'mobile_ephemeral')}
          >
            <KeyRound size={14} /> {t('chat.temporary')}
          </button>
        </div>

        {catalogLoading ? (
          <p className="muted">{t('common.loading')}</p>
        ) : catalogError ? (
          <div className="banner banner-error">
            <CircleAlert size={15} />
            <span>{t(errorMessageKey(catalogError))}</span>
            <Button variant="ghost" size="sm" onClick={() => void refreshCatalog()}>
              {t('common.retry')}
            </Button>
          </div>
        ) : providers.length === 0 && form.source === 'stored' ? (
          <p className="muted">{t('settings.emptyCatalog')}</p>
        ) : null}

        {form.source === 'stored' ? (
          <div className="form-grid">
            <div className="form-row">
              <Label htmlFor="provider-select">{t('chat.provider')}</Label>
              <Combobox
                ariaLabel={t('chat.provider')}
                items={providers.map((item: CatalogProvider) => ({
                  value: item.id,
                  label: item.name || item.id,
                  description: `${item.type} · ${item.models.length} models`,
                }))}
                value={form.providerId}
                onChange={chooseProvider}
                placeholder={t('chat.providerPlaceholder')}
                searchPlaceholder={t('common.search')}
                emptyText={t('chat.noResults')}
              />
            </div>
            <div className="form-row">
              <Label htmlFor="model-select">{t('chat.model')}</Label>
              {provider?.custom_model_id ? (
                <Input
                  id="model-select"
                  value={form.modelId}
                  placeholder={t('chat.customModelPlaceholder')}
                  onChange={(event) => update('modelId', event.target.value)}
                />
              ) : (
                <Combobox
                  ariaLabel={t('chat.model')}
                  items={modelItems}
                  value={form.modelId}
                  onChange={(value) => update('modelId', value)}
                  placeholder={t('chat.chooseModel')}
                  searchPlaceholder={t('common.search')}
                  emptyText={t('chat.noResults')}
                />
              )}
            </div>
            {provider && provider.keys.length > 0 && (
              <div className="form-row">
                <Label>{t('chat.key')}</Label>
                <Select value={form.keyId} onValueChange={(value) => update('keyId', value)}>
                  <SelectTrigger aria-label={t('chat.key')}>
                    <SelectValue placeholder={t('chat.keyPlaceholder')} />
                  </SelectTrigger>
                  <SelectContent>
                    {provider.keys.map((key) => (
                      <SelectItem key={key.id} value={key.id}>
                        {key.label || key.id} · {availabilityLabel(t, key.availability)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            )}
            {provider?.models.find((model) => model.id === form.modelId) && (
              <p className="field-hint">
                {availabilityLabel(
                  t,
                  provider.models.find((model) => model.id === form.modelId)?.availability || 'unavailable',
                )}
                {provider.models.find((model) => model.id === form.modelId)?.context_limit
                  ? ` · ${t('chat.contextLimit')}: ${provider.models.find((model) => model.id === form.modelId)?.context_limit}`
                  : ''}
                {provider.models.find((model) => model.id === form.modelId)?.output_limit
                  ? ` · ${t('chat.outputLimit')}: ${provider.models.find((model) => model.id === form.modelId)?.output_limit}`
                  : ''}
              </p>
            )}
          </div>
        ) : (
          <div className="form-grid">
            <p className="field-hint">
              <Info size={13} /> {t('chat.temporaryHint')}
            </p>
            <div className="form-grid two">
              <div className="form-row">
                <Label>{t('chat.provider')}</Label>
                <Select
                  value={form.providerType}
                  onValueChange={(value) => update('providerType', value as FormState['providerType'])}
                >
                  <SelectTrigger aria-label={t('chat.provider')}>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {ADAPTERS.map((adapter) => (
                      <SelectItem key={adapter} value={adapter}>
                        {adapter}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="form-row">
                <Label htmlFor="ephemeral-model">{t('chat.model')}</Label>
                <Input
                  id="ephemeral-model"
                  value={form.modelId}
                  placeholder={t('chat.customModelPlaceholder')}
                  onChange={(event) => update('modelId', event.target.value)}
                />
              </div>
            </div>
            <div className="form-row">
              <Label htmlFor="base-url">
                {t('chat.baseUrl')} <span className="optional">{t('common.optional')}</span>
              </Label>
              <Input
                id="base-url"
                value={form.baseUrl}
                placeholder={t('chat.baseUrlPlaceholder')}
                onChange={(event) => update('baseUrl', event.target.value)}
              />
            </div>
            <div className="form-row">
              <Label htmlFor="api-key">{t('chat.apiKey')}</Label>
              <Input
                id="api-key"
                type="password"
                autoComplete="off"
                value={form.apiKey}
                placeholder={t('chat.apiKeyPlaceholder')}
                onChange={(event) => update('apiKey', event.target.value)}
              />
            </div>
            <div className="form-grid three">
              {adapterParams.includes('max_tokens') && (
                <div className="form-row">
                  <Label htmlFor="max-tokens">{t('chat.maxTokens')}</Label>
                  <Input
                    id="max-tokens"
                    inputMode="numeric"
                    value={form.maxTokens}
                    onChange={(event) => update('maxTokens', event.target.value)}
                  />
                </div>
              )}
              {adapterParams.includes('temperature') && (
                <div className="form-row">
                  <Label htmlFor="temperature">{t('chat.temperature')}</Label>
                  <Input
                    id="temperature"
                    inputMode="decimal"
                    value={form.temperature}
                    onChange={(event) => update('temperature', event.target.value)}
                  />
                </div>
              )}
              {adapterParams.includes('top_p') && (
                <div className="form-row">
                  <Label htmlFor="top-p">{t('chat.topP')}</Label>
                  <Input
                    id="top-p"
                    inputMode="decimal"
                    value={form.topP}
                    onChange={(event) => update('topP', event.target.value)}
                  />
                </div>
              )}
              {adapterParams.includes('top_k') && (
                <div className="form-row">
                  <Label htmlFor="top-k">{t('chat.topK')}</Label>
                  <Input
                    id="top-k"
                    inputMode="numeric"
                    value={form.topK}
                    onChange={(event) => update('topK', event.target.value)}
                  />
                </div>
              )}
            </div>
          </div>
        )}

        <details className="advanced-block" open={Boolean(form.instructions)}>
          <summary>
            <SlidersHorizontal size={14} /> {t('chat.systemInstructions')}
          </summary>
          <Textarea
            value={form.instructions}
            rows={4}
            placeholder={t('chat.systemInstructions')}
            onChange={(event) => update('instructions', event.target.value)}
          />
          <p className="field-hint">{t('chat.instructionsHint')}</p>
        </details>

        {error && (
          <div className="banner banner-error" role="alert">
            <CircleAlert size={15} />
            <span>{error}</span>
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t('common.cancel')}
          </Button>
          <Button onClick={apply}>{t('common.apply')}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
