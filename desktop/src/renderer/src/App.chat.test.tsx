import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { memoryBackend } from './features/desktop/memoryBackend'
import { createWorkspace, emptyWorkspace } from './features/workspace/model'
import type { DeliveryResult, StreamEvent } from './features/desktop/backend'

afterEach(() => vi.restoreAllMocks())

describe('聊天体验', () => {
  it('错误与回复同时显示在输入区，分别关闭时保留草稿和另一个卡片', async () => {
    const workspace = createWorkspace()
    workspace.agents[0]!.messages = [
      {
        id: 'original',
        role: 'assistant',
        content: '之前的消息',
        createdAt: new Date().toISOString(),
        status: 'sent',
        reactions: [],
      },
    ]
    memoryBackend.loadFixture({ workspace })
    vi.spyOn(memoryBackend, 'loadUserProfile').mockRejectedValue(new Error('接口不存在'))
    const user = userEvent.setup()
    render(<App />)
    const alert = await screen.findByRole('alert')
    const input = screen.getByRole('textbox', { name: '给小黑发消息' })
    fireEvent.change(input, { target: { value: '第一行\n第二行' } })
    await user.click(screen.getByRole('button', { name: '回复' }))
    const reply = screen.getByRole('region', { name: '正在回复小黑' })
    const composerArea = input.closest('.composer-area')
    expect(alert.parentElement).toBe(composerArea)
    expect(reply.parentElement).toBe(composerArea)
    await user.click(screen.getByRole('button', { name: '关闭提示' }))
    expect(screen.queryByRole('alert')).toBeNull()
    expect(reply).toBeInTheDocument()
    expect(input).toHaveValue('第一行\n第二行')
    await user.click(screen.getByRole('button', { name: '取消回复' }))
    expect(screen.queryByRole('region', { name: '正在回复小黑' })).toBeNull()
    expect(input).toHaveValue('第一行\n第二行')
  })

  it('资料错误可关闭，普通操作和重复焦点刷新不会让同一提示重新出现', async () => {
    const load = vi
      .spyOn(memoryBackend, 'loadUserProfile')
      .mockRejectedValue(new Error('接口不存在'))
    const user = userEvent.setup()
    render(<App />)
    expect(await screen.findByRole('alert')).toHaveTextContent('读取用户资料失败：接口不存在')
    await user.click(screen.getByRole('button', { name: '关闭提示' }))
    expect(screen.queryByRole('alert')).toBeNull()
    await user.type(screen.getByRole('textbox', { name: '给小黑发消息' }), '继续写草稿')
    act(() => window.dispatchEvent(new Event('focus')))
    await waitFor(() => expect(load).toHaveBeenCalledTimes(2))
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('表情通过服务保存，失败可关闭提示，重试成功后可取消', async () => {
    const workspace = createWorkspace()
    workspace.agents[0]!.messages = [
      {
        id: 'original',
        role: 'assistant',
        content: '已经完成计划',
        createdAt: new Date().toISOString(),
        status: 'sent',
        reactions: [],
        sequence: 1,
      },
    ]
    memoryBackend.loadFixture({ workspace })
    const change = vi
      .spyOn(memoryBackend, 'setReaction')
      .mockRejectedValueOnce(new Error('服务暂时不可用'))
    const user = userEvent.setup()
    render(<App />)
    const mark = async () => {
      await user.click(screen.getByRole('button', { name: '添加表情' }))
      await user.click(
        within(screen.getByRole('menu', { name: '消息操作' })).getByRole('button', {
          name: '回应👍',
        }),
      )
    }
    await mark()
    expect(await screen.findByRole('alert')).toHaveTextContent('表情更新失败')
    expect(screen.queryByRole('button', { name: '👍，点击取消' })).toBeNull()
    await user.click(screen.getByRole('button', { name: '关闭提示' }))
    expect(screen.queryByRole('alert')).toBeNull()
    await mark()
    await user.click(await screen.findByRole('button', { name: '👍，点击取消' }))
    await waitFor(() => expect(screen.queryByRole('button', { name: '👍，点击取消' })).toBeNull())
    expect(change).toHaveBeenCalledTimes(3)
    expect(change.mock.calls[1]).toEqual(change.mock.calls[0])
    expect(change.mock.calls[2]?.[2]).toBe(false)
  })
  it('发送、工作动画和回执复用原气泡节点，连续发送各自显示状态且失败可重试', async () => {
    const workspace = createWorkspace()
    workspace.agents[0]!.messages = [
      {
        id: 'history',
        role: 'assistant',
        content: '历史回复',
        createdAt: '2026-11-16T01:00:00Z',
        status: 'sent',
        reactions: [],
        sequence: 1,
      },
    ]
    memoryBackend.loadFixture({ workspace, isModelPolicyAvailable: true })
    const deliveries: Array<{
      id: string
      finish: (value: DeliveryResult) => void
      publish: (event: StreamEvent) => void
    }> = []
    vi.spyOn(memoryBackend, 'deliverMessage').mockImplementation(
      (_agentId, _content, id, publish) =>
        new Promise((finish) => {
          deliveries.push({ id, finish, publish: publish! })
        }),
    )
    render(<App />)
    const history = screen.getByText('历史回复')
    const send = (content: string) => {
      fireEvent.change(screen.getByRole('textbox', { name: '给小黑发消息' }), {
        target: { value: content },
      })
      fireEvent.click(screen.getByRole('button', { name: '发送' }))
    }
    send('第一条')
    await waitFor(() => expect(deliveries).toHaveLength(1))
    const outgoing = screen.getByText('第一条')
    send('第二条')
    expect(screen.getAllByLabelText('发送中')).toHaveLength(2)
    expect(screen.queryByRole('button', { name: '未发送，点击重试' })).toBeNull()
    act(() =>
      deliveries[0]!.publish({
        type: 'activity',
        text: '',
        botId: 'assistant',
        runId: 'run',
        isWorking: true,
      }),
    )
    expect(screen.getByText('正在思考…')).toBeInTheDocument()
    expect(screen.getByText('历史回复')).toBe(history)
    await act(async () => deliveries[0]!.finish({ acceptedMessageId: 'server-first' }))
    expect(screen.getByText('第一条')).toBe(outgoing)
    expect(screen.getByText('历史回复')).toBe(history)
    await waitFor(() => expect(deliveries).toHaveLength(2))
    await act(async () => deliveries[1]!.finish({ code: 'OFFLINE', message: '无法连接服务' }))
    const failed = screen.getByText('第二条')
    expect(screen.queryByLabelText('发送中')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '未发送，点击重试' }))
    await waitFor(() => expect(deliveries).toHaveLength(3))
    expect(deliveries[2]!.id).toBe(deliveries[1]!.id)
    await act(async () => deliveries[2]!.finish({ acceptedMessageId: 'server-second' }))
    expect(screen.getByText('第二条')).toBe(failed)
    expect(screen.getByText('历史回复')).toBe(history)
  })

  it('消息已入库但运行失败时保留已发送消息，不显示未发送重试按钮', async () => {
    memoryBackend.loadFixture({ isModelPolicyAvailable: true })
    vi.spyOn(memoryBackend, 'deliverMessage').mockResolvedValue({
      acceptedMessageId: 'accepted-message',
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
