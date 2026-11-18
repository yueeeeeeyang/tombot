import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import * as desktopBackend from '../desktop/backend'
import type { UserEvent } from '../desktop/backend'
import { memoryBackend } from '../desktop/memoryBackend'
import { ChatLibrary } from './ChatLibrary'
import type { ChatFile, ChatFilePage } from '../../../../shared/chatFiles'
const file: ChatFile = {
  id: 'file',
  conversation_id: 'group',
  name: '方案.txt',
  size: 6,
  sha256: 'a'.repeat(64),
  media_type: 'text/plain',
  revision: 1,
  created_at: '2026-11-17T00:00:00Z',
}
const page = (files: ChatFile[] = [], next = ''): ChatFilePage => ({
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
      .spyOn(memoryBackend, 'listChatFiles')
      .mockResolvedValueOnce(page([file], 'cursor'))
      .mockResolvedValueOnce(page([file, { ...file, id: 'second', name: '草图.png' }]))
      .mockResolvedValue(page())
    render(<ChatLibrary conversationId="group" />)
    expect(await screen.findByText('方案.txt')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    expect(await screen.findByText('草图.png')).toBeInTheDocument()
    expect(screen.getAllByText('方案.txt')).toHaveLength(1)
    expect(load).toHaveBeenLastCalledWith('group', 'cursor', '')
    fireEvent.change(screen.getByRole('textbox', { name: '搜索聊天文件' }), {
      target: { value: '不存在' },
    })
    fireEvent.submit(screen.getByRole('search'))
    expect(await screen.findByText('没有找到匹配的文件。')).toBeInTheDocument()
    expect(load).toHaveBeenLastCalledWith('group', '', '不存在')
  })
  it('读取失败不能伪装为空，重试保留原游标', async () => {
    const load = vi
      .spyOn(memoryBackend, 'listChatFiles')
      .mockResolvedValueOnce(page([file], 'next'))
      .mockRejectedValueOnce(new Error('读取失败'))
      .mockResolvedValue(page())
    render(<ChatLibrary conversationId="group" />)
    await screen.findByText('方案.txt')
    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('读取失败')
    expect(screen.getByText('方案.txt')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull())
    expect(load).toHaveBeenLastCalledWith('group', 'next', '')
  })
  it('切换群组丢弃迟到响应和旧文件', async () => {
    let complete!: (value: ChatFilePage) => void
    vi.spyOn(memoryBackend, 'listChatFiles')
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            complete = resolve
          }),
      )
      .mockResolvedValue(page([{ ...file, id: 'new', name: '新群.txt', conversation_id: 'new' }]))
    const view = render(<ChatLibrary conversationId="old" />)
    await waitFor(() => expect(complete).toBeDefined())
    view.rerender(<ChatLibrary conversationId="new" />)
    await screen.findByText('新群.txt')
    await act(async () => complete(page([file])))
    expect(screen.queryByText('方案.txt')).toBeNull()
  })
  it('预览文本、下载取消、重命名冲突和确认删除', async () => {
    dialogs()
    vi.spyOn(memoryBackend, 'listChatFiles').mockResolvedValue(page([file]))
    vi.spyOn(memoryBackend, 'previewChatFile').mockResolvedValue({
      file,
      text: '<script>只是文件内容</script>',
    })
    const download = vi.spyOn(memoryBackend, 'downloadChatFile').mockResolvedValue(false)
    const rename = vi
      .spyOn(memoryBackend, 'renameChatFile')
      .mockRejectedValueOnce(new Error('文件已被修改'))
      .mockResolvedValue()
    const remove = vi.spyOn(memoryBackend, 'deleteChatFile').mockResolvedValue()
    render(<ChatLibrary conversationId="group" />)
    fireEvent.click(await screen.findByRole('button', { name: '预览 方案.txt' }))
    expect(await screen.findByText('<script>只是文件内容</script>')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '下载文件' }))
    await waitFor(() => expect(download).toHaveBeenCalledWith('group', 'file'))
    expect(screen.queryByText('文件已保存。')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }))
    fireEvent.click(screen.getByRole('button', { name: '更多操作 方案.txt' }))
    fireEvent.click(screen.getByRole('menuitem', { name: '重命名 方案.txt' }))
    fireEvent.change(screen.getByRole('textbox', { name: '文件名称' }), {
      target: { value: '新版.txt' },
    })
    fireEvent.click(screen.getByRole('button', { name: '保存名称' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('文件已被修改')
    expect(rename).toHaveBeenCalledWith('group', 'file', 1, '新版.txt')
    fireEvent.click(screen.getByRole('button', { name: '取消' }))
    fireEvent.click(screen.getByRole('button', { name: '更多操作 方案.txt' }))
    fireEvent.click(screen.getByRole('menuitem', { name: '删除 方案.txt' }))
    expect(remove).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: '确认删除' }))
    await waitFor(() => expect(remove).toHaveBeenCalledWith('group', 'file', 1))
  })
  it('只收录聊天文件，搜索按钮在搜索框内且没有上传入口', async () => {
    vi.spyOn(memoryBackend, 'listChatFiles').mockResolvedValue(page([file]))
    render(<ChatLibrary conversationId="group" />)
    await screen.findByText('方案.txt')
    expect(screen.queryByRole('button', { name: '上传文件' })).toBeNull()
    expect(document.querySelector('input[type="file"]')).toBeNull()
    expect(
      within(screen.getByRole('search')).getByRole('button', { name: '搜索文件' }),
    ).toBeInTheDocument()
  })
  it.each([{ files: [] }, { files: [file] }])(
    '文件列表或空状态不再展示收录说明与容量信息：%j',
    async ({ files }) => {
      vi.spyOn(memoryBackend, 'listChatFiles').mockResolvedValue(page(files))
      render(<ChatLibrary conversationId="group" />)
      if (files.length) await screen.findByText('方案.txt')
      else await screen.findByText('资料库还是空的。在聊天中发送文件后会显示在这里。')
      expect(screen.queryByText('聊天中你和猫咪发出的文件会自动收录。')).toBeNull()
      expect(screen.queryByText(/^已用 /)).toBeNull()
      expect(screen.queryByText(/256\.0 MB/)).toBeNull()
      expect(screen.getByRole('textbox', { name: '搜索聊天文件' })).toBeInTheDocument()
    },
  )
  it('预览失败可重试', async () => {
    dialogs()
    vi.spyOn(memoryBackend, 'listChatFiles').mockResolvedValue(page([file]))
    const preview = vi
      .spyOn(memoryBackend, 'previewChatFile')
      .mockRejectedValueOnce(new Error('文件已删除'))
      .mockResolvedValue({ file, text: '已恢复' })
    render(<ChatLibrary conversationId="group" />)
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
      .spyOn(memoryBackend, 'listChatFiles')
      .mockResolvedValueOnce(page([file]))
      .mockResolvedValue(page([{ ...file, name: '实时更新.txt' }]))
    const view = render(<ChatLibrary conversationId="group" />)
    await screen.findByText('方案.txt')
    const event = (group: string): UserEvent => ({
      sequence: 1,
      kind: 'chat.library.changed',
      resource_id: group,
      payload: { conversation_id: group },
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
    vi.spyOn(memoryBackend, 'listChatFiles').mockResolvedValue(page([file]))
    vi.spyOn(memoryBackend, 'previewChatFile').mockResolvedValue({
      file,
      image: 'data:image/png;base64,broken',
    })
    render(<ChatLibrary conversationId="group" />)
    fireEvent.click(await screen.findByRole('button', { name: '预览 方案.txt' }))
    fireEvent.error(await screen.findByRole('img', { name: '方案.txt' }))
    expect(screen.getByRole('alert')).toHaveTextContent('图片内容无法预览')
    expect(screen.getByRole('button', { name: '下载文件' })).toBeEnabled()
    expect(screen.queryByRole('img', { name: '方案.txt' })).toBeNull()
  })
})
