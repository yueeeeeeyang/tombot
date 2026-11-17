import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import * as desktopBackend from '../desktop/backend'
import type { UserEvent } from '../desktop/backend'
import { memoryBackend } from '../desktop/memoryBackend'
import { GroupLibrary } from './GroupLibrary'
import type { GroupFile, GroupFilePage } from '../../../../shared/groupFiles'
const file: GroupFile = {
  id: 'file',
  group_id: 'group',
  name: '方案.txt',
  size: 6,
  sha256: 'a'.repeat(64),
  media_type: 'text/plain',
  revision: 1,
  created_at: '2026-11-17T00:00:00Z',
}
const page = (files: GroupFile[] = [], next = ''): GroupFilePage => ({
  files,
  next,
  used_bytes: files.length * 6,
  max_bytes: 256 * 1024 * 1024,
  max_file_bytes: 16 * 1024 * 1024,
})
function dialogs() {
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) {
    this.setAttribute('open', '')
  })
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) {
    this.removeAttribute('open')
  })
}
describe('群资料库', () => {
  it('按游标分页去重，搜索从第一页重新开始', async () => {
    const load = vi
      .spyOn(memoryBackend, 'listGroupFiles')
      .mockResolvedValueOnce(page([file], 'cursor'))
      .mockResolvedValueOnce(page([file, { ...file, id: 'second', name: '草图.png' }]))
      .mockResolvedValue(page())
    render(<GroupLibrary groupId="group" />)
    expect(await screen.findByText('方案.txt')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    expect(await screen.findByText('草图.png')).toBeInTheDocument()
    expect(screen.getAllByText('方案.txt')).toHaveLength(1)
    expect(load).toHaveBeenLastCalledWith('group', 'cursor', '')
    fireEvent.change(screen.getByRole('textbox', { name: '搜索群文件' }), {
      target: { value: '不存在' },
    })
    fireEvent.submit(screen.getByRole('search'))
    expect(await screen.findByText('没有找到匹配的文件。')).toBeInTheDocument()
    expect(load).toHaveBeenLastCalledWith('group', '', '不存在')
  })
  it('读取失败不能伪装为空，重试保留原游标', async () => {
    const load = vi
      .spyOn(memoryBackend, 'listGroupFiles')
      .mockResolvedValueOnce(page([file], 'next'))
      .mockRejectedValueOnce(new Error('读取失败'))
      .mockResolvedValue(page())
    render(<GroupLibrary groupId="group" />)
    await screen.findByText('方案.txt')
    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('读取失败')
    expect(screen.getByText('方案.txt')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull())
    expect(load).toHaveBeenLastCalledWith('group', 'next', '')
  })
  it('切换群组丢弃迟到响应和旧文件', async () => {
    let complete!: (value: GroupFilePage) => void
    vi.spyOn(memoryBackend, 'listGroupFiles')
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            complete = resolve
          }),
      )
      .mockResolvedValue(page([{ ...file, id: 'new', name: '新群.txt', group_id: 'new' }]))
    const view = render(<GroupLibrary groupId="old" />)
    await waitFor(() => expect(complete).toBeDefined())
    view.rerender(<GroupLibrary groupId="new" />)
    await screen.findByText('新群.txt')
    await act(async () => complete(page([file])))
    expect(screen.queryByText('方案.txt')).toBeNull()
  })
  it('预览文本、下载取消、重命名冲突和确认删除', async () => {
    dialogs()
    vi.spyOn(memoryBackend, 'listGroupFiles').mockResolvedValue(page([file]))
    vi.spyOn(memoryBackend, 'previewGroupFile').mockResolvedValue({
      file,
      text: '<script>只是文件内容</script>',
    })
    const download = vi.spyOn(memoryBackend, 'downloadGroupFile').mockResolvedValue(false)
    const rename = vi
      .spyOn(memoryBackend, 'renameGroupFile')
      .mockRejectedValueOnce(new Error('文件已被修改'))
      .mockResolvedValue()
    const remove = vi.spyOn(memoryBackend, 'deleteGroupFile').mockResolvedValue()
    render(<GroupLibrary groupId="group" />)
    fireEvent.click(await screen.findByRole('button', { name: '预览 方案.txt' }))
    expect(await screen.findByText('<script>只是文件内容</script>')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '下载文件' }))
    await waitFor(() => expect(download).toHaveBeenCalledWith('group', 'file'))
    expect(screen.queryByText('文件已保存。')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }))
    fireEvent.click(screen.getByRole('button', { name: '重命名 方案.txt' }))
    fireEvent.change(screen.getByRole('textbox', { name: '文件名称' }), {
      target: { value: '新版.txt' },
    })
    fireEvent.click(screen.getByRole('button', { name: '保存名称' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('文件已被修改')
    expect(rename).toHaveBeenCalledWith('group', 'file', 1, '新版.txt')
    fireEvent.click(screen.getByRole('button', { name: '取消' }))
    fireEvent.click(screen.getByRole('button', { name: '删除 方案.txt' }))
    expect(remove).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: '确认删除' }))
    await waitFor(() => expect(remove).toHaveBeenCalledWith('group', 'file', 1))
  })
  it('上传失败保留错误和幂等标识，重试成功后刷新', async () => {
    vi.spyOn(memoryBackend, 'listGroupFiles').mockResolvedValue(page())
    const upload = vi
      .spyOn(memoryBackend, 'uploadGroupFile')
      .mockRejectedValueOnce(new Error('连接中断'))
      .mockResolvedValue(file)
    const selected = new File(['资料'], '资料.txt', { type: 'text/plain' })
    Object.defineProperty(selected, 'arrayBuffer', {
      value: () => Promise.resolve(new TextEncoder().encode('资料').buffer),
    })
    render(<GroupLibrary groupId="group" />)
    fireEvent.change(screen.getByLabelText('选择群文件'), { target: { files: [selected] } })
    expect(await screen.findByRole('alert')).toHaveTextContent('连接中断')
    fireEvent.click(screen.getByRole('button', { name: '重试上传（1）' }))
    await screen.findByText('已上传 1 个文件')
    expect(upload.mock.calls[0]?.[2]).toBe(upload.mock.calls[1]?.[2])
    expect(screen.queryByText('连接中断')).toBeNull()
  })
  it('过大文件不上传，预览失败可重试', async () => {
    dialogs()
    vi.spyOn(memoryBackend, 'listGroupFiles').mockResolvedValue(page([file]))
    const upload = vi.spyOn(memoryBackend, 'uploadGroupFile')
    const preview = vi
      .spyOn(memoryBackend, 'previewGroupFile')
      .mockRejectedValueOnce(new Error('文件已删除'))
      .mockResolvedValue({ file, text: '已恢复' })
    const selected = new File([], '过大.txt')
    Object.defineProperty(selected, 'size', { value: 16 * 1024 * 1024 + 1 })
    render(<GroupLibrary groupId="group" />)
    fireEvent.change(screen.getByLabelText('选择群文件'), { target: { files: [selected] } })
    expect(screen.getByRole('alert')).toHaveTextContent('单个文件最多 16 MB')
    expect(upload).not.toHaveBeenCalled()
    fireEvent.click(await screen.findByRole('button', { name: '预览 方案.txt' }))
    const dialog = screen.getByRole('dialog', { name: '文件预览', hidden: true })
    expect(await within(dialog).findByText('文件已删除')).toBeInTheDocument()
    fireEvent.click(within(dialog).getByText('重试预览'))
    await screen.findByText('已恢复')
    expect(preview).toHaveBeenCalledTimes(2)
  })
  it('群事件及重连刷新，其他群事件不触发读取，卸载取消订阅', async () => {
    let receive!: (event: UserEvent) => void
    const stop = vi.fn()
    const backend = Object.create(memoryBackend) as desktopBackend.DesktopBackend
    backend.watchEvents = vi.fn((callback) => {
      receive = callback
      return stop
    })
    vi.spyOn(desktopBackend, 'getBackend').mockReturnValue(backend)
    const load = vi
      .spyOn(memoryBackend, 'listGroupFiles')
      .mockResolvedValueOnce(page([file]))
      .mockResolvedValue(page([{ ...file, name: '实时更新.txt' }]))
    const view = render(<GroupLibrary groupId="group" />)
    await screen.findByText('方案.txt')
    const event = (group: string): UserEvent => ({
      sequence: 1,
      kind: 'group.library.changed',
      resource_id: group,
      payload: { group_id: group },
      created_at: file.created_at,
    })
    act(() => receive(event('other')))
    expect(load).toHaveBeenCalledTimes(1)
    act(() => receive(event('group')))
    await screen.findByText('实时更新.txt')
    expect(load).toHaveBeenCalledTimes(2)
    act(() => receive({ ...event(''), kind: 'workspace.refresh' }))
    await waitFor(() => expect(load).toHaveBeenCalledTimes(3))
    view.unmount()
    expect(stop).toHaveBeenCalledTimes(1)
  })
  it('图片解码失败显示明确错误，仍可下载原文件', async () => {
    dialogs()
    vi.spyOn(memoryBackend, 'listGroupFiles').mockResolvedValue(page([file]))
    vi.spyOn(memoryBackend, 'previewGroupFile').mockResolvedValue({
      file,
      image: 'data:image/png;base64,broken',
    })
    render(<GroupLibrary groupId="group" />)
    fireEvent.click(await screen.findByRole('button', { name: '预览 方案.txt' }))
    fireEvent.error(await screen.findByRole('img', { name: '方案.txt' }))
    expect(screen.getByRole('alert')).toHaveTextContent('图片内容无法预览')
    expect(screen.getByRole('button', { name: '下载文件' })).toBeEnabled()
    expect(screen.queryByRole('img', { name: '方案.txt' })).toBeNull()
  })
})
