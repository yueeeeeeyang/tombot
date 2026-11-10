import { act, renderHook } from '@testing-library/react'
import { expect, it, vi } from 'vitest'
import { useAttachments } from './useAttachments'
import type { MessageAttachment } from '../workspace/model'

const attachment: MessageAttachment = {
  name: '资料.txt',
  kind: 'file',
  mediaType: 'text/plain',
  path: 'shared/a.txt',
  sha256: 'a'.repeat(64),
}
it('拒绝超量批次与并发上传，失败保留已上传附件并解除锁定', async () => {
  let finish!: (item: MessageAttachment) => void
  const upload = vi.fn(
    () =>
      new Promise<MessageAttachment>((resolve) => {
        finish = resolve
      }),
  )
  const { result } = renderHook(() => useAttachments(upload))
  const file = new File(['内容'], '资料.txt')
  await expect(result.current.addFiles(Array<File>(5).fill(file))).rejects.toThrow('最多 4')
  expect(upload).not.toHaveBeenCalled()
  let pending!: Promise<void>
  act(() => {
    pending = result.current.addFiles([file])
  })
  expect(result.current.isUploading).toBe(true)
  await expect(result.current.addFiles([file])).rejects.toThrow('正在上传')
  await act(async () => {
    finish(attachment)
    await pending
  })
  expect(result.current.pending).toEqual([attachment])
  expect(result.current.isUploading).toBe(false)
  upload.mockRejectedValueOnce(new Error('失败'))
  await act(async () => {
    await expect(result.current.addFiles([file])).rejects.toThrow('失败')
  })
  expect(result.current.pending).toEqual([attachment])
  expect(result.current.isUploading).toBe(false)
})
