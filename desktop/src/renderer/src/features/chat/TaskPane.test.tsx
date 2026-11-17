import { fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { TaskPane } from './TaskPane'
import { taskFixture } from '../../../../shared/tasks.fixture'

const task = taskFixture

describe('任务面板', () => {
  it('只有启用开关，右键菜单可以删除', async () => {
    const user = userEvent.setup()
    const onToggle = vi.fn(async () => undefined)
    const onDelete = vi.fn(async () => undefined)
    render(
      <TaskPane
        agentId="assistant"
        onLoad={async () => [task]}
        onToggle={onToggle}
        onDelete={onDelete}
      />,
    )
    expect(screen.queryByRole('button', { name: '添加任务' })).not.toBeInTheDocument()
    await user.click(await screen.findByRole('switch', { name: '禁用晨跑提醒' }))
    expect(onToggle).toHaveBeenCalledWith(task)
    fireEvent.contextMenu(screen.getByText('晨跑提醒'))
    await user.click(await screen.findByRole('menuitem', { name: '删除' }))
    expect(onDelete).toHaveBeenCalledWith(task)
  })

  it('没有任务时展示空状态', async () => {
    render(
      <TaskPane
        agentId="assistant"
        onLoad={async () => []}
        onToggle={async () => undefined}
        onDelete={async () => undefined}
      />,
    )
    expect(await screen.findByText('还没有任务')).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveClass('cat-panel-empty')
  })

  it('读取失败时显示错误，不把失败展示为空任务', async () => {
    const view = render(
      <TaskPane
        agentId="assistant"
        onLoad={async () => {
          throw new Error('任务读取失败')
        }}
        onToggle={async () => undefined}
        onDelete={async () => undefined}
      />,
    )
    expect(await screen.findByRole('alert')).toHaveTextContent('任务读取失败')
    expect(screen.queryByText('还没有任务')).toBeNull()
    view.rerender(
      <TaskPane
        agentId="assistant"
        onLoad={async () => []}
        onToggle={async () => undefined}
        onDelete={async () => undefined}
      />,
    )
    expect(await screen.findByText('还没有任务')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

it('对话事件刷新任务并显示执行结果，卸载后取消订阅', async () => {
  let changed!: () => void
  const unwatch = vi.fn()
  const onLoad = vi
    .fn()
    .mockResolvedValueOnce([])
    .mockResolvedValueOnce([
      { ...task, last_run_at: '2026-11-16T01:00:00Z', last_status: 'succeeded' },
    ])
  const view = render(
    <TaskPane
      agentId="assistant"
      onLoad={onLoad}
      onToggle={vi.fn()}
      onDelete={vi.fn()}
      onWatch={(_id, listener) => {
        changed = listener
        return unwatch
      }}
    />,
  )
  await screen.findByText('还没有任务')
  changed()
  expect(await screen.findByText(/最近：已完成/)).toBeInTheDocument()
  expect(screen.getByText(/下次：11\/17 09:00/)).toBeInTheDocument()
  view.unmount()
  expect(unwatch).toHaveBeenCalledOnce()
})

it('操作中禁止重复提交，失败保留任务并显示错误', async () => {
  const user = userEvent.setup()
  let reject!: (error: Error) => void
  const onToggle = vi.fn(
    () =>
      new Promise<void>((_resolve, failure) => {
        reject = failure
      }),
  )
  const onLoad = vi.fn(async () => [task])
  render(<TaskPane agentId="assistant" onLoad={onLoad} onToggle={onToggle} onDelete={vi.fn()} />)
  const toggle = await screen.findByRole('switch', { name: '禁用晨跑提醒' })
  await user.click(toggle)
  await user.click(toggle)
  expect(toggle).toBeDisabled()
  expect(onToggle).toHaveBeenCalledOnce()
  reject(new Error('任务已变更，请刷新后重试'))
  expect(await screen.findByRole('alert')).toHaveTextContent('任务已变更')
  expect(toggle).toHaveAttribute('aria-checked', 'true')
  expect(screen.getByText('晨跑提醒')).toBeInTheDocument()
  expect(onLoad).toHaveBeenCalledTimes(2)
})

it('更多按钮支持键盘删除，删除成功才移除卡片', async () => {
  const user = userEvent.setup()
  const onLoad = vi.fn().mockResolvedValueOnce([task]).mockResolvedValueOnce([])
  const onDelete = vi.fn(async () => undefined)
  render(<TaskPane agentId="assistant" onLoad={onLoad} onToggle={vi.fn()} onDelete={onDelete} />)
  const menu = await screen.findByRole('button', { name: '管理晨跑提醒' })
  menu.focus()
  await user.keyboard('{Enter}')
  expect(screen.getByRole('menuitem', { name: '删除' })).toHaveFocus()
  await user.keyboard('{Enter}')
  await screen.findByText('还没有任务')
  expect(onDelete).toHaveBeenCalledWith(task)
})

it('完成但尚未投递的结果明确提示等待对话结束', async () => {
  render(
    <TaskPane
      agentId="assistant"
      onLoad={async () => [
        {
          ...task,
          last_run_at: '2026-11-16T01:00:00Z',
          last_status: 'succeeded',
          last_delivery_status: 'waiting',
        },
      ]}
      onToggle={vi.fn()}
      onDelete={vi.fn()}
    />,
  )
  expect(await screen.findByText('结果已就绪，将在本轮对话结束后发送。')).toBeInTheDocument()
  expect(screen.getByText(/最近：已完成/)).toBeInTheDocument()
})
