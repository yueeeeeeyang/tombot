import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import App from '../../App'
import { memoryBackend } from '../desktop/memoryBackend'
import type { BootstrapStatus } from '../desktop/backend'

const failedBootstrap: BootstrapStatus = {
  phase: 'failed',
  cursor: 4,
  error: '夹具没有浏览器',
  steps: [
    { id: 'detect_os', title: '检测操作系统', status: 'ok', duration_ms: 1 },
    { id: 'install_fonts', title: '安装中文字体', status: 'ok', duration_ms: 1 },
    {
      id: 'ensure_browser',
      title: '准备托管浏览器',
      status: 'failed',
      detail: '夹具没有浏览器',
      duration_ms: 2,
    },
  ],
}

function expectDragShell(): void {
  expect(document.querySelector('.session-gate')).toBeTruthy()
  expect(document.querySelector('.session-traffic-lights')).toBeTruthy()
  expect(document.querySelector('.session-card')).toBeTruthy()
}

describe('会话门禁', () => {
  it('连接、失败、初始化和密码页都留出拖动区', async () => {
    memoryBackend.phase = 'loading'
    const loading = render(<App />)
    expect(screen.getByRole('heading', { name: '正在连接服务' })).toBeInTheDocument()
    expectDragShell()
    loading.unmount()

    memoryBackend.phase = 'loading'
    memoryBackend.sessionError = new Error('无法连接本机服务。请确认服务已启动后重试。')
    const user = userEvent.setup()
    render(<App />)
    expect(await screen.findByRole('heading', { name: '无法连接服务' })).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('无法连接')
    expectDragShell()
    expect(screen.getByRole('button', { name: '重新连接' }).closest('.session-card')).toBeTruthy()
    memoryBackend.sessionError = null
    memoryBackend.phase = 'setup'
    await user.click(screen.getByRole('button', { name: '重新连接' }))
    expect(await screen.findByRole('heading', { name: '初始化 TomBot' })).toBeInTheDocument()
    expect(screen.getByLabelText('初始化码').closest('.session-card')).toBeTruthy()
    expectDragShell()
  })

  it('未初始化时先走初始化，不直接显示引导进度', () => {
    memoryBackend.phase = 'setup'
    memoryBackend.bootstrap = failedBootstrap
    render(<App />)
    expect(screen.getByRole('heading', { name: '初始化 TomBot' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '正在准备服务' })).not.toBeInTheDocument()
    expect(screen.queryByRole('textbox', { name: '给小黑发消息' })).not.toBeInTheDocument()
  })

  it('验证密码后引导未完成时显示阶段和进度，失败重试不会进入工作空间', async () => {
    memoryBackend.phase = 'login'
    memoryBackend.password = '口'.repeat(12)
    memoryBackend.bootstrap = failedBootstrap
    memoryBackend.failNextBootstrap = true
    const user = userEvent.setup()
    render(<App />)
    await user.type(screen.getByLabelText('密码'), '口'.repeat(12))
    await user.click(screen.getByRole('button', { name: '进入工作空间' }))
    expect(await screen.findByRole('heading', { name: '正在准备服务' })).toBeInTheDocument()
    expect(screen.getByText('当前阶段：引导失败')).toBeInTheDocument()
    expect(screen.getByText('已完成 2 / 3')).toBeInTheDocument()
    expect(screen.getByRole('progressbar', { name: '引导进度' })).toHaveProperty('value', 2)
    expect(screen.getByRole('alert')).toHaveTextContent('夹具没有浏览器')
    expect(screen.queryByRole('textbox', { name: '给小黑发消息' })).not.toBeInTheDocument()
    expectDragShell()
    await user.click(screen.getByRole('button', { name: '重新准备' }))
    expect(screen.getByRole('heading', { name: '正在准备服务' })).toBeInTheDocument()
    expect(screen.queryByRole('textbox', { name: '给小黑发消息' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '重新准备' }))
    expect(await screen.findByRole('textbox', { name: '给小黑发消息' })).toBeInTheDocument()
  })

  it('引导事件把进行中更新为失败，成功后才进入工作空间', async () => {
    memoryBackend.bootstrap = {
      phase: 'preparing',
      cursor: 2,
      steps: [
        { id: 'detect_os', title: '检测操作系统', status: 'ok', duration_ms: 1 },
        { id: 'install_fonts', title: '安装中文字体', status: 'running', duration_ms: 0 },
      ],
    }
    const user = userEvent.setup()
    render(<App />)
    expect(screen.getByRole('heading', { name: '正在准备服务' })).toBeInTheDocument()
    expect(screen.getByText('当前阶段：准备依赖，正在安装中文字体')).toBeInTheDocument()
    expect(screen.getByText('进行中')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '重新准备' })).not.toBeInTheDocument()
    act(() => {
      memoryBackend.publishBootstrap({
        phase: 'failed',
        cursor: 3,
        error: '夹具没有浏览器',
        steps: [
          { id: 'detect_os', title: '检测操作系统', status: 'ok', duration_ms: 1 },
          {
            id: 'install_fonts',
            title: '安装中文字体',
            status: 'failed',
            detail: '夹具没有浏览器',
            duration_ms: 2,
          },
        ],
      })
    })
    expect(screen.getByRole('alert')).toHaveTextContent('夹具没有浏览器')
    await user.click(screen.getByRole('button', { name: '重新准备' }))
    expect(await screen.findByRole('textbox', { name: '给小黑发消息' })).toBeInTheDocument()
  })
})
