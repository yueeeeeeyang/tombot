import { createHash } from 'node:crypto'
import { describe, expect, it, vi } from 'vitest'
import {
  deleteChatFile,
  listChatFiles,
  previewChatFile,
  previewMessageFile,
  readChatFile,
  renameChatFile,
  uploadChatFile,
} from './chatFiles'
import { chatFilePath, parseChatFilePath } from '../shared/chatFiles'
const conversationId = '01J00000000000000000000001',
  id = '01J00000000000000000000002'
const bytes = Buffer.from('共享资料')
const file = {
  id,
  conversation_id: conversationId,
  name: '资料.txt',
  media_type: 'text/plain; charset=utf-8',
  size: bytes.length,
  sha256: createHash('sha256').update(bytes).digest('hex'),
  revision: 1,
  created_at: '2026-11-17T00:00:00Z',
}
describe('群文件协议', () => {
  it('分页搜索正确编码且拒绝跨群返回', async () => {
    const call = vi.fn().mockResolvedValue({
      files: [file],
      next: '',
      used_bytes: 9,
      max_bytes: 100,
      max_file_bytes: 100,
    })
    expect((await listChatFiles(call, conversationId, '', '计划 & 100%')).files).toEqual([file])
    expect(call.mock.calls[0]?.[1]).toBe(
      `/conversations/${conversationId}/files?before=&q=%E8%AE%A1%E5%88%92+%26+100%25`,
    )
    call.mockResolvedValue({
      files: [{ ...file, conversation_id: id }],
      next: '',
      used_bytes: 9,
      max_bytes: 100,
      max_file_bytes: 100,
    })
    await expect(listChatFiles(call, conversationId)).rejects.toThrow('不匹配')
  })
  it('上传携带同一幂等标识并校验摘要，大小限制在请求前执行', async () => {
    const call = vi.fn().mockResolvedValue({ file })
    expect(await uploadChatFile(call, conversationId, { name: file.name, bytes }, id)).toEqual(file)
    expect(call).toHaveBeenCalledWith(
      'POST',
      `/conversations/${conversationId}/files`,
      { client_request_id: id, name: file.name, content_base64: bytes.toString('base64') },
      60000,
    )
    await expect(
      uploadChatFile(
        call,
        conversationId,
        { name: '过大', bytes: new Uint8Array(16 * 1024 * 1024 + 1) },
        id,
      ),
    ).rejects.toThrow('16 MB')
    expect(call).toHaveBeenCalledTimes(1)
    call.mockResolvedValue({ file: { ...file, sha256: '0'.repeat(64) } })
    await expect(
      uploadChatFile(call, conversationId, { name: file.name, bytes }, id),
    ).rejects.toThrow('校验失败')
  })
  it('下载校验内容完整性与归属，错误不转为空文件', async () => {
    const call = vi.fn().mockResolvedValue({ file, content_base64: bytes.toString('base64') })
    expect((await readChatFile(call, conversationId, id)).bytes).toEqual(bytes)
    call.mockResolvedValue({ file, content_base64: '' })
    await expect(readChatFile(call, conversationId, id)).rejects.toThrow('校验失败')
    call.mockRejectedValue(new Error('已删除'))
    await expect(readChatFile(call, conversationId, id)).rejects.toThrow('已删除')
  })
  it('纯文本预览不执行 HTML，长文本截断，二进制只展示元数据', async () => {
    const content = Buffer.from('<script>untrusted</script>' + '文'.repeat(30000))
    const record = {
      ...file,
      size: content.length,
      sha256: createHash('sha256').update(content).digest('hex'),
      media_type: 'text/html; charset=utf-8',
    }
    const call = vi
      .fn()
      .mockResolvedValue({ file: record, content_base64: content.toString('base64') })
    expect(await previewChatFile(call, conversationId, id)).toMatchObject({
      truncated: true,
      text: expect.stringContaining('<script>untrusted</script>'),
    })
    call.mockResolvedValue({
      file: { ...file, media_type: 'application/pdf' },
      content_base64: bytes.toString('base64'),
    })
    expect(await previewChatFile(call, conversationId, id)).toEqual({
      file: { ...file, media_type: 'application/pdf' },
    })
  })
  it('重命名删除携带版本，路径不能指定任意资源或本机文件', async () => {
    const call = vi.fn().mockResolvedValue({ file: { ...file, revision: 2, name: '新资料.txt' } })
    await renameChatFile(call, conversationId, id, 1, '新资料.txt')
    expect(call).toHaveBeenLastCalledWith('PUT', `/conversations/${conversationId}/files/${id}`, {
      expected_revision: 1,
      name: '新资料.txt',
    })
    await deleteChatFile(call, conversationId, id, 2)
    expect(call).toHaveBeenLastCalledWith(
      'DELETE',
      `/conversations/${conversationId}/files/${id}`,
      {
        expected_revision: 2,
      },
    )
    expect(parseChatFilePath(chatFilePath(file))).toEqual({ conversationId, fileId: id })
    expect(() => parseChatFilePath('../../private')).toThrow('无效')
    await expect(readChatFile(call, '../other', id)).rejects.toThrow('无效')
  })
})

it('按会话校验消息预览，只返回受支持的媒体类型', async () => {
  for (const [name, declared, expected] of [
    ['图片.webp', 'image/webp', 'image/webp'],
    ['语音.wav', 'audio/x-wav', 'audio/wav'],
    ['录音.m4a', 'video/mp4', 'audio/mp4'],
    ['影片.webm', 'video/webm', 'video/webm'],
    ['旧录音.mp3', 'application/octet-stream', 'audio/mpeg'],
  ]) {
    const call = vi.fn().mockResolvedValue({
      file: { ...file, name, media_type: declared },
      content_base64: bytes.toString('base64'),
    })
    await expect(previewMessageFile(call, chatFilePath(file))).resolves.toBe(
      `data:${expected};base64,${bytes.toString('base64')}`,
    )
  }
  for (const [name, type] of [
    ['网页.mp4', 'text/html'],
    ['绘图.svg', 'image/svg+xml'],
    ['文档.docx', 'application/octet-stream'],
  ]) {
    const call = vi.fn().mockResolvedValue({
      file: { ...file, name, media_type: type },
      content_base64: bytes.toString('base64'),
    })
    await expect(previewMessageFile(call, chatFilePath(file))).rejects.toThrow('此格式')
  }
  const corrupt = vi.fn().mockResolvedValue({
    file: { ...file, name: '录音.mp3', media_type: 'audio/mpeg' },
    content_base64: '',
  })
  await expect(previewMessageFile(corrupt, chatFilePath(file))).rejects.toThrow('校验失败')
})
