import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import type { ChatMessage } from '../workspace/model'
import { useMessageScroll } from './useMessageScroll'

function message(id: string, overrides: Partial<ChatMessage> = {}): ChatMessage {
  return {
    id,
    role: 'assistant',
    content: id,
    createdAt: '2026-11-16T01:00:00Z',
    status: 'sent',
    reactions: [],
    ...overrides,
  }
}
function Timeline({ messages }: { messages: ChatMessage[] }) {
  const { viewportRef, contentRef, onScroll, showScrollToBottom, scrollToBottom } =
    useMessageScroll(messages)
  return (
    <section ref={viewportRef} onScroll={onScroll} aria-label="会话">
      {showScrollToBottom && <button onClick={scrollToBottom}>回到最新消息</button>}
      <div ref={contentRef}>
        {messages.map((item) => (
          <div key={item.requestId ?? item.id}>{item.content}</div>
        ))}
      </div>
    </section>
  )
}
afterEach(() => vi.unstubAllGlobals())

function setup() {
  let notifyResize!: () => void
  const disconnect = vi.fn()
  vi.stubGlobal(
    'ResizeObserver',
    class {
      constructor(callback: () => void) {
        notifyResize = callback
      }
      observe = vi.fn()
      disconnect = disconnect
    },
  )
  const messages = [message('first'), message('last')]
  const rendered = render(<Timeline messages={messages} />)
  const element = screen.getByRole('region', { name: '会话' })
  let height = 1000
  let top = 600
  Object.defineProperties(element, {
    scrollHeight: { get: () => height },
    clientHeight: { get: () => 400 },
    scrollTop: {
      get: () => top,
      set: (value: number) => {
        top = Math.min(value, height - 400)
      },
    },
  })
  act(() => notifyResize())
  return {
    ...rendered,
    messages,
    element,
    resize: () => act(() => notifyResize()),
    disconnect,
    setHeight: (value: number) => {
      height = value
    },
  }
}

it('阅读历史时收到新消息和正文变高保持位置，自己发送才回到底部', () => {
  const { element, messages, setHeight, rerender, resize } = setup()
  element.scrollTop = 120
  fireEvent.scroll(element)
  setHeight(1100)
  rerender(<Timeline messages={[...messages, message('reply')]} />)
  resize()
  expect(element.scrollTop).toBe(120)
  const outgoing = message('local', { role: 'user', status: 'unsent', requestId: 'local' })
  rerender(<Timeline messages={[...messages, outgoing]} />)
  expect(element.scrollTop).toBe(700)
  // 发送回执更改服务端标识时不能再次把已上翻的读者拉回底部。
  element.scrollTop = 100
  fireEvent.scroll(element)
  rerender(<Timeline messages={[...messages, { ...outgoing, id: 'server', status: 'sent' }]} />)
  expect(element.scrollTop).toBe(100)
})

it('前插历史保留锚点，底部读者跟随图片和视口变化，卸载清理监听', () => {
  const { element, messages, setHeight, rerender, resize, unmount, disconnect } = setup()
  element.scrollTop = 100
  fireEvent.scroll(element)
  setHeight(1300)
  rerender(<Timeline messages={[message('older'), ...messages]} />)
  expect(element.scrollTop).toBe(400)
  element.scrollTop = 900
  fireEvent.scroll(element)
  setHeight(1500)
  resize()
  expect(element.scrollTop).toBe(1100)
  unmount()
  expect(disconnect).toHaveBeenCalledOnce()
})

it('滚动离开底部才出现按钮，返回底部后自动跟随新消息', () => {
  const { element, messages, setHeight, rerender } = setup()
  expect(screen.queryByRole('button', { name: '回到最新消息' })).toBeNull()
  element.scrollTop = 120
  fireEvent.scroll(element)
  fireEvent.click(screen.getByRole('button', { name: '回到最新消息' }))
  expect(element.scrollTop).toBe(600)
  expect(screen.queryByRole('button', { name: '回到最新消息' })).toBeNull()
  setHeight(1200)
  rerender(<Timeline messages={[...messages, message('new')]} />)
  expect(element.scrollTop).toBe(800)
  element.scrollTop = 750
  fireEvent.scroll(element)
  expect(screen.getByRole('button', { name: '回到最新消息' })).toBeInTheDocument()
  element.scrollTop = 760
  fireEvent.scroll(element)
  expect(screen.queryByRole('button', { name: '回到最新消息' })).toBeNull()
})
