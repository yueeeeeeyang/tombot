import { createHash } from 'node:crypto'
import { describe, expect, it, vi } from 'vitest'
import {
  deleteGroupFile,
  listGroupFiles,
  previewGroupFile,
  readGroupFile,
  renameGroupFile,
  uploadGroupFile,
} from './groupFiles'
import { groupFilePath, parseGroupFilePath } from '../shared/groupFiles'
const groupId = '01J00000000000000000000001',
  id = '01J00000000000000000000002'
const bytes = Buffer.from('共享资料')
const file = {
  id,
  group_id: groupId,
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
    expect((await listGroupFiles(call, groupId, '', '计划 & 100%')).files).toEqual([file])
    expect(call.mock.calls[0]?.[1]).toBe(
      `/groups/${groupId}/files?before=&q=%E8%AE%A1%E5%88%92+%26+100%25`,
    )
    call.mockResolvedValue({
      files: [{ ...file, group_id: id }],
      next: '',
      used_bytes: 9,
      max_bytes: 100,
      max_file_bytes: 100,
    })
    await expect(listGroupFiles(call, groupId)).rejects.toThrow('不匹配')
  })
  it('上传携带同一幂等标识并校验摘要，大小限制在请求前执行', async () => {
    const call = vi.fn().mockResolvedValue({ file })
    expect(await uploadGroupFile(call, groupId, { name: file.name, bytes }, id)).toEqual(file)
    expect(call).toHaveBeenCalledWith(
      'POST',
      `/groups/${groupId}/files`,
      { client_request_id: id, name: file.name, content_base64: bytes.toString('base64') },
      60000,
    )
    await expect(
      uploadGroupFile(
        call,
        groupId,
        { name: '过大', bytes: new Uint8Array(16 * 1024 * 1024 + 1) },
        id,
      ),
    ).rejects.toThrow('16 MB')
    expect(call).toHaveBeenCalledTimes(1)
    call.mockResolvedValue({ file: { ...file, sha256: '0'.repeat(64) } })
    await expect(uploadGroupFile(call, groupId, { name: file.name, bytes }, id)).rejects.toThrow(
      '校验失败',
    )
  })
  it('下载校验内容完整性与归属，错误不转为空文件', async () => {
    const call = vi.fn().mockResolvedValue({ file, content_base64: bytes.toString('base64') })
    expect((await readGroupFile(call, groupId, id)).bytes).toEqual(bytes)
    call.mockResolvedValue({ file, content_base64: '' })
    await expect(readGroupFile(call, groupId, id)).rejects.toThrow('校验失败')
    call.mockRejectedValue(new Error('已删除'))
    await expect(readGroupFile(call, groupId, id)).rejects.toThrow('已删除')
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
    expect(await previewGroupFile(call, groupId, id)).toMatchObject({
      truncated: true,
      text: expect.stringContaining('<script>untrusted</script>'),
    })
    call.mockResolvedValue({
      file: { ...file, media_type: 'application/pdf' },
      content_base64: bytes.toString('base64'),
    })
    expect(await previewGroupFile(call, groupId, id)).toEqual({
      file: { ...file, media_type: 'application/pdf' },
    })
  })
  it('重命名删除携带版本，路径不能指定任意资源或本机文件', async () => {
    const call = vi.fn().mockResolvedValue({ file: { ...file, revision: 2, name: '新资料.txt' } })
    await renameGroupFile(call, groupId, id, 1, '新资料.txt')
    expect(call).toHaveBeenLastCalledWith('PUT', `/groups/${groupId}/files/${id}`, {
      expected_revision: 1,
      name: '新资料.txt',
    })
    await deleteGroupFile(call, groupId, id, 2)
    expect(call).toHaveBeenLastCalledWith('DELETE', `/groups/${groupId}/files/${id}`, {
      expected_revision: 2,
    })
    expect(parseGroupFilePath(groupFilePath(file))).toEqual({ groupId, fileId: id })
    expect(() => parseGroupFilePath('../../private')).toThrow('无效')
    await expect(readGroupFile(call, '../other', id)).rejects.toThrow('无效')
  })
})
