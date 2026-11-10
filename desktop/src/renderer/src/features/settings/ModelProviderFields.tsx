import { useEffect, useState } from 'react'
import { Select } from '../../components/Select'
import { getBackend } from '../desktop/backend'
import { validateSettingsDraft, type SettingsDraft } from './settings'

interface BuiltinProvider {
  id: string
  name: string
}

interface DiscoveredModel {
  id: string
  name: string
}

const fallbackProviders: BuiltinProvider[] = [{ id: 'deepseek', name: 'DeepSeek' }]

interface ModelProviderFieldsProps {
  draft: SettingsDraft
  onChange: (draft: SettingsDraft) => void
  apiKey: string
  hasApiKey: boolean
  onApiKeyChange: (apiKey: string) => void
}

/** 内置提供商先选择再填密钥；名单来自服务端，后面增加供应商时这里不用改。自定义提供商全部手填。 */
export function ModelProviderFields({
  draft,
  onChange,
  apiKey,
  hasApiKey,
  onApiKeyChange,
}: ModelProviderFieldsProps) {
  const [providers, setProviders] = useState<BuiltinProvider[]>(fallbackProviders)
  const [models, setModels] = useState<DiscoveredModel[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const builtin = providers.some((provider) => provider.id === draft.provider)
  const selected: BuiltinProvider = providers.find((provider) => provider.id === draft.provider) ??
    providers[0] ?? { id: 'deepseek', name: 'DeepSeek' }
  const errors = validateSettingsDraft(draft)
  const modelOptions = models.map((model) => ({
    value: model.id,
    label: model.name || model.id,
  }))
  if (draft.modelName && !modelOptions.some((option) => option.value === draft.modelName)) {
    modelOptions.unshift({ value: draft.modelName, label: draft.modelName })
  }

  useEffect(() => {
    const load = getBackend().listBuiltinProviders
    if (!load) return
    let cancelled = false
    void load()
      .then((listed) => {
        if (!cancelled && listed.length > 0) setProviders(listed)
      })
      .catch(() => undefined)
    return () => {
      cancelled = true
    }
  }, [])

  function chooseBuiltin(): void {
    const next = providers.find((provider) => provider.id === draft.provider) ?? providers[0]
    if (!next) return
    onChange({ ...draft, provider: next.id, providerName: next.name, baseUrl: '' })
  }

  function chooseProvider(id: string): void {
    const next = providers.find((provider) => provider.id === id)
    if (!next || next.id === draft.provider) return
    setModels([])
    onChange({
      ...draft,
      provider: next.id,
      providerName: next.name,
      baseUrl: '',
      modelName: '',
    })
  }

  async function pullModels(): Promise<void> {
    setBusy(true)
    setError('')
    try {
      const listed = await getBackend().listProviderModels?.(selected.id, apiKey)
      const first = listed?.[0]
      if (!listed?.length || !first) {
        setError('没有拉到可用模型。')
        return
      }
      setModels(listed)
      if (!listed.some((model) => model.id === draft.modelName)) {
        onChange({ ...draft, modelName: first.id })
      }
    } catch (reason) {
      setModels([])
      setError(reason instanceof Error ? reason.message : '拉取模型失败。')
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <div className="setting-row">
        <span>提供商类型</span>
        <span className="provider-kind" role="group" aria-label="提供商类型">
          <button type="button" aria-pressed={builtin} onClick={chooseBuiltin}>
            内置提供商
          </button>
          <button
            type="button"
            aria-pressed={!builtin}
            onClick={() =>
              onChange({
                ...draft,
                provider: 'custom',
                providerName: draft.provider === 'custom' ? draft.providerName : '',
              })
            }
          >
            自定义提供商
          </button>
        </span>
      </div>
      {builtin ? (
        <>
          <div className="setting-row">
            <span>提供商</span>
            <Select
              label="提供商"
              value={selected.id}
              onChange={chooseProvider}
              options={providers.map((provider) => ({ value: provider.id, label: provider.name }))}
            />
          </div>
          <p className="setting-note">接口地址已内置在服务端，只需填写 API Key 后拉取模型。</p>
          <label className="setting-row" htmlFor="model-api-key">
            <span>API 密钥</span>
            <input
              id="model-api-key"
              className="setting-text-input"
              type="password"
              value={apiKey}
              placeholder={hasApiKey ? '已保存，留空则使用已保存的密钥' : '输入 API 密钥'}
              autoComplete="off"
              onChange={(event) => onApiKeyChange(event.target.value)}
            />
          </label>
          <div className="setting-row">
            <span>模型列表</span>
            <button
              type="button"
              className="provider-fetch"
              disabled={busy}
              onClick={() => void pullModels()}
            >
              {busy ? '正在拉取' : '拉取模型'}
            </button>
          </div>
          <div className="setting-row">
            <span>默认模型</span>
            <Select
              label="默认模型"
              value={draft.modelName}
              placeholder="请先拉取模型"
              onChange={(modelName) => onChange({ ...draft, modelName })}
              options={modelOptions}
            />
          </div>
          {error && (
            <p className="setting-error" role="alert">
              {error}
            </p>
          )}
        </>
      ) : (
        <>
          <label className="setting-row" htmlFor="provider-name">
            <span>提供商名称</span>
            <input
              id="provider-name"
              className="setting-text-input"
              value={draft.providerName}
              maxLength={40}
              placeholder="例如 公司网关"
              onChange={(event) => onChange({ ...draft, providerName: event.target.value })}
            />
          </label>
          <div className="setting-field">
            <label className="setting-row" htmlFor="model-base-url">
              <span>API 地址</span>
              <input
                id="model-base-url"
                className="setting-text-input"
                type="url"
                value={draft.baseUrl}
                maxLength={500}
                placeholder="输入供应商 API 地址"
                aria-invalid={Boolean(errors.baseUrl)}
                aria-describedby={errors.baseUrl ? 'model-base-url-error' : undefined}
                onChange={(event) => onChange({ ...draft, baseUrl: event.target.value })}
              />
            </label>
            {errors.baseUrl && (
              <p className="setting-error" id="model-base-url-error" role="alert">
                {errors.baseUrl}
              </p>
            )}
          </div>
          <label className="setting-row" htmlFor="model-api-key">
            <span>API 密钥</span>
            <input
              id="model-api-key"
              className="setting-text-input"
              type="password"
              value={apiKey}
              placeholder={hasApiKey ? '已保存，留空则不修改' : '输入 API 密钥'}
              autoComplete="off"
              onChange={(event) => onApiKeyChange(event.target.value)}
            />
          </label>
          <label className="setting-row">
            <span>模型名称</span>
            <input
              className="setting-text-input"
              value={draft.modelName}
              maxLength={120}
              placeholder="输入模型名称"
              onChange={(event) => onChange({ ...draft, modelName: event.target.value })}
            />
          </label>
        </>
      )}
    </>
  )
}
