import { useLayoutEffect, useRef } from 'react'
import type { ChatMessage } from '../workspace/model'

const bottomThreshold = 48
const messageKey = (message?: ChatMessage) => message?.requestId ?? message?.id

/** 在绘制前定位消息；阅读历史时保持锚点，自己的新消息和底部阅读才自动跟随。 */
export function useMessageScroll(messages: ChatMessage[]) {
  const viewportRef = useRef<HTMLElement>(null)
  const contentRef = useRef<HTMLDivElement>(null)
  const isAtBottom = useRef(true)
  const previous = useRef<{ messages: ChatMessage[]; height: number } | null>(null)

  function onScroll(): void {
    const element = viewportRef.current
    if (element)
      isAtBottom.current =
        element.scrollHeight - element.scrollTop - element.clientHeight <= bottomThreshold
  }

  useLayoutEffect(() => {
    const element = viewportRef.current
    if (!element) return
    const old = previous.current
    const last = messages.at(-1)
    const isOwnNewMessage =
      last?.role === 'user' &&
      last.status === 'unsent' &&
      !old?.messages.some((message) => messageKey(message) === messageKey(last))
    if (isAtBottom.current || isOwnNewMessage) {
      element.scrollTop = element.scrollHeight
      isAtBottom.current = true
    } else if (
      old &&
      messageKey(old.messages[0]) !== messageKey(messages[0]) &&
      messages.some((message) => messageKey(message) === messageKey(old.messages[0]))
    ) {
      element.scrollTop += element.scrollHeight - old.height
    }
    previous.current = { messages, height: element.scrollHeight }
  }, [messages])

  useLayoutEffect(() => {
    const element = viewportRef.current
    const body = contentRef.current
    if (!element || !body) return
    // 图片、流式正文和输入框变高不一定新增消息，仍需维持底部阅读位置。
    const observer = new ResizeObserver(() => {
      if (isAtBottom.current) element.scrollTop = element.scrollHeight
      if (previous.current) previous.current.height = element.scrollHeight
    })
    observer.observe(element)
    observer.observe(body)
    return () => observer.disconnect()
  }, [])

  return { viewportRef, contentRef, onScroll }
}
