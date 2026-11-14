import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { CircleCheck, Info } from 'lucide-react'
import { AdminClient, type OperationMessage } from '../api/client'

type Message = OperationMessage & { id: number }
const MESSAGE_DURATION = 5_000
const MAX_MESSAGES = 3

/** 使用浏览器顶层显示操作结果，避免被尚未关闭的模态框遮挡；消息只保留在当前会话。 */
export function Messages({ client }: { client: AdminClient }) {
  const [messages, setMessages] = useState<Message[]>([])
  const layer = useRef<HTMLDivElement>(null)
  const sequence = useRef(0)
  useEffect(() => {
    const timers = new Map<number, ReturnType<typeof setTimeout>>()
    client.setMessenger((message) => {
      const id = ++sequence.current
      if (timers.size >= MAX_MESSAGES) {
        const first = timers.keys().next().value!
        clearTimeout(timers.get(first))
        timers.delete(first)
      }
      setMessages((previous) => [...previous.slice(-(MAX_MESSAGES - 1)), { ...message, id }])
      timers.set(
        id,
        setTimeout(() => {
          timers.delete(id)
          setMessages((previous) => previous.filter((item) => item.id !== id))
        }, MESSAGE_DURATION),
      )
    })
    return () => {
      client.setMessenger(null)
      timers.forEach(clearTimeout)
    }
  }, [client])
  useLayoutEffect(() => {
    const element = layer.current
    if (!element || !messages.length) return
    element.showPopover()
    return () => element.hidePopover()
  }, [messages])
  return createPortal(
    <div
      ref={layer}
      popover="manual"
      className="messages"
      aria-live="polite"
      aria-relevant="additions"
    >
      {messages.map((message) => (
        <div key={message.id} className={`message message-${message.tone}`} role="status">
          {message.tone === 'success' ? (
            <CircleCheck size={18} aria-hidden />
          ) : (
            <Info size={18} aria-hidden />
          )}
          <span>{message.text}</span>
        </div>
      ))}
    </div>,
    document.body,
  )
}
