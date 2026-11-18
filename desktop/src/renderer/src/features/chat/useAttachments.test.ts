import { act, renderHook } from '@testing-library/react'
import { expect, it, vi } from 'vitest'
import type { MessageAttachment } from '../workspace/model'
import { useAttachments } from './useAttachments'

function uploaded(file: File): MessageAttachment {
  return {
    name: file.name,
    kind: 'file',
    mediaType: file.type,
    path: 'chat-files/' + file.name,
    sha256: 'a'.repeat(64),
  }
}
it('选取、移除或超量均不上传；发送期间锁住草稿，阻止重复提交和并发添加', async () => {
  let finish!: (item: MessageAttachment) => void
  const upload = vi.fn(
    () =>
      new Promise<MessageAttachment>((resolve) => {
        finish = resolve
      }),
  )
  const submit = vi.fn(() => true)
  const { result } = renderHook(() => useAttachments(upload))
  const file = new File(['正文'], '资料.txt')
  await expect(result.current.addFiles(Array<File>(5).fill(file))).rejects.toThrow('最多 4')
  await act(() => result.current.addFiles([file, file]))
  act(() => result.current.removeFile(result.current.draftFiles[0]!.id))
  expect(upload).not.toHaveBeenCalled()
  let sending!: Promise<boolean>
  act(() => {
    sending = result.current.sendFiles(submit)
  })
  expect(result.current.isSending).toBe(true)
  await expect(result.current.addFiles([file])).rejects.toThrow('正在发送')
  await expect(result.current.sendFiles(submit)).resolves.toBe(false)
  expect(submit).not.toHaveBeenCalled()
  await act(async () => {
    finish(uploaded(file))
    await sending
  })
  expect(submit).toHaveBeenCalledExactlyOnceWith([uploaded(file)])
  expect(result.current.draftFiles).toEqual([])
  expect(result.current.isSending).toBe(false)
})
it('部分上传失败保留整批草稿，重试复用成功附件，消息提交失败也不重复上传', async () => {
  const upload = vi.fn(async (file: File) => uploaded(file))
  const first = new File(['a'], 'a.txt'),
    second = new File(['b'], 'b.txt')
  upload.mockResolvedValueOnce(uploaded(first)).mockRejectedValueOnce(new Error('上传失败'))
  const submit = vi.fn(() => true)
  const { result } = renderHook(() => useAttachments(upload))
  await act(() => result.current.addFiles([first, second]))
  await act(async () => {
    await expect(result.current.sendFiles(submit)).rejects.toThrow('上传失败')
  })
  expect(result.current.isSending).toBe(false)
  expect(result.current.draftFiles.map((entry) => entry.file)).toEqual([first, second])
  expect(submit).not.toHaveBeenCalled()
  await act(() => result.current.sendFiles(() => false).then(() => {}))
  expect(result.current.draftFiles).toHaveLength(2)
  await act(() => result.current.sendFiles(submit).then(() => {}))
  expect(upload.mock.calls.map(([file]) => file)).toEqual([first, second, second])
  expect(submit).toHaveBeenCalledExactlyOnceWith([uploaded(first), uploaded(second)])
  expect(result.current.draftFiles).toHaveLength(0)
})
