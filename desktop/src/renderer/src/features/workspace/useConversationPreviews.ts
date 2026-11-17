import { useEffect, useState } from 'react'
import { getBackend, type ConversationPreview } from '../desktop/backend'

/** 启动、焦点恢复和消息事件统一刷新摘要；串行合并请求，旧会话回包不污染新目录。 */
export function useConversationPreviews(
  conversationIds: string[],
  onError: (message: string) => void,
) {
  const [previews, setPreviews] = useState<Record<string, ConversationPreview>>({})
  const key = JSON.stringify([...new Set(conversationIds)].filter(Boolean).sort())
  useEffect(() => {
    const ids: string[] = JSON.parse(key)
    if (!ids.length) return
    const backend = getBackend()
    let cancelled = false
    let running = false
    let requested = false
    let debounce: ReturnType<typeof setTimeout> | undefined
    async function refresh() {
      if (running) {
        requested = true
        return
      }
      running = true
      do {
        requested = false
        try {
          const received: ConversationPreview[] = []
          for (let index = 0; index < ids.length && !cancelled; index += 100) {
            received.push(
              ...(await backend.loadConversationPreviews(ids.slice(index, index + 100))),
            )
          }
          if (!cancelled)
            setPreviews((current) => {
              const next = { ...current }
              for (const preview of received) {
                if ((next[preview.conversation_id]?.sequence ?? -1) <= preview.sequence)
                  next[preview.conversation_id] = preview
              }
              return next
            })
        } catch (failure) {
          if (!cancelled)
            onError(`读取聊天摘要失败：${failure instanceof Error ? failure.message : '请重试。'}`)
        }
      } while (requested && !cancelled)
      running = false
    }
    const schedule = () => {
      clearTimeout(debounce)
      debounce = setTimeout(() => void refresh(), 150)
    }
    void refresh()
    const stop = backend.watchEvents?.(
      (event) => {
        if (
          event.kind === 'workspace.refresh' ||
          (event.kind === 'message.created' && ids.includes(String(event.payload.conversation_id)))
        )
          schedule()
      },
      (failure) => {
        if (!cancelled) onError(failure.message)
      },
    )
    const timer = setInterval(() => void refresh(), 10000)
    window.addEventListener('focus', schedule)
    return () => {
      cancelled = true
      clearTimeout(debounce)
      clearInterval(timer)
      stop?.()
      window.removeEventListener('focus', schedule)
    }
  }, [key, onError])
  return previews
}
