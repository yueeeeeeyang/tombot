import { useLayoutEffect, useRef, useState } from 'react'

const bottomThreshold = 48
type ScrollMessage = { id: string; requestId?: string; role: string; status?: string }
const messageKey = (message?: ScrollMessage) => message?.requestId ?? message?.id

/** 在绘制前定位消息；阅读历史时保持锚点，自己的新消息和底部阅读才自动跟随。 */
export function useMessageScroll(messages: ScrollMessage[]) {
  const viewportRef = useRef<HTMLElement>(null)
  const contentRef = useRef<HTMLDivElement>(null)
  const isAtBottom = useRef(true)
  const returnFrame = useRef<number | undefined>(undefined)
  const isReturning = useRef(false)
  const [showScrollToBottom, setShowScrollToBottom] = useState(false)
  const previous = useRef<{ messages: ScrollMessage[]; height: number } | null>(null)

  function onScroll(): void {
    const element = viewportRef.current
    if (element && !isReturning.current) {
      isAtBottom.current =
        element.scrollHeight - element.scrollTop - element.clientHeight <= bottomThreshold
      setShowScrollToBottom(!isAtBottom.current)
    }
  }

  function scrollToBottom(): void {
    const element = viewportRef.current
    if (!element) return
    isAtBottom.current = true
    isReturning.current = true
    // 先终止引用定位产生的平滑滚动，再在按钮消失后的下一帧复核布局。
    element.scrollTo({ top: element.scrollHeight, behavior: 'instant' })
    setShowScrollToBottom(false)
    cancelAnimationFrame(returnFrame.current ?? 0)
    returnFrame.current = requestAnimationFrame(() => {
      element.scrollTo({ top: element.scrollHeight, behavior: 'instant' })
      isReturning.current = false
      returnFrame.current = undefined
    })
  }
  function pauseFollowing(): void {
    cancelAnimationFrame(returnFrame.current ?? 0)
    isReturning.current = false
    isAtBottom.current = false
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
    setShowScrollToBottom(
      element.scrollHeight - element.scrollTop - element.clientHeight > bottomThreshold,
    )
    previous.current = { messages, height: element.scrollHeight }
  }, [messages])

  useLayoutEffect(() => {
    const element = viewportRef.current
    const body = contentRef.current
    if (!element || !body) return
    // 图片、流式正文和输入框变高不一定新增消息，仍需维持底部阅读位置。
    const observer = new ResizeObserver(() => {
      if (isAtBottom.current) element.scrollTop = element.scrollHeight
      setShowScrollToBottom(
        element.scrollHeight - element.scrollTop - element.clientHeight > bottomThreshold,
      )
      if (previous.current) previous.current.height = element.scrollHeight
    })
    // 主动滚轮、触屏或键盘阅读优先于尚未完成的返回底部定位。
    const interrupt = () => {
      if (!isReturning.current) return
      cancelAnimationFrame(returnFrame.current ?? 0)
      isReturning.current = false
      isAtBottom.current = false
    }
    const onKey = (event: KeyboardEvent) => {
      if (['ArrowUp', 'PageUp', 'Home', 'PageDown', 'End', 'ArrowDown'].includes(event.key))
        interrupt()
    }
    element.addEventListener('wheel', interrupt, { passive: true })
    element.addEventListener('touchstart', interrupt, { passive: true })
    element.addEventListener('keydown', onKey)
    observer.observe(element)
    observer.observe(body)
    return () => {
      observer.disconnect()
      cancelAnimationFrame(returnFrame.current ?? 0)
      element.removeEventListener('wheel', interrupt)
      element.removeEventListener('touchstart', interrupt)
      element.removeEventListener('keydown', onKey)
    }
  }, [])

  return { viewportRef, contentRef, onScroll, showScrollToBottom, scrollToBottom, pauseFollowing }
}
