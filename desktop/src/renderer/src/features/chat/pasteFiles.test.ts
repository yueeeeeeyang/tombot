import type { ClipboardEvent } from 'react'
import { afterEach, expect, it, vi } from 'vitest'
import { waitFor } from '@testing-library/react'
import { memoryBackend, MemoryBackend } from '../desktop/memoryBackend'
import { pasteFiles } from './pasteFiles'

afterEach(() => {
  window.desktop = { platform: 'linux', backend: memoryBackend }
})
function paste(files: File[], types = ['Files'], text = '') {
  return {
    preventDefault: vi.fn(),
    clipboardData: { files, types, getData: () => text },
  } as unknown as ClipboardEvent
}
it('粘贴图片直接上传，普通文本和链接保留默认输入行为', async () => {
  const upload = vi.fn(async () => undefined)
  const error = vi.fn()
  const file = new File(['picture'], 'cat.png', { type: 'image/png' })
  const event = paste([file])
  pasteFiles(event, upload, error)
  expect(event.preventDefault).toHaveBeenCalled()
  expect(upload).toHaveBeenCalledWith([file])
  window.desktop = {
    platform: 'linux',
    backend: Object.assign(new MemoryBackend(), { readClipboardFiles: async () => [] }),
  }
  for (const type of ['text/plain', 'text/uri-list']) {
    const plain = paste([], [type], 'https://example.test')
    pasteFiles(plain, upload, error)
    expect(plain.preventDefault).not.toHaveBeenCalled()
  }
  await waitFor(() => expect(error).not.toHaveBeenCalled())
})
it('原生文件引用进入相同上传流程，读取或上传失败显示错误', async () => {
  const read = vi.fn(async () => [
    { name: '资料.txt', type: 'text/plain', bytes: new Uint8Array([1, 2]) },
  ])
  window.desktop = {
    platform: 'darwin',
    backend: Object.assign(new MemoryBackend(), { readClipboardFiles: read }),
  }
  const upload = vi.fn(async (files: File[]) => {
    expect(files[0]?.name).toBe('资料.txt')
    throw new Error('上传失败')
  })
  const error = vi.fn()
  pasteFiles(paste([]), upload, error)
  await waitFor(() => expect(error).toHaveBeenCalledWith('上传失败'))
  expect(upload.mock.calls[0]?.[0]).toBeDefined()
  read.mockRejectedValue(new Error('文件不可读取'))
  pasteFiles(paste([]), upload, error)
  await waitFor(() => expect(error).toHaveBeenCalledWith('文件不可读取'))
})
