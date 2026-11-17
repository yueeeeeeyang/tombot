import { taskFixture } from '../../../../shared/tasks.fixture'
import { render, screen, within, waitFor, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { createWorkspace, type Artifact } from '../workspace/model'
import { CatSidePanel } from './CatSidePanel'
import { memoryBackend } from '../desktop/memoryBackend'

const agent = createWorkspace().agents[0]!

describe('猫咪侧栏面板', () => {
  it('动态内展示电脑和任务卡片，仅保留动态和资料库页签', async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    render(<CatSidePanel agent={agent} onClose={onClose} onComputerUrl={vi.fn()} />)
    expect(screen.getByRole('heading', { name: '小黑' })).toHaveTextContent('小黑')
    expect(screen.queryByText('介绍')).not.toBeInTheDocument()
    expect(screen.queryByText('相处久一点，会更了解彼此。')).not.toBeInTheDocument()
    expect(screen.queryByText(agent.description)).toBeNull()
    expect(screen.queryByRole('tab', { name: '记忆' })).toBeNull()
    expect(screen.getAllByRole('tab')).toHaveLength(2)
    expect(screen.queryByRole('tab', { name: '电脑' })).not.toBeInTheDocument()
    expect(screen.queryByRole('tab', { name: '任务' })).not.toBeInTheDocument()
    expect(
      within(screen.getByRole('region', { name: '电脑' })).getByRole('status'),
    ).toHaveTextContent('未连接')
    expect(
      await within(screen.getByRole('region', { name: '任务' })).findByText('还没有任务'),
    ).toBeInTheDocument()
    expect(screen.getAllByRole('tabpanel')).toHaveLength(1)
    expect(screen.queryByText('名称')).not.toBeInTheDocument()
    expect(screen.queryByText('分组')).not.toBeInTheDocument()
    expect(screen.queryByText('我的助手')).not.toBeInTheDocument()
    expect(screen.queryByText('模型策略')).not.toBeInTheDocument()
    expect(screen.queryByText('由服务端管理')).not.toBeInTheDocument()
    expect(screen.queryByRole('combobox', { name: '模型' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: '资料库' }))
    expect(screen.getByRole('status')).toHaveTextContent('资料库还是空的')
    await user.keyboard('{ArrowRight}')
    expect(screen.getByRole('tab', { name: '动态', selected: true })).toHaveFocus()
    await user.click(screen.getByRole('button', { name: '小黑的电脑' }))
    expect(screen.getByLabelText('远端电脑地址')).toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: '动态' }))
    expect(screen.queryByText(/自动使用管理员分配的模型策略/)).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '导出草稿' })).not.toBeInTheDocument()
    const close = screen.getByRole('button', { name: '关闭猫咪面板' })
    expect(close).toHaveClass('round-button')
    await user.click(close)
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('动态重绘不重复请求任务，切换猫咪后读取新数据且忽略旧回包', async () => {
    let finish!: (value: []) => void
    const load = vi
      .spyOn(memoryBackend, 'listTasks')
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            finish = resolve
          }),
      )
      .mockResolvedValue([
        {
          ...taskFixture,
          id: 'new-task',
          name: '新的提醒',
          cron: '0 9 * * *',
          prompt: '提醒工作',
          is_enabled: true,
          revision: 1,
        },
      ])
    const view = render(<CatSidePanel agent={agent} onClose={vi.fn()} onComputerUrl={vi.fn()} />)
    await waitFor(() => expect(load).toHaveBeenCalledTimes(1))
    view.rerender(
      <CatSidePanel
        agent={{ ...agent, name: '新名称' }}
        onClose={vi.fn()}
        onComputerUrl={vi.fn()}
      />,
    )
    expect(load).toHaveBeenCalledTimes(1)
    view.rerender(
      <CatSidePanel
        agent={{ ...agent, id: 'another-cat' }}
        onClose={vi.fn()}
        onComputerUrl={vi.fn()}
      />,
    )
    expect(await screen.findByRole('switch', { name: '禁用新的提醒' })).toBeInTheDocument()
    finish([])
    await waitFor(() => expect(screen.getByText('新的提醒')).toBeInTheDocument())
    expect(load).toHaveBeenLastCalledWith('another-cat')
  })

  it('动态里的任务请求失败时显示错误卡片', async () => {
    vi.spyOn(memoryBackend, 'listTasks').mockRejectedValue(new Error('定时读取失败'))
    render(<CatSidePanel agent={agent} onClose={vi.fn()} onComputerUrl={vi.fn()} />)
    expect(
      await within(screen.getByRole('region', { name: '任务' })).findByRole('alert'),
    ).toHaveTextContent('定时读取失败')
    expect(screen.queryByText('还没有任务')).not.toBeInTheDocument()
  })

  it('任务菜单先处理 Esc，再次按下才关闭整个面板', async () => {
    const user = userEvent.setup()
    const close = vi.fn()
    vi.spyOn(memoryBackend, 'listTasks').mockResolvedValue([
      {
        ...taskFixture,
        id: 'task',
        name: '晨跑提醒',
        cron: '0 9 * * *',
        prompt: '提醒晨跑',
        is_enabled: true,
        revision: 1,
      },
    ])
    render(<CatSidePanel agent={agent} onClose={close} onComputerUrl={vi.fn()} />)
    fireEvent.contextMenu(await screen.findByText('晨跑提醒'))
    expect(await screen.findByRole('menu', { name: '任务' })).toBeInTheDocument()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    expect(close).not.toHaveBeenCalled()
    await user.keyboard('{Escape}')
    expect(close).toHaveBeenCalledOnce()
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
