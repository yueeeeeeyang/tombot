import { Blob } from 'node:buffer'
import { mkdtemp, mkdir, rm, symlink, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'
import { afterEach, expect, it, vi } from 'vitest'
const read = vi.hoisted(() => vi.fn())
vi.mock('electron', () => ({ clipboard: { read } }))
import { parseClipboardFileUrls, readClipboardFiles } from './clipboardFiles'

const directories: string[] = []
afterEach(async () => {
  for (const path of directories.splice(0)) await rm(path, { recursive: true, force: true })
})
function clipboardItem(type: string, value: string) {
  return { types: [type], getType: vi.fn(async () => new Blob([value])) }
}
async function fixture() {
  const directory = await mkdtemp(join(tmpdir(), 'tombot-clipboard-'))
  directories.push(directory)
  const file = join(directory, '资料.md')
  await writeFile(file, '测试附件')
  return { directory, file }
}

it('只读取剪贴板中的文件引用，保留文件名、内容与顺序', async () => {
  const { file } = await fixture()
  read.mockResolvedValue([clipboardItem('text/uri-list', pathToFileURL(file).href)])
  const files = await readClipboardFiles()
  expect(files).toHaveLength(1)
  expect(files[0]?.name).toBe('资料.md')
  expect(Buffer.from(files[0]!.bytes).toString()).toBe('测试附件')
  read.mockResolvedValue([clipboardItem('text/plain', file)])
  expect(await readClipboardFiles()).toEqual([])
  read.mockResolvedValue([clipboardItem('text/uri-list', 'https://example.test')])
  expect(await readClipboardFiles()).toEqual([])
})

it('拒绝远程文件地址、文件夹、链接、过多文件及超大批次', async () => {
  expect(() => parseClipboardFileUrls('file://remote/share/a.txt')).toThrow('本地文件')
  const { file, directory } = await fixture()
  const folder = join(directory, 'folder')
  await mkdir(folder)
  read.mockResolvedValue([clipboardItem('text/uri-list', pathToFileURL(folder).href)])
  await expect(readClipboardFiles()).rejects.toThrow('文件夹')
  const link = join(directory, 'link')
  await symlink(file, link)
  read.mockResolvedValue([clipboardItem('text/uri-list', pathToFileURL(link).href)])
  await expect(readClipboardFiles()).rejects.toThrow()
  read.mockResolvedValue([
    clipboardItem('text/uri-list', Array(5).fill(pathToFileURL(file).href).join('\n')),
  ])
  await expect(readClipboardFiles()).rejects.toThrow('最多 4')
  await writeFile(file, Buffer.alloc(32 * 1024 * 1024 + 1))
  read.mockResolvedValue([clipboardItem('text/uri-list', pathToFileURL(file).href)])
  await expect(readClipboardFiles()).rejects.toThrow('32 MiB')
})
