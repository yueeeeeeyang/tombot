import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { getAgentActivity } from '../avatars/activity'
import { WorkStatus } from './WorkStatus'

afterEach(() => vi.restoreAllMocks())

describe('工作中的猫咪头像', () => {
  it('跟随当前猫咪，保留完整摘要，不修改侧栏活动状态', () => {
    const view = render(<WorkStatus agents={[{ id: 'cat-a', avatar: 'calico' }]} />)
    const status = screen.getByRole('status')
    expect(status).toHaveTextContent('正在思考…')
    expect(status.querySelector('.avatar')).toHaveAttribute('data-avatar', 'calico')
    expect(status.querySelector('.avatar')).toHaveAttribute('data-pose', 'working')
    expect(status.querySelector('.cat-work-mark')).not.toBeNull()
    expect(getAgentActivity('cat-a')).toBe('idle')
    view.rerender(
      <WorkStatus agents={[{ id: 'cat-b', avatar: 'ragdoll' }]} summary="正在核对资料" />,
    )
    expect(status).toHaveTextContent('正在核对资料')
    expect(status.querySelector('.avatar')).toHaveAttribute('data-avatar', 'ragdoll')
    view.unmount()
    expect(getAgentActivity('cat-b')).toBe('idle')
  })

  it('群聊最多展示三个头像，更多成员只显示数量', () => {
    render(
      <WorkStatus
        agents={Array.from({ length: 6 }, (_, index) => ({
          id: `cat-${index}`,
          avatar: 'bombay' as const,
        }))}
      />,
    )
    expect(screen.getByRole('status').querySelectorAll('.avatar')).toHaveLength(3)
    expect(screen.getByText('+3')).toBeInTheDocument()
  })

  it('减弱动效时头像静止，摘要仍可阅读', () => {
    const media = window.matchMedia('(prefers-reduced-motion: reduce)')
    vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
      ...media,
      media: query,
      matches: true,
    }))
    render(<WorkStatus agents={[{ id: 'cat-c', avatar: 'siamese' }]} summary="正在整理结论" />)
    expect(screen.getByRole('status').querySelector('.avatar')).toHaveAttribute('data-pose', 'idle')
    expect(screen.getByRole('status').querySelector('.cat-work-mark')).toBeNull()
    expect(screen.getByRole('status')).toHaveTextContent('正在整理结论')
  })
})
