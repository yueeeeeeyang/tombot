import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { createWorkspace } from '../workspace/model'
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
    expect(screen.getByRole('status')).toHaveTextContent('还没有自己的电脑')
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
      <CatSidePanel agent={agent} onClose={onClose} onModelChange={vi.fn()} onExport={vi.fn()} />,
    )
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    expect(onClose).toHaveBeenCalledTimes(1)
  })
})
