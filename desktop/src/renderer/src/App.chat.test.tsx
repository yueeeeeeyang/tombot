import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { memoryBackend } from './features/desktop/memoryBackend'
import { emptyWorkspace } from './features/workspace/model'
import type { DeliveryResult, StreamEvent } from './features/desktop/backend'

afterEach(() => vi.restoreAllMocks())

describe('聊天体验', () => {
  it('招聘后自动打开聊天，仅展示模型欢迎内容且重开不会重复生成', async () => {
    memoryBackend.loadFixture({
      workspace: emptyWorkspace(),
      settings: { baseUrl: 'https://model.example.test', modelName: '测试模型' },
    })
    const welcome = vi.spyOn(memoryBackend, 'welcomeAgent')
    const user = userEvent.setup()
    const { unmount } = render(<App />)
    expect(screen.queryByRole('textbox', { name: /一句话介绍/ })).toBeNull()
    await user.type(screen.getByRole('textbox', { name: '助手名称' }), '灵感搭档')
    await user.click(screen.getByRole('button', { name: '招聘猫咪' }))
    expect(await screen.findByRole('textbox', { name: '给灵感搭档发消息' })).toBeInTheDocument()
    expect(await screen.findByText('内存测试欢迎内容')).toBeInTheDocument()
    expect(welcome).toHaveBeenCalledTimes(1)
    expect(screen.queryByText('你的想法，有了一个新家。')).toBeNull()
    expect(screen.queryByText(/嗨，我是/)).toBeNull()
    expect(
      memoryBackend.tryWorkspaceSync().workspace.agents[0]?.messages.map((message) => message.role),
    ).toEqual(['assistant'])
    unmount()
    render(<App />)
    expect(screen.getByText('内存测试欢迎内容')).toBeInTheDocument()
    expect(welcome).toHaveBeenCalledTimes(1)
  })
  it('摘要显示在输入框上方，切换猫咪不串线，失败后清除动画并提示', async () => {
    memoryBackend.loadFixture({
      settings: { baseUrl: 'https://model.example.test', modelName: '测试模型' },
    })
    let finish!: (value: DeliveryResult) => void
    let publish!: (event: StreamEvent) => void
    vi.spyOn(memoryBackend, 'deliverMessage').mockImplementation(
      (_agentId, _messageId, onEvent) => {
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
  it('只有一只猫咪时也显示操作和设置预设，并可直接进入模型设置', async () => {
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
    fireEvent.click(screen.getByRole('option', { name: /模型设置/ }))
    expect(screen.getByRole('dialog', { name: '设置' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '内置提供商' })).toBeInTheDocument()
  })
  it('欢迎请求失败仍保留新猫咪并显示真实错误', async () => {
    vi.spyOn(memoryBackend, 'welcomeAgent').mockRejectedValue(new Error('欢迎请求连接失败'))
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '新建' }))
    await user.click(screen.getByRole('menuitem', { name: '招聘猫咪' }))
    await user.type(screen.getByRole('textbox', { name: '助手名称' }), '新搭档')
    await user.click(screen.getByRole('button', { name: '招聘猫咪' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('欢迎请求连接失败')
    expect(screen.getByRole('textbox', { name: '给新搭档发消息' })).toBeInTheDocument()
    expect(screen.queryByText('正在思考…')).toBeNull()
  })
})
