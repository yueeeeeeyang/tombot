import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { memoryBackend } from './features/desktop/memoryBackend'
import { emptyWorkspace } from './features/workspace/model'
import type { DeliveryResult, StreamEvent } from './features/desktop/backend'

afterEach(() => vi.restoreAllMocks())

describe('聊天体验', () => {
  it('消息已入库但运行失败时保留已发送消息，不显示未发送重试按钮', async () => {
    memoryBackend.loadFixture({ isModelPolicyAvailable: true })
    vi.spyOn(memoryBackend, 'deliverMessage').mockResolvedValue({
      messages: [
        {
          id: 'accepted-message',
          role: 'user',
          content: '你好',
          createdAt: new Date().toISOString(),
          sequence: 1,
          speaker: '',
          reactions: [],
          attachments: [],
        },
      ],
      code: 'EXECUTION_FAILED',
      message: '消息已接收，但服务端执行失败，请联系管理员检查运行日志。',
    })
    const user = userEvent.setup()
    render(<App />)
    await user.type(screen.getByRole('textbox', { name: '给小黑发消息' }), '你好')
    await user.click(screen.getByRole('button', { name: '发送' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('消息已接收，但服务端执行失败')
    expect(within(screen.getByRole('log')).getAllByText('你好')).toHaveLength(1)
    expect(screen.queryByRole('button', { name: '未发送，点击重试' })).toBeNull()
    expect(screen.queryByText('正在思考…')).toBeNull()
  })

  it('招聘后直接打开空会话，不再请求欢迎语', async () => {
    memoryBackend.loadFixture({
      workspace: emptyWorkspace(),
      isModelPolicyAvailable: true,
    })
    const deliverMessage = vi.spyOn(memoryBackend, 'deliverMessage')
    const user = userEvent.setup()
    const { unmount } = render(<App />)
    expect(screen.queryByRole('textbox', { name: /一句话介绍/ })).toBeNull()
    await user.type(screen.getByRole('textbox', { name: '助手名称' }), '灵感搭档')
    await user.click(screen.getByRole('button', { name: '招聘猫咪' }))
    expect(await screen.findByRole('textbox', { name: '给灵感搭档发消息' })).toBeInTheDocument()
    expect(screen.queryByText('内存测试欢迎内容')).toBeNull()
    expect(deliverMessage).not.toHaveBeenCalled()
    expect(
      memoryBackend.tryWorkspaceSync().workspace.agents[0]?.messages.map((message) => message.role),
    ).toEqual([])
    unmount()
    render(<App />)
    expect(screen.getByRole('textbox', { name: '给灵感搭档发消息' })).toBeInTheDocument()
    expect(deliverMessage).not.toHaveBeenCalled()
  })
  it('摘要显示在输入框上方，切换猫咪不串线，失败后清除动画并提示', async () => {
    memoryBackend.loadFixture({
      isModelPolicyAvailable: true,
    })
    let finish!: (value: DeliveryResult) => void
    let publish!: (event: StreamEvent) => void
    vi.spyOn(memoryBackend, 'deliverMessage').mockImplementation(
      (_agentId, _content, _requestId, onEvent) => {
        publish = onEvent!
        return new Promise((resolve) => {
          finish = resolve
        })
      },
    )
    const user = userEvent.setup()
    render(<App />)
    await user.type(screen.getByRole('textbox', { name: '给小黑发消息' }), '整理材料')
    await user.click(screen.getByRole('button', { name: '发送' }))
    await waitFor(() => expect(publish).toBeDefined())
    expect(screen.queryByText('正在思考…')).toBeNull()
    act(() =>
      publish({ type: 'activity', text: '', botId: 'assistant', runId: 'root', isWorking: true }),
    )
    expect(screen.getByText('正在思考…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '未发送，点击重试' })).toBeNull()
    act(() => {
      publish({
        type: 'process',
        text: '正在归纳材料中的关键结论',
        botId: 'assistant',
        runId: 'root',
      })
      publish({ type: 'delta', text: '**结论**' })
    })
    const summary = screen.getByText('正在归纳材料中的关键结论')
    const workingCat = summary.closest('.work-status')?.querySelector('.avatar')
    expect(workingCat).toHaveAttribute('data-avatar', 'bombay')
    expect(workingCat).toHaveAttribute('data-pose', 'working')
    expect(summary.closest('.composer-area')).not.toBeNull()
    expect(screen.getByRole('log')).not.toHaveTextContent('正在归纳')
    expect(screen.getByText('结论').tagName).toBe('STRONG')
    await user.click(screen.getByRole('button', { name: /^游戏程序/ }))
    expect(screen.queryByText('正在归纳材料中的关键结论')).toBeNull()
    await user.click(screen.getByRole('button', { name: '小黑' }))
    expect(screen.getByText('正在归纳材料中的关键结论')).toBeInTheDocument()
    await act(async () => finish({ code: 'MODEL_FAILED', message: '模型连接中断' }))
    expect(screen.queryByText('正在归纳材料中的关键结论')).toBeNull()
    expect(screen.getByRole('alert')).toHaveTextContent('模型连接中断')
    expect(screen.getByRole('button', { name: '未发送，点击重试' })).toBeInTheDocument()
  })
  it('只有一只猫咪时也显示操作和设置预设，并可直接进入模型策略', async () => {
    const fixture = memoryBackend.tryWorkspaceSync().workspace
    memoryBackend.loadFixture({ workspace: { ...fixture, agents: fixture.agents.slice(0, 1) } })
    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: '搜索助手和消息' }))
    expect(within(screen.getByRole('group', { name: '操作' })).getAllByRole('option')).toHaveLength(
      2,
    )
    expect(within(screen.getByRole('group', { name: '设置' })).getAllByRole('option')).toHaveLength(
      2,
    )
    fireEvent.click(screen.getByRole('option', { name: /模型策略/ }))
    expect(screen.getByRole('dialog', { name: '设置' })).toBeInTheDocument()
    expect(screen.getByText('由服务端管理')).toBeInTheDocument()
    expect(screen.queryByLabelText('API 密钥')).not.toBeInTheDocument()
  })
  it('招聘不依赖服务端回合：投递接口不可用时仍保留新猫咪', async () => {
    vi.spyOn(memoryBackend, 'deliverMessage').mockRejectedValue(new Error('投递接口不可用'))
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '新建' }))
    await user.click(screen.getByRole('menuitem', { name: '招聘猫咪' }))
    await user.type(screen.getByRole('textbox', { name: '助手名称' }), '新搭档')
    await user.click(screen.getByRole('button', { name: '招聘猫咪' }))
    expect(await screen.findByRole('textbox', { name: '给新搭档发消息' })).toBeInTheDocument()
    expect(screen.queryByText('正在思考…')).toBeNull()
  })
})
