import { act, render, renderHook, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import App from '../../App'
import type { DesktopBackend, SessionSnapshot } from '../desktop/backend'
import { memoryBackend } from '../desktop/memoryBackend'
import { CONNECTION_CHECK_MS } from './sessionProbe'
import { useSession } from './useSession'

const previous = window.desktop

afterEach(() => {
  vi.useRealTimers()
  window.desktop = previous
})

function installBackend(overrides: Partial<DesktopBackend> = {}): DesktopBackend {
  const backend = {
    session: vi
      .fn()
      .mockResolvedValue({ phase: 'address', serverUrl: '' } satisfies SessionSnapshot),
    connect: vi.fn().mockResolvedValue({
      phase: 'login',
      serverUrl: 'https://tom.example.com',
    } satisfies SessionSnapshot),
    browserLogin: vi.fn().mockResolvedValue({ phase: 'ready' } satisfies SessionSnapshot),
    checkClientUpdate: vi.fn().mockResolvedValue(null),
    watchUpdateRequest: () => () => undefined,
    loadWorkspace: vi.fn(),
    saveWorkspace: vi.fn(),
    deliverMessage: vi.fn(),
    ...overrides,
  } as DesktopBackend
  window.desktop = { platform: 'linux', backend }
  return backend
}

describe('服务地址', () => {
  it('工作空间加载中断时显示失败原因，服务恢复后打开之前的工作空间', async () => {
    const { workspace } = memoryBackend.tryWorkspaceSync()
    const session = vi
      .fn<DesktopBackend['session']>()
      .mockResolvedValueOnce({ phase: 'ready' })
      .mockResolvedValueOnce({ phase: 'offline' })
      .mockResolvedValue({ phase: 'ready' })
    const loadWorkspace = vi
      .fn<DesktopBackend['loadWorkspace']>()
      .mockResolvedValueOnce({ errorCode: 'OFFLINE', message: '无法连接服务' })
      .mockResolvedValue({ workspace, pristine: false })
    const backend = Object.create(memoryBackend) as DesktopBackend
    backend.trySessionSync = undefined
    backend.tryWorkspaceSync = undefined
    backend.session = session
    backend.loadWorkspace = loadWorkspace
    window.desktop = { platform: 'linux', backend }
    render(<App />)
    expect(await screen.findByRole('alert')).toHaveTextContent('无法连接服务')
    expect(screen.queryByRole('heading', { name: '招聘第一只猫咪' })).not.toBeInTheDocument()
    await act(async () => window.dispatchEvent(new Event('online')))
    expect(await screen.findByRole('textbox', { name: '给小黑发消息' })).toBeInTheDocument()
    expect(loadWorkspace).toHaveBeenCalledTimes(2)
  })

  it.each([false, true])(
    '未受信任证书提供核对入口并支持验证重试（跨进程状态：%s）',
    async (isSnapshot) => {
      const address = 'https://tom.example.com'
      let finish!: () => void
      const backend = installBackend({
        session: vi.fn().mockResolvedValue({ phase: 'login', serverUrl: address }),
        browserLogin: isSnapshot
          ? vi.fn().mockResolvedValue({
              phase: 'login',
              message: '证书尚未受信任',
              errorCode: 'CERTIFICATE_UNTRUSTED',
            })
          : vi
              .fn()
              .mockRejectedValue(
                Object.assign(new Error('证书尚未受信任'), { code: 'CERTIFICATE_UNTRUSTED' }),
              ),
        trustServerCertificate: vi
          .fn()
          .mockRejectedValueOnce(new Error('指纹不一致'))
          .mockImplementationOnce(
            () =>
              new Promise<void>((resolve) => {
                finish = resolve
              }),
          ),
      })
      const user = userEvent.setup()
      render(<App />)
      await user.click(await screen.findByRole('button', { name: '在浏览器中登录' }))
      expect(await screen.findByLabelText('证书 SHA-256 指纹')).toHaveValue('')
      expect(screen.queryByRole('button', { name: '在浏览器中登录' })).not.toBeInTheDocument()
      await user.type(screen.getByLabelText('证书 SHA-256 指纹'), '0'.repeat(64))
      await user.click(screen.getByRole('button', { name: '验证并信任证书' }))
      expect(await screen.findByRole('alert')).toHaveTextContent('指纹不一致')
      await user.click(screen.getByRole('button', { name: '验证并信任证书' }))
      expect(screen.getByRole('button', { name: '修改地址' })).toBeDisabled()
      expect(screen.getByRole('button', { name: '正在验证证书' })).toBeDisabled()
      finish()
      expect(await screen.findByRole('status')).toHaveTextContent('证书已验证并保存')
      expect(screen.getByRole('button', { name: '在浏览器中登录' })).toBeEnabled()
      expect(screen.queryByLabelText('证书 SHA-256 指纹')).not.toBeInTheDocument()
      expect(backend.trustServerCertificate).toHaveBeenCalledWith(address, '0'.repeat(64))
      expect(backend.browserLogin).toHaveBeenCalledTimes(1)
    },
  )

  it('会话续期遇到证书错误也可验证，修改地址清除上一台服务的提示', async () => {
    installBackend({
      session: vi.fn().mockResolvedValue({
        phase: 'offline',
        serverUrl: 'https://tom.example.com',
        errorCode: 'CERTIFICATE_UNTRUSTED',
        message: '证书尚未受信任',
      }),
    })
    const user = userEvent.setup()
    render(<App />)
    expect(await screen.findByLabelText('证书 SHA-256 指纹')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '修改地址' }))
    expect(screen.queryByLabelText('证书 SHA-256 指纹')).not.toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.getByLabelText('服务地址')).toHaveValue('https://tom.example.com')
  })

  it('缺少服务地址时不会提供浏览器登录入口', async () => {
    const backend = installBackend({ session: vi.fn().mockResolvedValue({ phase: 'login' }) })
    render(<App />)
    expect(await screen.findByLabelText('服务地址')).toHaveValue('')
    expect(screen.queryByRole('button', { name: '在浏览器中登录' })).not.toBeInTheDocument()
    expect(backend.browserLogin).not.toHaveBeenCalled()
  })

  it('登录页显示服务地址，可以修改地址后重新确认', async () => {
    const initial = 'https://first.example.test'
    const changed = 'https://second.example.test:9443'
    const backend = installBackend({
      session: vi.fn().mockResolvedValue({ phase: 'login', serverUrl: initial }),
      connect: vi.fn().mockResolvedValue({ phase: 'login', serverUrl: changed }),
      browserLogin: vi.fn().mockRejectedValue(new Error('登录未完成')),
    })
    const user = userEvent.setup()
    render(<App />)
    expect(await screen.findByText(initial)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '在浏览器中登录' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('登录未完成')
    await user.click(screen.getByRole('button', { name: '修改地址' }))
    expect(screen.getByLabelText('服务地址')).toHaveValue(initial)
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    await user.clear(screen.getByLabelText('服务地址'))
    await user.type(screen.getByLabelText('服务地址'), changed + '/')
    await user.click(screen.getByRole('button', { name: '连接' }))
    expect(await screen.findByText(changed)).toBeInTheDocument()
    expect(screen.queryByText(initial)).not.toBeInTheDocument()
    expect(backend.connect).toHaveBeenCalledWith(changed)
  })

  it('等待浏览器授权时禁止切换服务，失败后恢复修改入口', async () => {
    let rejectLogin!: (error: Error) => void
    installBackend({
      session: vi.fn().mockResolvedValue({ phase: 'login', serverUrl: 'https://tom.example.com' }),
      browserLogin: vi.fn().mockImplementation(
        () =>
          new Promise((_, reject) => {
            rejectLogin = reject
          }),
      ),
    })
    const user = userEvent.setup()
    render(<App />)
    await user.click(await screen.findByRole('button', { name: '在浏览器中登录' }))
    expect(screen.getByRole('button', { name: '修改地址' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '等待浏览器登录' })).toBeDisabled()
    rejectLogin(new Error('登录授权已失效'))
    expect(await screen.findByRole('alert')).toHaveTextContent('登录授权已失效')
    expect(screen.getByRole('button', { name: '修改地址' })).toBeEnabled()
  })

  it('没有已保存地址时显示连接页，预填不会写成正在连接', async () => {
    installBackend({
      session: vi.fn().mockResolvedValue({
        phase: 'address',
        serverUrl: 'https://tom.example.com',
      }),
    })
    render(<App />)
    expect(await screen.findByRole('heading', { name: '连接服务' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '正在连接服务' })).not.toBeInTheDocument()
    expect(screen.getByLabelText('服务地址')).toHaveValue('https://tom.example.com')
    expect(screen.queryByRole('textbox', { name: '给小黑发消息' })).not.toBeInTheDocument()
  })

  it('没有预填时地址是空的', async () => {
    installBackend()
    render(<App />)
    const input = await screen.findByLabelText('服务地址')
    expect(input).toHaveValue('')
  })

  it('首次询问服务失败后进入失败页，可以改地址再连', async () => {
    const backend = installBackend({
      session: vi.fn().mockRejectedValue(new Error('无法连接服务。请确认地址和网络后重试。')),
    })
    const user = userEvent.setup()
    render(<App />)
    expect(await screen.findByRole('heading', { name: '无法连接服务' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '正在连接服务' })).not.toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('无法连接服务')
    await user.type(screen.getByLabelText('服务地址'), 'https://tom.example.com')
    await user.click(screen.getByRole('button', { name: '重新连接' }))
    expect(await screen.findByRole('heading', { name: '登录 TomBot' })).toBeInTheDocument()
    expect(backend.connect).toHaveBeenCalledWith('https://tom.example.com')
  })

  it('已保存地址连不上时，失败页带上原来的地址', async () => {
    installBackend({
      session: vi.fn().mockResolvedValue({
        phase: 'offline',
        message: '无法连接服务。请确认地址和网络后重试。',
        serverUrl: 'https://tom.example.com',
      }),
    })
    render(<App />)
    expect(await screen.findByRole('heading', { name: '无法连接服务' })).toBeInTheDocument()
    expect(screen.getByLabelText('服务地址')).toHaveValue('https://tom.example.com')
    expect(screen.getByRole('button', { name: '重新连接' })).toBeEnabled()
  })

  it('填写合法地址后进入浏览器登录页', async () => {
    memoryBackend.phase = 'address'
    const user = userEvent.setup()
    render(<App />)
    expect(screen.getByRole('heading', { name: '连接服务' })).toBeInTheDocument()
    await user.type(screen.getByLabelText('服务地址'), 'https://tom.example.com')
    await user.click(screen.getByRole('button', { name: '连接' }))
    expect(await screen.findByRole('heading', { name: '登录 TomBot' })).toBeInTheDocument()
    expect(memoryBackend.serverUrl).toBe('https://tom.example.com')
  })

  it('地址不合法时留在连接页，不会发起连接', async () => {
    memoryBackend.phase = 'address'
    const connect = vi.spyOn(memoryBackend, 'connect')
    const user = userEvent.setup()
    render(<App />)
    await user.type(screen.getByLabelText('服务地址'), '127.0.0.1:8787')
    await user.click(screen.getByRole('button', { name: '连接' }))
    expect(screen.getByRole('alert')).toHaveTextContent('http://')
    expect(screen.getByRole('heading', { name: '连接服务' })).toBeInTheDocument()
    expect(connect).not.toHaveBeenCalled()
    expect(memoryBackend.serverUrl).toBe('')
  })

  it('连接失败后可以修改地址再试', async () => {
    memoryBackend.phase = 'offline'
    memoryBackend.serverUrl = 'http://10.0.0.8:8787'
    memoryBackend.connectError = '无法连接服务。请确认地址和网络后重试。'
    const user = userEvent.setup()
    render(<App />)
    expect(screen.getByLabelText('服务地址')).toHaveValue('http://10.0.0.8:8787')
    await user.click(screen.getByRole('button', { name: '重新连接' }))
    expect(await screen.findByRole('heading', { name: '无法连接服务' })).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('无法连接服务')
    memoryBackend.connectError = null
    await user.clear(screen.getByLabelText('服务地址'))
    await user.type(screen.getByLabelText('服务地址'), 'https://tom.example.com')
    await user.click(screen.getByRole('button', { name: '重新连接' }))
    expect(await screen.findByRole('heading', { name: '登录 TomBot' })).toBeInTheDocument()
    expect(memoryBackend.serverUrl).toBe('https://tom.example.com')
  })
})

describe('已保存会话的恢复', () => {
  const serverUrl = 'https://tom.example.test'

  it('启动时网络异常，服务恢复后通过后台探测直接回到已登录状态', async () => {
    vi.useFakeTimers()
    const backend = installBackend({
      session: vi
        .fn()
        .mockResolvedValueOnce({ phase: 'offline', errorCode: 'CONNECTION_TIMEOUT', serverUrl })
        .mockResolvedValueOnce({ phase: 'offline', errorCode: 'IDENTITY_UNAVAILABLE', serverUrl })
        .mockResolvedValue({ phase: 'ready', serverUrl }),
    })
    const { result } = renderHook(useSession)
    await act(async () => {})
    expect(result.current.phase).toBe('offline')
    await act(() => vi.advanceTimersByTimeAsync(CONNECTION_CHECK_MS))
    expect(result.current.phase).toBe('offline')
    await act(() => vi.advanceTimersByTimeAsync(CONNECTION_CHECK_MS))
    expect(result.current.phase).toBe('ready')
    await act(() => vi.advanceTimersByTimeAsync(CONNECTION_CHECK_MS))
    expect(backend.session).toHaveBeenCalledTimes(3)
    expect(backend.connect).not.toHaveBeenCalled()
    expect(backend.browserLogin).not.toHaveBeenCalled()
  })

  it('恢复时发现证书变化，停止后台探测并要求用户核对', async () => {
    vi.useFakeTimers()
    const backend = installBackend({
      session: vi
        .fn()
        .mockResolvedValueOnce({ phase: 'offline', errorCode: 'OFFLINE', serverUrl })
        .mockResolvedValue({ phase: 'offline', errorCode: 'CERTIFICATE_CHANGED', serverUrl }),
    })
    const { result } = renderHook(useSession)
    await act(async () => {})
    await act(async () => window.dispatchEvent(new Event('online')))
    expect(result.current.needsCertificateTrust).toBe(true)
    await act(async () => window.dispatchEvent(new Event('focus')))
    await act(() => vi.advanceTimersByTimeAsync(CONNECTION_CHECK_MS))
    expect(backend.session).toHaveBeenCalledTimes(2)
    expect(backend.browserLogin).not.toHaveBeenCalled()
  })

  it('用户编辑服务地址后，迟到的恢复结果不能覆盖输入或发起下一次探测', async () => {
    let complete!: (snapshot: SessionSnapshot) => void
    const backend = installBackend({
      session: vi
        .fn()
        .mockResolvedValueOnce({ phase: 'offline', errorCode: 'OFFLINE', serverUrl })
        .mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              complete = resolve
            }),
        ),
    })
    const user = userEvent.setup()
    render(<App />)
    const address = await screen.findByLabelText('服务地址')
    act(() => window.dispatchEvent(new Event('online')))
    expect(backend.session).toHaveBeenCalledTimes(2)
    const changedAddress = 'https://changed.example.test'
    await user.clear(address)
    await user.type(address, changedAddress)
    await act(async () => complete({ phase: 'ready', serverUrl }))
    act(() => window.dispatchEvent(new Event('focus')))
    expect(backend.session).toHaveBeenCalledTimes(2)
    expect(screen.getByRole('heading', { name: '无法连接服务' })).toBeInTheDocument()
    expect(address).toHaveValue(changedAddress)
    await user.click(screen.getByRole('button', { name: '重新连接' }))
    expect(await screen.findByRole('heading', { name: '登录 TomBot' })).toBeInTheDocument()
    expect(backend.connect).toHaveBeenCalledOnce()
    expect(backend.connect).toHaveBeenCalledWith(changedAddress)
  })
})
