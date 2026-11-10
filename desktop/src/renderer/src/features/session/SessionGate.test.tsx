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

function stepRow(title: string): HTMLElement {
  const row = screen.getByText(title).closest('.session-step')
  if (!(row instanceof HTMLElement)) throw new Error(`找不到步骤：${title}`)
  return row
}

describe('会话门禁', () => {
  it('连接、失败、初始化和密码页都留出拖动区', () => {
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
    render(<App />)
    expect(screen.getByRole('heading', { name: '无法连接服务' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新连接' }).closest('.session-card')).toBeTruthy()
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
    expect(document.querySelector('.session-step-spinner')).toBeNull()
    expect(stepRow('准备托管浏览器')).not.toHaveClass('session-step--running')
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
    expect(stepRow('安装中文字体')).toHaveClass('session-step--running')
    expect(stepRow('安装中文字体').querySelector('.session-step-spinner')).toBeTruthy()
    expect(stepRow('检测操作系统').querySelector('.session-step-spinner')).toBeNull()
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
    expect(document.querySelector('.session-step-spinner')).toBeNull()
    expect(stepRow('安装中文字体')).not.toHaveClass('session-step--running')
    await user.click(screen.getByRole('button', { name: '重新准备' }))
    expect(await screen.findByRole('textbox', { name: '给小黑发消息' })).toBeInTheDocument()
  })

  it('只有进行中的步骤带执行指示，已完成、待处理和失败保持静止', () => {
    memoryBackend.bootstrap = {
      phase: 'preparing',
      cursor: 4,
      steps: [
        { id: 'detect_os', title: '检测操作系统', status: 'ok', duration_ms: 1 },
        { id: 'prepare_dirs', title: '准备数据与工作目录', status: 'degraded', duration_ms: 1 },
        { id: 'install_fonts', title: '安装中文字体', status: 'pending', duration_ms: 0 },
        { id: 'ensure_browser', title: '准备托管浏览器', status: 'running', duration_ms: 0 },
        { id: 'selfcheck', title: '自检浏览器', status: 'failed', duration_ms: 2 },
      ],
    }
    render(<App />)
    const running = stepRow('准备托管浏览器')
    expect(running).toHaveClass('session-step--running')
    expect(running).toHaveAttribute('aria-busy', 'true')
    expect(running.querySelectorAll('.session-step-spinner')).toHaveLength(1)
    for (const title of ['检测操作系统', '准备数据与工作目录', '安装中文字体', '自检浏览器']) {
      const row = stepRow(title)
      expect(row).not.toHaveClass('session-step--running')
      expect(row).not.toHaveAttribute('aria-busy')
      expect(row.querySelector('.session-step-spinner')).toBeNull()
    }
    expect(document.querySelectorAll('.session-step-spinner')).toHaveLength(1)
    expect(document.querySelector('.session-progress.session-step--running')).toBeNull()
  })
})
