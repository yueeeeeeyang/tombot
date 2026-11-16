import { useEffect, useRef, useState } from 'react'
import { getBackend, type UserProfile } from '../desktop/backend'

/** 启动和返回客户端时读取本人资料；网络故障保留最近成功的资料并明确提示。 */
export function useUserProfile() {
  const backend = getBackend()
  const [profile, setProfile] = useState<UserProfile>()
  const [error, setError] = useState('')
  const dismissedError = useRef('')
  useEffect(() => {
    let disposed = false
    let loading = false
    let needsRefresh = false
    async function refresh(): Promise<void> {
      if (loading) {
        needsRefresh = true
        return
      }
      loading = true
      try {
        do {
          needsRefresh = false
          const latest = await backend.loadUserProfile()
          if (disposed) return
          setProfile(latest)
          dismissedError.current = ''
          setError('')
        } while (needsRefresh)
      } catch (failure) {
        if (!disposed) {
          const message = `读取用户资料失败：${(failure as Error).message}`
          setError(dismissedError.current === message ? '' : message)
        }
      } finally {
        loading = false
      }
    }
    const onFocus = () => {
      void refresh()
    }
    const onVisibilityChange = () => {
      if (document.visibilityState === 'visible') void refresh()
    }
    void refresh()
    const stopWatchingFocus = backend.watchWindowFocus(onFocus)
    window.addEventListener('online', onFocus)
    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => {
      disposed = true
      stopWatchingFocus()
      window.removeEventListener('online', onFocus)
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }, [backend])
  return {
    profile,
    error,
    dismissError: () => {
      // 相同故障的焦点刷新与迟到回包不重新弹出；成功恢复后出现的新故障仍提示。
      dismissedError.current = error || dismissedError.current
      setError('')
    },
  }
}
