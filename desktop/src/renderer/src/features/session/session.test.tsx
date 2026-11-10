import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import App from '../../App'
import type { DesktopBackend, SessionSnapshot } from '../desktop/backend'
import { memoryBackend } from '../desktop/memoryBackend'

const previous = window.desktop

afterEach(() => {
  window.desktop = previous
})

function installBackend(overrides: Partial<DesktopBackend> = {}): DesktopBackend {
  const backend = {
    session: vi
      .fn()
      .mockResolvedValue({ phase: 'address', serverUrl: '' } satisfies SessionSnapshot),
    connect: vi.fn().mockResolvedValue({
      phase: 'setup',
      serverUrl: 'https://tom.example.com',
    } satisfies SessionSnapshot),
    setup: vi.fn(),
    login: vi.fn(),
    loadWorkspace: vi.fn(),
    saveWorkspace: vi.fn(),
    deliverMessage: vi.fn(),
    loadSettings: vi.fn(),
    saveSettings: vi.fn(),
    ...overrides,
  } as DesktopBackend
  window.desktop = { platform: 'linux', backend }
  return backend
}

describe('服务地址', () => {
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
    expect(await screen.findByRole('heading', { name: '初始化 TomBot' })).toBeInTheDocument()
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

  it('填写合法地址后才进入初始化', async () => {
    memoryBackend.phase = 'address'
    const user = userEvent.setup()
    render(<App />)
    expect(screen.getByRole('heading', { name: '连接服务' })).toBeInTheDocument()
    await user.type(screen.getByLabelText('服务地址'), 'https://tom.example.com')
    await user.click(screen.getByRole('button', { name: '连接' }))
    expect(await screen.findByRole('heading', { name: '初始化 TomBot' })).toBeInTheDocument()
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
    expect(await screen.findByRole('heading', { name: '初始化 TomBot' })).toBeInTheDocument()
    expect(memoryBackend.serverUrl).toBe('https://tom.example.com')
  })
})
