import { useEffect, useRef, useState } from 'react'
import { getBackend, type SettingsView } from '../desktop/backend'
import { createSettingsDraft, validateSettingsDraft, type SettingsDraft } from './settings'

/** 只保留表单字段。服务返回的 hasApiKey 不能再提交，否则会被当成未知字段拒绝。 */
function settingsDraftFrom(view: SettingsDraft): SettingsDraft {
  return {
    language: view.language,
    provider: view.provider,
    baseUrl: view.baseUrl,
    modelName: view.modelName,
    temperature: view.temperature,
    topP: view.topP,
    maxOutputTokens: view.maxOutputTokens,
    isStreaming: view.isStreaming,
  }
}

/** 把语言和模型设置保存到服务。主题仍由本机偏好负责，密钥不会写入本地存储。 */
export function useServerSettings() {
  const backend = getBackend()
  const sync = backend.trySettingsSync?.()
  const [draft, setDraftState] = useState<SettingsDraft>(
    sync ? settingsDraftFrom(sync) : createSettingsDraft(),
  )
  const [hasApiKey, setHasApiKey] = useState(sync?.hasApiKey ?? false)
  const [apiKey, setApiKeyState] = useState('')
  const [error, setError] = useState('')
  const dirty = useRef(false)
  const draftRef = useRef(draft)
  const apiKeyRef = useRef(apiKey)
  const saveQueue = useRef(Promise.resolve())

  useEffect(() => {
    if (sync) return
    let cancelled = false
    void backend.loadSettings().then((view) => {
      if (cancelled || dirty.current) return
      const next = settingsDraftFrom(view)
      draftRef.current = next
      setDraftState(next)
      setHasApiKey(view.hasApiKey)
    })
    return () => {
      cancelled = true
    }
  }, [backend, sync])

  function applySaved(view: SettingsView): void {
    setHasApiKey(view.hasApiKey)
    setError('')
  }

  function persist(next: SettingsDraft, key: string): void {
    const errors = validateSettingsDraft(next)
    if (errors.baseUrl || errors.maxOutputTokens) return
    const clean = settingsDraftFrom(next)
    if (backend.savesSynchronously) {
      try {
        applySaved(backend.saveSettings(clean, key) as SettingsView)
      } catch (failure) {
        setError((failure as Error).message)
      }
      return
    }
    // 连续输入会发出多次保存。按顺序写，避免较早的请求覆盖后面的模型名称。
    saveQueue.current = saveQueue.current.then(async () => {
      try {
        applySaved(await backend.saveSettings(clean, key))
      } catch (failure) {
        setError((failure as Error).message)
      }
    })
  }

  function setDraft(next: SettingsDraft): void {
    dirty.current = true
    draftRef.current = next
    setDraftState(next)
    persist(next, apiKeyRef.current)
  }

  function setApiKey(next: string): void {
    dirty.current = true
    apiKeyRef.current = next
    setApiKeyState(next)
    persist(draftRef.current, next)
  }

  return { draft, setDraft, apiKey, setApiKey, hasApiKey, error }
}
