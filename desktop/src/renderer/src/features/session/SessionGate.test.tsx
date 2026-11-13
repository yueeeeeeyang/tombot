import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import App from '../../App'
import { memoryBackend } from '../desktop/memoryBackend'

function expectDragShell(): void {
  expect(document.querySelector('.session-gate')).toBeTruthy()
  expect(document.querySelector('.session-traffic-lights')).toBeTruthy()
  expect(document.querySelector('.session-card')).toBeTruthy()
}

describe('会话门禁', () => {
  it('连接、失败和登录页都留出拖动区', () => {
    memoryBackend.phase = 'loading'
    const loading = render(<App />)
    expect(screen.getByRole('heading', { name: '正在连接服务' })).toBeInTheDocument()
    expectDragShell()
    loading.unmount()

    memoryBackend.phase = 'address'
    const address = render(<App />)
    expect(screen.getByRole('heading', { name: '连接服务' })).toBeInTheDocument()
    expect(screen.getByLabelText('服务地址').closest('.session-card')).toBeTruthy()
    expectDragShell()
    address.unmount()

    memoryBackend.phase = 'offline'
    memoryBackend.serverUrl = 'https://tom.example.com'
    memoryBackend.connectError = '无法连接服务。请确认地址和网络后重试。'
    const offline = render(<App />)
    expect(screen.getByRole('heading', { name: '无法连接服务' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新连接' }).closest('.session-card')).toBeTruthy()
    expectDragShell()
    offline.unmount()

    memoryBackend.phase = 'login'
    render(<App />)
    expect(screen.getByRole('heading', { name: '登录 TomBot' })).toBeInTheDocument()
    expectDragShell()
  })

  it('登录页不收集密码，只提供浏览器登录入口', () => {
    memoryBackend.phase = 'login'
    render(<App />)
    expect(screen.getByRole('heading', { name: '登录 TomBot' })).toBeInTheDocument()
    expect(screen.queryByLabelText('密码')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('初始化码')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '在浏览器中登录' })).toBeInTheDocument()
    expect(screen.queryByRole('textbox', { name: '给小黑发消息' })).not.toBeInTheDocument()
  })

  it('浏览器登录完成后进入工作空间', async () => {
    memoryBackend.phase = 'login'
    memoryBackend.password = '口'.repeat(12)
    const user = userEvent.setup()
    render(<App />)
    await user.click(screen.getByRole('button', { name: '在浏览器中登录' }))
    expect(await screen.findByRole('textbox', { name: '给小黑发消息' })).toBeInTheDocument()
  })
})
