import { taskFixture } from '../../../../shared/tasks.fixture'
import { render, screen, within, waitFor, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { createWorkspace } from '../workspace/model'
import { CatSidePanel } from './CatSidePanel'
import { memoryBackend } from '../desktop/memoryBackend'

const agent = { ...createWorkspace().agents[0]!, conversationId: 'direct-chat' }

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
    expect(await screen.findByText(/资料库还是空的/)).toBeInTheDocument()
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

  it('单聊资料库按会话读取用户与猫咪的附件，搜索包含历史文件', async () => {
    const user = userEvent.setup()
    const files = ['用户上传.txt', '猫咪报告.md'].map((name, i) => ({
      id: `file-${i}`,
      conversation_id: agent.conversationId,
      name,
      media_type: 'text/plain',
      size: 6,
      sha256: 'a'.repeat(64),
      revision: 1,
      created_at: '2026-11-17T00:00:00Z',
    }))
    const load = vi.spyOn(memoryBackend, 'listChatFiles').mockResolvedValue({
      files,
      next: '',
      used_bytes: 12,
      max_bytes: 1024,
      max_file_bytes: 1024,
    })
    render(<CatSidePanel agent={agent} onClose={vi.fn()} onComputerUrl={vi.fn()} />)
    await user.click(screen.getByRole('tab', { name: '资料库' }))
    expect(await screen.findByText('用户上传.txt')).toBeInTheDocument()
    expect(screen.getByText('猫咪报告.md')).toBeInTheDocument()
    expect(load).toHaveBeenCalledWith('direct-chat', '', '')
    await user.type(screen.getByRole('textbox', { name: '搜索聊天文件' }), '报告')
    await user.click(screen.getByRole('button', { name: '搜索文件' }))
    await waitFor(() => expect(load).toHaveBeenLastCalledWith('direct-chat', '', '报告'))
    expect(screen.queryByRole('button', { name: '上传文件' })).toBeNull()
  })
})
