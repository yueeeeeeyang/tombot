import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { expect, it, vi } from 'vitest'
import { memoryBackend } from '../desktop/memoryBackend'
import { ClientUpdates, UpdatePanel } from './ClientUpdates'

it('自动提醒展示升级内容，稍后处理同版本不重复弹出，手动下载可重试', async () => {
  const release = { id: 'release', version: '1.10.0', notes: '改善离线重连\n修复消息排序' }
  const check = vi.spyOn(memoryBackend, 'checkClientUpdate').mockResolvedValue(release)
  const download = vi
    .spyOn(memoryBackend, 'downloadClientUpdate')
    .mockRejectedValueOnce(new Error('下载中断'))
    .mockResolvedValue(true)
  render(
    <ClientUpdates connected>
      <section aria-label="更新">
        <UpdatePanel />
      </section>
    </ClientUpdates>,
  )
  const dialog = await screen.findByRole('dialog', { name: '发现客户端新版本' })
  expect(within(dialog).getByText('TomBot 1.10.0')).toBeVisible()
  expect(dialog).toHaveTextContent('修复消息排序')
  fireEvent.click(within(dialog).getByRole('button', { name: '稍后再说' }))
  await act(async () => {
    window.dispatchEvent(new Event('online'))
  })
  expect(check).toHaveBeenCalledTimes(2)
  expect(screen.queryByRole('dialog')).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: '下载安装包' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('下载中断')
  fireEvent.click(screen.getByRole('button', { name: '下载安装包' }))
  expect(await screen.findByText(/安装包已保存/)).toBeVisible()
  expect(download).toHaveBeenCalledWith(release.id)
})
it('自动检查失败可在更新页重试，之后发现新版本会在定时检查时提醒', async () => {
  vi.useFakeTimers()
  try {
    const check = vi
      .spyOn(memoryBackend, 'checkClientUpdate')
      .mockRejectedValueOnce(new Error('服务不可用'))
      .mockResolvedValueOnce(null)
      .mockResolvedValue({ id: 'new', version: '1.2.3', notes: '新的内容' })
    render(
      <ClientUpdates connected>
        <UpdatePanel />
      </ClientUpdates>,
    )
    await act(async () => {
      await Promise.resolve()
    })
    expect(screen.getByRole('alert')).toHaveTextContent('服务不可用')
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: '检查更新' }))
    })
    expect(screen.getByText('当前已是最新版本')).toBeVisible()
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6 * 60 * 60 * 1000)
    })
    expect(screen.getByRole('dialog')).toHaveTextContent('新的内容')
    expect(check).toHaveBeenCalledTimes(3)
  } finally {
    vi.useRealTimers()
  }
})
it('切换源站后迟到的旧结果不触发提醒', async () => {
  let finish!: () => void
  vi.spyOn(memoryBackend, 'checkClientUpdate')
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = () => resolve({ id: 'old', version: '2.0.0', notes: '另一台服务' })
        }),
    )
    .mockResolvedValue(null)
  const view = render(
    <ClientUpdates key="first" connected>
      <UpdatePanel />
    </ClientUpdates>,
  )
  await waitFor(() => expect(finish).toBeDefined())
  view.rerender(
    <ClientUpdates key="second" connected>
      <UpdatePanel />
    </ClientUpdates>,
  )
  await act(async () => finish())
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(screen.getByText('当前已是最新版本')).toBeVisible()
})

it('立即升级使用自动安装通道，显示校验进度，重复点击合并，失败后可重试', async () => {
  const release = { id: 'first', version: '1.2.3', notes: '修复更新' }
  vi.spyOn(memoryBackend, 'checkClientUpdate').mockResolvedValue(release)
  const download = vi.spyOn(memoryBackend, 'downloadClientUpdate')
  let reject!: (error: Error) => void
  const install = vi
    .spyOn(memoryBackend, 'installClientUpdate')
    .mockImplementationOnce((_id, progress) => {
      progress('已下载并校验 50%')
      return new Promise((_resolve, fail) => {
        reject = fail
      })
    })
    .mockResolvedValue(true)
  render(
    <ClientUpdates connected>
      <span />
    </ClientUpdates>,
  )
  const dialog = await screen.findByRole('dialog')
  const button = within(dialog).getByRole('button', { name: '立即升级' })
  fireEvent.click(button)
  fireEvent.click(button)
  expect(button).toBeDisabled()
  expect(install).toHaveBeenCalledTimes(1)
  expect(install).toHaveBeenCalledWith(release.id, expect.any(Function))
  expect(within(dialog).getByRole('status')).toHaveTextContent('50%')
  await act(async () => reject(new Error('完整性校验失败')))
  expect(within(dialog).getByRole('alert')).toHaveTextContent('校验失败')
  fireEvent.click(button)
  expect(await within(dialog).findByText('正在安装并重启客户端…')).toBeVisible()
  expect(download).not.toHaveBeenCalled()
})

it('历史手动安装包显示原因并保留下载入口', async () => {
  vi.spyOn(memoryBackend, 'checkClientUpdate').mockResolvedValue({
    id: 'legacy',
    version: '2.0.0',
    notes: '更新',
    installUnavailable: '格式不支持自动安装',
  })
  render(
    <ClientUpdates connected>
      <span />
    </ClientUpdates>,
  )
  const dialog = await screen.findByRole('dialog')
  expect(within(dialog).getByRole('button', { name: '立即升级' })).toBeDisabled()
  expect(within(dialog).getByText('格式不支持自动安装')).toBeVisible()
  expect(within(dialog).getByRole('button', { name: '下载安装包' })).toBeEnabled()
})
