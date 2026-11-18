import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { chatFilePath } from '../../../../shared/chatFiles'
import { getBackend } from '../desktop/backend'
import { ChatImageCache } from './chatImageCache'

const ChatImagePreviewContext = createContext<ChatImageCache | null>(null)

export function useChatImageCache(): ChatImageCache | null {
  return useContext(ChatImagePreviewContext)
}

/** 缓存在同一登录工作区的会话之间共享，随工作区销毁。 */
export function ChatImagePreviewProvider({ children }: { children: ReactNode }) {
  const [cache] = useState(() => new ChatImageCache())
  useEffect(() => {
    const stop = getBackend().watchEvents?.(
      (event) => {
        if (event.kind === 'workspace.refresh') cache.clear()
        if (event.kind === 'chat.library.changed') {
          const { conversation_id, file_id } = event.payload
          if (typeof conversation_id === 'string' && typeof file_id === 'string')
            cache.invalidate(chatFilePath({ conversation_id, id: file_id }))
        }
      },
      () => cache.clear(),
    )
    return () => {
      stop?.()
      cache.clear()
    }
  }, [cache])
  return <ChatImagePreviewContext value={cache}>{children}</ChatImagePreviewContext>
}
