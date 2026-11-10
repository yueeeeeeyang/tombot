import { render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { DesktopBackend } from '../desktop/backend'
import { createWorkspace } from '../workspace/model'
import { DmThreadDialog } from './ConversationPanel'

afterEach(() => {
  delete window.desktop
})

describe('私信只读气泡', () => {
  it('按时间用双方气泡展示，不能回复', async () => {
    const workspace = createWorkspace()
    const self = workspace.agents[0]
    const peer = workspace.agents.find((agent) => agent.id === 'developer')
    if (!self || !peer) throw new Error('缺少测试猫咪')
    const backend: Partial<DesktopBackend> = {
      listConversationMessages: vi.fn(async () => [
        {
          id: 'newer',
          role: 'assistant' as const,
          speaker: peer.id,
          content: '好的',
          createdAt: '2026-11-10T08:01:00.000Z',
          reactions: [],
          attachments: [],
        },
        {
          id: 'older',
          role: 'assistant' as const,
          speaker: self.id,
          content: '晨会我先看了',
          createdAt: '2026-11-10T08:00:00.000Z',
          reactions: [],
          attachments: [],
        },
      ]),
    }
    window.desktop = { platform: 'linux', backend: backend as DesktopBackend }
    const onClose = vi.fn()
    render(
      <DmThreadDialog
        notice={{
          direction: 'sent',
          peerId: peer.id,
          peerName: peer.name,
          conversationId: 'dm-1',
        }}
        self={self}
        agents={workspace.agents}
        onClose={onClose}
        onError={() => undefined}
      />,
    )
    const dialog = await screen.findByRole('dialog', { name: '小黑 ↔ 游戏程序' })
    const bubbles = dialog.querySelectorAll('.dm-bubble')
    expect(bubbles).toHaveLength(2)
    expect(bubbles[0]).toHaveTextContent('晨会我先看了')
    expect(bubbles[1]).toHaveTextContent('好的')
    expect(dialog.querySelector('.user-bubble, .assistant-bubble, .dialog-heading')).toBeNull()
    expect(dialog.querySelectorAll('.dm-line--peer')).toHaveLength(0)
    expect(dialog.querySelector('.dm-capsule')).toHaveTextContent('小黑')
    expect(dialog.querySelector('.dm-capsule')).toHaveTextContent('游戏程序')
    expect(dialog.querySelector('.dm-name--self')).toHaveTextContent('小黑')
    expect(dialog.querySelector('.dm-name--peer')).toHaveTextContent('游戏程序')
    expect(dialog.querySelector('form, textarea')).toBeNull()
    expect(within(dialog).queryByRole('button', { name: '回复' })).not.toBeInTheDocument()
    expect(within(dialog).queryByRole('button', { name: '发送' })).not.toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: '关闭弹窗' })).toBeInTheDocument()
  })
})
