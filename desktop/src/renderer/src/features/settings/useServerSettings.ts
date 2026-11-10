import { useEffect, useRef, useState } from 'react'
import { getBackend } from '../desktop/backend'
import { createSettingsDraft, validateSettingsDraft, type SettingsDraft } from './settings'

/** 把语言和模型设置保存到服务。主题仍由本机偏好负责，密钥不会写入本地存储。 */
export function useServerSettings() {
  const backend = getBackend()
  const sync = backend.trySettingsSync?.()
  const [draft, setDraftState] = useState<SettingsDraft>(sync ?? createSettingsDraft())
  const [hasApiKey, setHasApiKey] = useState(sync?.hasApiKey ?? false)
  const [apiKey, setApiKeyState] = useState('')
  const [error, setError] = useState('')
  const dirty = useRef(false)
  const draftRef = useRef(draft)
  const apiKeyRef = useRef(apiKey)

  useEffect(() => {
    if (sync) return
    let cancelled = false
    void backend.loadSettings().then((view) => {
      if (cancelled || dirty.current) return
      draftRef.current = view
      setDraftState(view)
      setHasApiKey(view.hasApiKey)
    })
    return () => {
      cancelled = true
    }
  }, [backend, sync])

  function persist(next: SettingsDraft, key: string): void {
    const errors = validateSettingsDraft(next)
    if (errors.baseUrl || errors.maxOutputTokens) return
    void Promise.resolve(backend.saveSettings(next, key)).then(
      (view) => {
        setHasApiKey(view.hasApiKey)
        setError('')
      },
      (failure: Error) => setError(failure.message),
    )
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
