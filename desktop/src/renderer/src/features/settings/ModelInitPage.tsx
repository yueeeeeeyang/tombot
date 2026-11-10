import { useState } from 'react'
import { SessionFrame } from '../session/SessionGate'
import { ModelProviderFields } from './ModelProviderFields'
import type { SettingsDraft } from './settings'

/** 没有默认模型时的整页。先在本页收齐字段，确认后才写入服务。 */
export function ModelInitPage({
  draft,
  onChange,
  apiKey,
  hasApiKey,
  onApiKeyChange,
  error,
}: {
  draft: SettingsDraft
  onChange: (draft: SettingsDraft) => void
  apiKey: string
  hasApiKey: boolean
  onApiKeyChange: (apiKey: string) => void
  error: string
}) {
  const [localDraft, setLocalDraft] = useState(draft)
  const [localKey, setLocalKey] = useState(apiKey)
  const builtin = localDraft.provider !== 'custom'
  const ready = builtin
    ? localKey.trim() !== '' && localDraft.modelName.trim() !== ''
    : localKey.trim() !== '' &&
      localDraft.providerName.trim() !== '' &&
      localDraft.baseUrl.trim() !== '' &&
      localDraft.modelName.trim() !== ''

  function continueNext(): void {
    if (!ready) return
    onChange(localDraft)
    onApiKeyChange(localKey)
  }

  return (
    <SessionFrame>
      <form
        className="session-card session-card--wide"
        aria-label="配置默认模型"
        onSubmit={(event) => {
          event.preventDefault()
          continueNext()
        }}
      >
        <h1>配置默认模型</h1>
        <p>先配置一个默认模型，助手才能回复。</p>
        <p>API Key 只保存在服务端，不会回显。</p>
        <ModelProviderFields
          draft={localDraft}
          onChange={setLocalDraft}
          apiKey={localKey}
          hasApiKey={hasApiKey}
          onApiKeyChange={setLocalKey}
        />
        {error && (
          <p className="form-error" role="alert">
            {error}
          </p>
        )}
        <button className="primary-button" type="submit" disabled={!ready}>
          继续
        </button>
      </form>
    </SessionFrame>
  )
}
