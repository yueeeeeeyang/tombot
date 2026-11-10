import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { createWorkspace, type Artifact } from '../workspace/model'
import { CatSidePanel } from './CatSidePanel'

const agent = createWorkspace().agents[0]!

describe('猫咪侧栏面板', () => {
  it('详情展示名称、介绍、分组和模型，另外两个栏目是空状态', async () => {
    const user = userEvent.setup()
    const onModelChange = vi.fn()
    const onClose = vi.fn()
    const onExport = vi.fn()
    render(
      <CatSidePanel
        agent={agent}
        onClose={onClose}
        onModelChange={onModelChange}
        onComputerUrl={vi.fn()}
        onExport={onExport}
      />,
    )
    expect(screen.getByRole('heading', { name: '小黑' })).toHaveTextContent('小黑')
    expect(screen.getByText('你的日常搭档，陪你把想法变成现实。')).toBeInTheDocument()
    expect(screen.getByText('我的助手')).toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: '模型' })).toHaveTextContent('GPT-4o')
    await user.click(screen.getByRole('tab', { name: '资料库' }))
    expect(screen.getByRole('status')).toHaveTextContent('资料库还是空的')
    await user.keyboard('{ArrowRight}')
    expect(screen.getByRole('tab', { name: '电脑', selected: true })).toHaveFocus()
    expect(screen.getByRole('status')).toHaveTextContent('未连接')
    expect(screen.getByLabelText('远端电脑地址')).toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: '详情' }))
    await user.click(screen.getByRole('combobox', { name: '模型' }))
    await user.click(screen.getByRole('option', { name: 'Claude Sonnet' }))
    expect(onModelChange).toHaveBeenCalledWith('claude-sonnet')
    await user.click(screen.getByRole('button', { name: '导出草稿' }))
    expect(onExport).toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: '关闭猫咪面板' }))
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('按下 Esc 关闭面板', () => {
    const onClose = vi.fn()
    render(
      <CatSidePanel
        agent={agent}
        onClose={onClose}
        onModelChange={vi.fn()}
        onComputerUrl={vi.fn()}
        onExport={vi.fn()}
      />,
    )
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
        onModelChange={vi.fn()}
        onComputerUrl={vi.fn()}
        onExport={vi.fn()}
        now={now}
      />,
    )
    await user.click(screen.getByRole('tab', { name: '资料库' }))
    expect(screen.getByRole('region', { name: '今天' })).toBeInTheDocument()
    expect(screen.queryByText('今天的文件 1')).not.toBeInTheDocument()
    expect(screen.getByText('今天的文件 6')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '展开' }))
    expect(screen.getByText('今天的文件 1')).toBeInTheDocument()
    expect(screen.getByText('本周笔记')).toBeInTheDocument()
  })
})
