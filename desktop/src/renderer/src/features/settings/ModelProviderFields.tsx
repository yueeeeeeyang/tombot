import { useState } from 'react'
import { getBackend } from '../desktop/backend'
import { validateSettingsDraft, type SettingsDraft } from './settings'

interface ModelProviderFieldsProps {
  draft: SettingsDraft
  onChange: (draft: SettingsDraft) => void
  apiKey: string
  hasApiKey: boolean
  onApiKeyChange: (apiKey: string) => void
}

/** 内置提供商只填密钥并由服务端拉模型；自定义提供商的地址和模型都手填。 */
export function ModelProviderFields({
  draft,
  onChange,
  apiKey,
  hasApiKey,
  onApiKeyChange,
}: ModelProviderFieldsProps) {
  const builtin = draft.provider === 'deepseek'
  const errors = validateSettingsDraft(draft)
  const [models, setModels] = useState<Array<{ id: string; name: string }>>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function pullModels(): Promise<void> {
    setBusy(true)
    setError('')
    try {
      const listed = await getBackend().listProviderModels?.('deepseek', apiKey)
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
      <div className="setting-row provider-mode">
        <span>提供商类型</span>
        <span className="provider-mode-actions">
          <button
            type="button"
            className={builtin ? 'is-selected' : ''}
            aria-pressed={builtin}
            onClick={() =>
              onChange({ ...draft, provider: 'deepseek', providerName: 'DeepSeek', baseUrl: '' })
            }
          >
            内置提供商
          </button>
          <button
            type="button"
            className={!builtin ? 'is-selected' : ''}
            aria-pressed={!builtin}
            onClick={() =>
              onChange({
                ...draft,
                provider: 'custom',
                providerName: draft.provider === 'deepseek' ? '' : draft.providerName,
              })
            }
          >
            自定义提供商
          </button>
        </span>
      </div>
      {builtin ? (
        <>
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
            <button type="button" disabled={busy} onClick={() => void pullModels()}>
              {busy ? '正在拉取' : '拉取模型'}
            </button>
          </div>
          <label className="setting-row" htmlFor="builtin-model">
            <span>默认模型</span>
            <select
              id="builtin-model"
              className="setting-text-input"
              aria-label="默认模型"
              value={draft.modelName}
              onChange={(event) => onChange({ ...draft, modelName: event.target.value })}
            >
              <option value="">请选择</option>
              {models.map((model) => (
                <option key={model.id} value={model.id}>
                  {model.name || model.id}
                </option>
              ))}
              {draft.modelName && !models.some((model) => model.id === draft.modelName) && (
                <option value={draft.modelName}>{draft.modelName}</option>
              )}
            </select>
          </label>
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
