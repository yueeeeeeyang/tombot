import { fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { RoutinePane, type CatRoutine } from './RoutinePane'

const routine: CatRoutine = {
  id: 'r1',
  name: '晨跑提醒',
  cron: '0 9 * * 3',
  prompt: '提醒晨跑',
  is_enabled: true,
  revision: 1,
}

describe('定时面板', () => {
  it('只有启用开关，右键菜单可以删除', async () => {
    const user = userEvent.setup()
    const onToggle = vi.fn(async () => undefined)
    const onDelete = vi.fn(async () => undefined)
    render(
      <RoutinePane
        agentId="assistant"
        onLoad={async () => [routine]}
        onToggle={onToggle}
        onDelete={onDelete}
      />,
    )
    expect(screen.queryByRole('button', { name: '添加定时任务' })).not.toBeInTheDocument()
    await user.click(await screen.findByRole('switch', { name: '暂停晨跑提醒' }))
    expect(onToggle).toHaveBeenCalledWith(routine)
    fireEvent.contextMenu(screen.getByText('晨跑提醒'))
    await user.click(await screen.findByRole('menuitem', { name: '删除' }))
    expect(onDelete).toHaveBeenCalledWith(routine)
  })

  it('没有定时任务时展示空状态', async () => {
    render(
      <RoutinePane
        agentId="assistant"
        onLoad={async () => []}
        onToggle={async () => undefined}
        onDelete={async () => undefined}
      />,
    )
    expect(await screen.findByRole('status')).toHaveTextContent('还没有定时任务')
    expect(screen.getByRole('status')).toHaveClass('cat-panel-empty')
  })

  it('读取失败时显示错误，不把失败展示为空定时任务', async () => {
    const view = render(
      <RoutinePane
        agentId="assistant"
        onLoad={async () => {
          throw new Error('定时读取失败')
        }}
        onToggle={async () => undefined}
        onDelete={async () => undefined}
      />,
    )
    expect(await screen.findByRole('alert')).toHaveTextContent('定时读取失败')
    expect(screen.queryByText('还没有定时任务')).toBeNull()
    view.rerender(
      <RoutinePane
        agentId="assistant"
        onLoad={async () => []}
        onToggle={async () => undefined}
        onDelete={async () => undefined}
      />,
    )
    expect(await screen.findByRole('status')).toHaveClass('cat-panel-empty')
    expect(screen.queryByRole('alert')).toBeNull()
  })
})
