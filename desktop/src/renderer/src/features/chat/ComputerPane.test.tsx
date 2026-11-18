import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useEffect } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { ConsoleBackend, DesktopConsole } from '../../../../shared/console'
import { ComputerPane } from './ComputerPane'
import { getBackend } from '../desktop/backend'
vi.mock('../desktop/backend', () => ({ getBackend: vi.fn() }))
vi.mock('./ComputerScreen', () => ({
  ComputerScreen({
    isControlling,
    onConnected,
  }: {
    isControlling: boolean
    onConnected: () => void
  }) {
    useEffect(onConnected, [onConnected])
    return (
      <div role="img" aria-label="小黑的电脑画面">
        {isControlling ? '可操作' : '只读'}
      </div>
    )
  },
}))
const session: DesktopConsole = {
  id: '01ARZ3NDEKTSV4RRFFQ69G5FAV',
  bot_id: '01ARZ3NDEKTSV4RRFFQ69G5FAW',
  state: 'viewing',

  error_code: '',
  expires_at: '2026-11-17T00:00:30Z',
}
function backend(): ConsoleBackend {
  let current = session
  return {
    openConsole: vi.fn(async () => (current = session)),
    consoleStatus: vi.fn(async () => current),
    closeConsole: vi.fn(async () => (current = { ...session, state: 'closed' })),
    controlConsole: vi.fn(
      async (_id, takeover) => (current = { ...session, state: takeover ? 'active' : 'viewing' }),
    ),
    connectConsole: vi.fn(),
  }
}
async function openViewer() {
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: '小黑的电脑' }))
  return { user, viewer: within(screen.getByRole('dialog', { name: '小黑的电脑' })) }
}
beforeEach(() => vi.clearAllMocks())
describe('猫咪电脑观看与接管', () => {
  it('单击电脑卡片直接打开只读观看窗口，取得控制权后才能输入，交还后继续观看', async () => {
    const api = backend()
    vi.mocked(getBackend).mockReturnValue(api as ReturnType<typeof getBackend>)
    render(<ComputerPane agentId={session.bot_id} agentName="小黑" />)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(api.openConsole).not.toHaveBeenCalled()
    const { user, viewer } = await openViewer()
    expect(await viewer.findByRole('img')).toHaveTextContent('只读')
    expect(viewer.getByText('仅观看')).toBeVisible()
    expect(viewer.getByRole('button', { name: '全屏查看' })).toBeVisible()
    expect(api.controlConsole).not.toHaveBeenCalled()
    expect(api.openConsole).toHaveBeenCalledOnce()
    expect(api.openConsole).toHaveBeenCalledWith(
      session.bot_id,
      expect.stringMatching(/^[0-9A-HJKMNP-TV-Z]{26}$/),
    )
    await user.click(viewer.getByRole('button', { name: '取得控制权' }))
    expect(viewer.getByRole('img')).toHaveTextContent('可操作')
    await user.click(viewer.getByRole('button', { name: '交还控制权' }))
    expect(viewer.getByRole('img')).toHaveTextContent('只读')
    expect(api.closeConsole).not.toHaveBeenCalled()
    await user.click(viewer.getByRole('button', { name: '关闭观看' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(api.closeConsole).toHaveBeenCalledWith(session.id)
  })
  it('接管失败不启用输入，不自动重放控制', async () => {
    const api = backend()
    vi.mocked(api.controlConsole).mockRejectedValue(new Error('接管未确认'))
    vi.mocked(getBackend).mockReturnValue(api as ReturnType<typeof getBackend>)
    render(<ComputerPane agentId={session.bot_id} agentName="小黑" />)
    const { user, viewer } = await openViewer()
    await viewer.findByRole('img')
    await user.click(viewer.getByRole('button', { name: '取得控制权' }))
    expect(await viewer.findByRole('alert')).toHaveTextContent('接管未确认')
    expect(api.controlConsole).toHaveBeenCalledOnce()
    expect(viewer.queryByText('可操作')).not.toBeInTheDocument()
  })
  it('申请期间可关闭观看窗口，回收迟到的会话且不打开画面', async () => {
    const api = backend()
    let resolve!: (value: DesktopConsole) => void
    vi.mocked(api.openConsole).mockImplementationOnce(
      () =>
        new Promise((done) => {
          resolve = done
        }),
    )
    vi.mocked(getBackend).mockReturnValue(api as ReturnType<typeof getBackend>)
    render(<ComputerPane agentId={session.bot_id} agentName="小黑" />)
    const { user, viewer } = await openViewer()
    expect(api.openConsole).toHaveBeenCalledOnce()
    expect(viewer.queryByRole('img')).not.toBeInTheDocument()
    await user.click(viewer.getByRole('button', { name: '关闭观看' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(api.closeConsole).not.toHaveBeenCalled()

    await act(async () => resolve(session))
    expect(api.closeConsole).toHaveBeenCalledExactlyOnceWith(session.id)
    expect(api.consoleStatus).not.toHaveBeenCalled()
    expect(api.connectConsole).not.toHaveBeenCalled()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })
  it('离开面板撤销观看，迟到的申请也必须关闭', async () => {
    const api = backend()
    let resolve!: (value: DesktopConsole) => void
    vi.mocked(api.openConsole).mockImplementation(
      () =>
        new Promise((done) => {
          resolve = done
        }),
    )
    vi.mocked(getBackend).mockReturnValue(api as ReturnType<typeof getBackend>)
    const view = render(<ComputerPane agentId={session.bot_id} agentName="小黑" />)
    await openViewer()
    view.unmount()
    await act(async () => resolve(session))
    await waitFor(() => expect(api.closeConsole).toHaveBeenCalledWith(session.id))
  })
  it('连接失败明确提示，重新连接成功后才显示画面', async () => {
    const api = backend()
    vi.mocked(api.openConsole).mockRejectedValueOnce(new Error('电脑暂不可用'))
    vi.mocked(getBackend).mockReturnValue(api as ReturnType<typeof getBackend>)
    render(<ComputerPane agentId={session.bot_id} agentName="小黑" />)
    const { user, viewer } = await openViewer()
    expect(await viewer.findByRole('alert')).toHaveTextContent('电脑暂不可用')
    expect(viewer.queryByRole('img')).not.toBeInTheDocument()
    expect(api.connectConsole).not.toHaveBeenCalled()
    await user.click(viewer.getByRole('button', { name: '重新连接' }))
    expect(await viewer.findByRole('img')).toHaveTextContent('只读')
    expect(viewer.queryByRole('alert')).not.toBeInTheDocument()
    expect(api.openConsole).toHaveBeenCalledTimes(2)
    expect(vi.mocked(api.openConsole).mock.calls[1]).toEqual(
      vi.mocked(api.openConsole).mock.calls[0],
    )
  })
})
