import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { createWorkspace, type Artifact } from '../workspace/model'
import { CatSidePanel } from './CatSidePanel'

const agent = createWorkspace().agents[0]!

describe('猫咪侧栏面板', () => {
  it('详情展示名称、介绍、分组和模型，另外两个栏目是空状态', async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    render(<CatSidePanel agent={agent} onClose={onClose} onComputerUrl={vi.fn()} />)
    expect(screen.getByRole('heading', { name: '小黑' })).toHaveTextContent('小黑')
    expect(await screen.findByText('相处久一点，会更了解彼此。')).toBeInTheDocument()
    expect(screen.queryByText(agent.description)).toBeNull()
    expect(screen.queryByRole('tab', { name: '记忆' })).toBeNull()
    expect(screen.getAllByRole('tab')).toHaveLength(4)
    expect(screen.getByText('我的助手')).toBeInTheDocument()
    expect(screen.getByText('由服务端管理')).toBeInTheDocument()
    expect(screen.queryByRole('combobox', { name: '模型' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: '资料库' }))
    expect(screen.getByRole('status')).toHaveTextContent('资料库还是空的')
    await user.keyboard('{ArrowRight}')
    expect(screen.getByRole('tab', { name: '电脑', selected: true })).toHaveFocus()
    expect(screen.getByRole('status')).toHaveTextContent('未连接')
    expect(screen.getByLabelText('远端电脑地址')).toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: '详情' }))
    expect(screen.getByText('自动使用管理员分配的模型策略，无需在客户端配置。')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '导出草稿' })).not.toBeInTheDocument()
    const close = screen.getByRole('button', { name: '关闭猫咪面板' })
    expect(close).toHaveClass('round-button')
    await user.click(close)
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('按下 Esc 关闭面板', () => {
    const onClose = vi.fn()
    render(<CatSidePanel agent={agent} onClose={onClose} onComputerUrl={vi.fn()} />)
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('资料库默认只显示每组前 5 条，展开后能看到其余产物', async () => {
    const user = userEvent.setup()
    const now = new Date(2026, 10, 10, 18, 0, 0)
    const artifacts: Artifact[] = Array.from({ length: 6 }, (_, index) => ({
      id: `today-${index}`,
      title: `今天的文件 ${index + 1}`,
      kind: 'file',
      createdAt: new Date(2026, 10, 10, 10 + index, 0, 0).toISOString(),
    }))
    artifacts.push({
      id: 'week',
      title: '本周笔记',
      kind: 'video',
      createdAt: new Date(2026, 10, 9, 12, 0, 0).toISOString(),
    })
    render(
      <CatSidePanel
        agent={{ ...agent, artifacts }}
        onClose={vi.fn()}
        onComputerUrl={vi.fn()}
        now={now}
      />,
    )
    await user.click(screen.getByRole('tab', { name: '资料库' }))
    const today = screen.getByRole('region', { name: '今天' })
    const heading = within(today).getByRole('heading', { name: '今天' })
    expect(heading.closest('.artifact-card')).toBeNull()
    expect(heading.nextElementSibling).toHaveClass('artifact-card')
    expect(today.querySelector('.artifact-card button')).toHaveTextContent('展开')
    expect(screen.queryByText('今天的文件 1')).not.toBeInTheDocument()
    expect(screen.getByText('今天的文件 6')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '展开' }))
    expect(screen.getByText('今天的文件 1')).toBeInTheDocument()
    expect(screen.getByText('本周笔记')).toBeInTheDocument()
  })
})
