import { fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { DEFAULT_CAT_MODEL_ID } from '../models/catalog'
import {
  saveMessage,
  toggleReaction,
  updateDraft,
  type Agent,
  type ChatMessage,
} from '../workspace/model'
import { ChatPanel } from './ChatPanel'

function createMessage(overrides: Partial<ChatMessage> = {}): ChatMessage {
  return {
    id: 'message-one',
    role: 'user',
    content: '你好',
    createdAt: '2026-11-10T08:00:00.000Z',
    status: 'unsent',
    reactions: [],
    ...overrides,
  }
}

function createAgent(messages: ChatMessage[] = [createMessage()], draft = ''): Agent {
  return {
    id: 'assistant',
    name: '小黑',
    description: '说明',
    group: '我的助手',
    avatar: 'bombay',
    modelId: DEFAULT_CAT_MODEL_ID,
    artifacts: [],
    computerUrl: '',
    messages,
    draft,
  }
}

function renderPanel(
  agent = createAgent(),
  overrides: Partial<Parameters<typeof ChatPanel>[0]> = {},
) {
  const props = {
    agent,
    isBlocked: false,
    onDraft: vi.fn(),
    onSave: vi.fn(() => true),
    onDelete: vi.fn(),
    onReact: vi.fn(),
    onRetry: vi.fn(),
    onDetails: vi.fn(),
    isDetailsOpen: false,
    onNotice: vi.fn(),
    onError: vi.fn(),
    ...overrides,
  }
  render(<ChatPanel {...props} />)
  return props
}

function StatefulPanel({ agent }: { agent: Agent }) {
  const [current, setCurrent] = useState(agent)
  return (
    <ChatPanel
      agent={current}
      isBlocked={false}
      onDraft={(draft) =>
        setCurrent(
          updateDraft(
            { version: 5, activeAgentId: current.id, leadAgentId: current.id, agents: [current] },
            current.id,
            draft,
          ).agents[0]!,
        )
      }
      onSave={(replyToId) => {
        const workspace = {
          version: 5 as const,
          activeAgentId: current.id,
          leadAgentId: current.id,
          agents: [current],
        }
        setCurrent(
          saveMessage(
            workspace,
            current.id,
            `saved-${current.messages.length}`,
            '2026-11-10T08:10:00.000Z',
            replyToId,
          ).agents[0]!,
        )
        return true
      }}
      onDelete={(messageId) =>
        setCurrent({
          ...current,
          messages: current.messages.filter((message) => message.id !== messageId),
        })
      }
      onReact={(messageId, emoji) =>
        setCurrent(
          toggleReaction(
            { version: 5, activeAgentId: current.id, leadAgentId: current.id, agents: [current] },
            current.id,
            messageId,
            emoji,
          ).agents[0]!,
        )
      }
      onRetry={vi.fn()}
      onDetails={vi.fn()}
      isDetailsOpen={false}
      onNotice={vi.fn()}
      onError={vi.fn()}
    />
  )
}

describe('会话顶部名称', () => {
  it('箭头朝右，宽度与显隐交给样式控制', () => {
    renderPanel()
    const chevron = screen.getByRole('button', { name: '查看小黑资料' }).querySelector('svg')
    expect(chevron).toHaveClass('pill-chevron', 'lucide-chevron-right')
    expect(chevron).toHaveAttribute('width', '14')
  })
})

describe('消息气泡操作', () => {
  it('用户和助手气泡都有朝向中央的快捷按钮，失败状态只保留图标', () => {
    renderPanel(
      createAgent([
        createMessage({ id: 'user', content: '用户说', status: 'failed' }),
        createMessage({
          id: 'bot',
          role: 'assistant',
          content: '助手说',
          status: 'sent',
          requestId: 'req-7',
          createdAt: '2026-11-10T08:10:00.000Z',
        }),
      ]),
    )
    const userRow = screen.getByText('用户说').closest('.bubble-row')!
    const assistantRow = screen.getByText('助手说').closest('.bubble-row')!
    expect(userRow.closest('.message-row')).toHaveClass('user-row')
    expect(assistantRow.closest('.message-row')).toHaveClass('assistant-row')
    expect(
      userRow
        .querySelector('.message-actions')!
        .compareDocumentPosition(userRow.querySelector('.message-bubble')!) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy()
    expect(
      assistantRow
        .querySelector('.message-bubble')!
        .compareDocumentPosition(assistantRow.querySelector('.message-actions')!) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy()
    expect(screen.getByRole('button', { name: '发送失败，点击重试' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '未发送，点击重试' })).not.toBeInTheDocument()
    expect(screen.queryByText(/· 未发送/)).not.toBeInTheDocument()
    expect(screen.getAllByRole('time')).toHaveLength(2)
  })

  it('表情胶囊渲染在用户和助手气泡内部', () => {
    renderPanel(
      createAgent([
        createMessage({
          id: 'user',
          content: '用户说',
          reactions: [{ emoji: '👍', count: 2 }],
        }),
        createMessage({
          id: 'bot',
          role: 'assistant',
          content: '助手说',
          status: 'sent',
          createdAt: '2026-11-10T08:10:00.000Z',
          reactions: [{ emoji: '❤️', count: 1 }],
        }),
      ]),
    )
    const userPill = screen.getByRole('button', { name: '👍 2，点击取消' })
    const assistantPill = screen.getByRole('button', { name: '❤️，点击取消' })
    expect(userPill.closest('.user-bubble')).toContainElement(screen.getByText('用户说'))
    expect(assistantPill.closest('.assistant-bubble')).toContainElement(screen.getByText('助手说'))
    expect(userPill.compareDocumentPosition(screen.getByText('用户说'))).toBe(
      Node.DOCUMENT_POSITION_PRECEDING,
    )
    expect(document.querySelector('.message-line > .reaction-pills')).toBeNull()
  })

  it('右键菜单包含表情和原有操作，没有请求标识时不显示复制请求 ID', async () => {
    const user = userEvent.setup()
    const props = renderPanel()
    fireEvent.contextMenu(screen.getByText('你好'), { clientX: 30, clientY: 40 })
    const menu = await screen.findByRole('menu', { name: '消息操作' })
    expect(within(menu).getByRole('button', { name: '回应👍' })).toBeInTheDocument()
    expect(within(menu).getByRole('menuitem', { name: '回复' })).toBeInTheDocument()
    expect(within(menu).getByRole('menuitem', { name: '复制' })).toBeInTheDocument()
    expect(within(menu).getByRole('menuitem', { name: '删除' })).toBeInTheDocument()
    expect(within(menu).getByRole('menuitem', { name: '重试' })).toBeInTheDocument()
    expect(within(menu).queryByRole('menuitem', { name: '复制请求 ID' })).not.toBeInTheDocument()
    await user.keyboard('{ArrowDown}')
    expect(within(menu).getByRole('button', { name: '回应👎' })).toHaveFocus()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    fireEvent.contextMenu(screen.getByText('你好'), { clientX: 30, clientY: 40 })
    fireEvent.pointerDown(document.body)
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '未发送，点击重试' }))
    expect(props.onRetry).toHaveBeenCalledWith('message-one')
  })

  it('菜单超出窗口时贴住边缘，Shift+F10 和更多按钮都能打开', () => {
    const previousSize = { width: window.innerWidth, height: window.innerHeight }
    Object.defineProperty(window, 'innerWidth', { configurable: true, value: 100 })
    Object.defineProperty(window, 'innerHeight', { configurable: true, value: 100 })
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
      x: 0,
      y: 0,
      top: 0,
      left: 0,
      right: 280,
      bottom: 360,
      width: 280,
      height: 360,
      toJSON: () => ({}),
    })
    renderPanel()
    fireEvent.contextMenu(screen.getByText('你好'), { clientX: 9999, clientY: 9999 })
    const menu = screen.getByRole('menu', { name: '消息操作' })
    expect(menu.style.left).toBe('8px')
    expect(menu.style.top).toBe('8px')
    Object.defineProperty(window, 'innerWidth', { configurable: true, value: previousSize.width })
    Object.defineProperty(window, 'innerHeight', { configurable: true, value: previousSize.height })
    fireEvent.keyDown(menu, { key: 'Escape' })
    fireEvent.keyDown(screen.getByRole('button', { name: '更多操作' }), {
      key: 'F10',
      shiftKey: true,
    })
    expect(screen.getByRole('menu', { name: '消息操作' })).toBeInTheDocument()
  })

  it('复制成功和失败都会告知用户，请求标识只在存在时出现', async () => {
    const user = userEvent.setup()
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    const props = renderPanel(createAgent([createMessage({ requestId: 'req-9', status: 'sent' })]))
    fireEvent.contextMenu(screen.getByText('你好'), { clientX: 12, clientY: 12 })
    await user.click(screen.getByRole('menuitem', { name: '复制请求 ID' }))
    expect(writeText).toHaveBeenCalledWith('req-9')
    expect(props.onNotice).toHaveBeenCalledWith('已复制请求 ID')
    expect(screen.queryByRole('menuitem', { name: '重试' })).not.toBeInTheDocument()
    writeText.mockRejectedValueOnce(new Error('拒绝'))
    fireEvent.contextMenu(screen.getByText('你好'), { clientX: 12, clientY: 12 })
    await user.click(screen.getByRole('menuitem', { name: '复制' }))
    expect(props.onError).toHaveBeenCalledWith('复制失败，请检查剪贴板权限后重试。')
  })

  it('回复可以取消；保存失败时保留引用，成功后显示可跳转的摘要', async () => {
    const user = userEvent.setup()
    const failed = renderPanel(createAgent([createMessage()], '接着说'), {
      onSave: vi.fn(() => false),
    })
    await user.click(screen.getByRole('button', { name: '回复' }))
    expect(screen.getByText('回复').closest('.composer-reply')).toHaveTextContent('你好')
    await user.click(screen.getByRole('button', { name: '发送' }))
    expect(screen.getByText('回复').closest('.composer-reply')).toBeInTheDocument()
    expect(failed.onSave).toHaveBeenCalledWith('message-one', [])
    await user.click(screen.getByRole('button', { name: '取消回复' }))
    expect(screen.queryByText('回复')).not.toBeInTheDocument()
  })

  it('保存回复后可以点摘要回到原消息', async () => {
    const user = userEvent.setup()
    render(
      <StatefulPanel agent={createAgent([createMessage({ content: '原来的想法' })], '接着说')} />,
    )
    await user.click(screen.getAllByRole('button', { name: '回复' })[0]!)
    await user.click(screen.getAllByRole('button', { name: '发送' })[0]!)
    const quote = screen.getByRole('button', { name: '查看原消息：原来的想法' })
    await user.click(quote)
    expect(Element.prototype.scrollIntoView).toHaveBeenCalled()
    expect(document.getElementById('message-message-one')).toHaveClass('is-highlighted')
  })

  it('数量大于 1 时显示计数，原消息不在时说明原因', async () => {
    const user = userEvent.setup()
    const onError = vi.fn()
    render(
      <ChatPanel
        agent={createAgent([
          createMessage({
            id: 'kept',
            content: '留下',
            reactions: [{ emoji: '👍', count: 2 }],
          }),
          createMessage({
            id: 'reply',
            content: '引用丢了',
            createdAt: '2026-11-10T08:02:00.000Z',
            replyTo: { messageId: 'missing', excerpt: '已经删除' },
          }),
        ])}
        isBlocked={false}
        onDraft={vi.fn()}
        onSave={vi.fn(() => true)}
        onDelete={vi.fn()}
        onReact={vi.fn()}
        onRetry={vi.fn()}
        onDetails={vi.fn()}
        isDetailsOpen={false}
        onNotice={vi.fn()}
        onError={onError}
      />,
    )
    const pill = screen.getByRole('button', { name: '👍 2，点击取消' })
    expect(pill).toHaveTextContent('2')
    expect(screen.getAllByRole('time')).toHaveLength(1)
    await user.click(screen.getByRole('button', { name: '查看原消息：已经删除' }))
    expect(onError).toHaveBeenCalledWith('原消息已不在当前会话中。')
  })

  it('表情可以加上再取消，删除后消息消失', async () => {
    const user = userEvent.setup()
    render(<StatefulPanel agent={createAgent()} />)
    fireEvent.contextMenu(screen.getAllByText('你好')[0]!, { clientX: 8, clientY: 8 })
    await user.click(screen.getByRole('button', { name: '回应❤️' }))
    const added = screen.getByRole('button', { name: '❤️，点击取消' })
    await user.click(added)
    expect(screen.queryByRole('button', { name: '❤️，点击取消' })).not.toBeInTheDocument()
    await user.click(screen.getAllByRole('button', { name: '更多操作' })[0]!)
    await user.click(screen.getByRole('menuitem', { name: '删除' }))
    expect(screen.queryByText('你好')).not.toBeInTheDocument()
  })
})
