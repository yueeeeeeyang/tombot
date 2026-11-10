import { clipboard } from 'electron'
import { spawn } from 'node:child_process'
import { constants } from 'node:fs'
import { lstat, open } from 'node:fs/promises'
import { basename, extname, isAbsolute } from 'node:path'
import { fileURLToPath } from 'node:url'

const MAX_FILES = 4
const MAX_BYTES = 32 * 1024 * 1024

/** 只读取系统剪贴板明确标记的本地文件；普通文本路径不会触发文件访问。 */
export async function readClipboardFiles(): Promise<
  Array<{ name: string; type: string; bytes: Uint8Array }>
> {
  const items = await clipboard.read()
  const paths: string[] = []
  for (const item of items) {
    const find = (format: string) =>
      item.types.find(
        (type) => type === format || type === `electron application/osclipboard;format="${format}"`,
      )
    const plist = process.platform === 'darwin' ? find('NSFilenamesPboardType') : undefined
    const uri = find('public.file-url') ?? find('text/uri-list')
    const windows = process.platform === 'win32' ? find('FileNameW') : undefined
    const format = plist ?? uri ?? windows
    if (!format) continue
    const blob = await item.getType(format)
    if (!('arrayBuffer' in blob)) throw new Error('剪贴板文件格式无效。')
    if (blob.size > 64 * 1024) throw new Error('剪贴板文件列表过长。')
    const encoded = Buffer.from(await blob.arrayBuffer())
    if (plist) paths.push(...(await decodeFileList(encoded)))
    else if (uri && /^file:/mu.test(encoded.toString('utf8')))
      paths.push(...parseClipboardFileUrls(encoded.toString('utf8')))
    else if (uri) continue
    else paths.push(...encoded.toString('utf16le').split('\0').filter(Boolean))
  }
  if (paths.length > MAX_FILES) throw new Error('一条消息最多 4 个附件。')
  const result = []
  let total = 0
  for (const path of [...new Set(paths)]) {
    if (!isAbsolute(path) || path.includes('\0')) throw new Error('剪贴板文件路径无效。')
    const source = await lstat(path)
    if (source.isSymbolicLink()) throw new Error('请粘贴原文件，不支持符号链接。')
    if (!source.isFile()) throw new Error('请粘贴普通文件，文件夹需要先压缩。')
    const file = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK)
    try {
      const stat = await file.stat()
      if (!stat.isFile()) throw new Error('请粘贴普通文件，文件夹需要先压缩。')
      if (stat.ino !== source.ino || stat.dev !== source.dev)
        throw new Error('文件已被替换，请重新复制。')
      total += stat.size
      if (total > MAX_BYTES) throw new Error('一次粘贴的附件总大小不能超过 32 MiB。')
      const buffer = Buffer.alloc(stat.size + 1)
      let bytesRead = 0
      while (bytesRead < buffer.length) {
        const chunk = await file.read(buffer, bytesRead, buffer.length - bytesRead, bytesRead)
        if (!chunk.bytesRead) break
        bytesRead += chunk.bytesRead
      }
      const after = await file.stat()
      if (bytesRead !== stat.size || after.size !== stat.size || after.mtimeMs !== stat.mtimeMs)
        throw new Error('文件正在变化，请重新复制后粘贴。')
      const types: Record<string, string> = {
        '.png': 'image/png',
        '.jpg': 'image/jpeg',
        '.jpeg': 'image/jpeg',
        '.webp': 'image/webp',
        '.pdf': 'application/pdf',
        '.txt': 'text/plain',
        '.md': 'text/markdown',
      }
      result.push({
        name: basename(path),
        type: types[extname(path).toLowerCase()] ?? 'application/octet-stream',
        bytes: new Uint8Array(buffer.subarray(0, bytesRead)),
      })
    } finally {
      await file.close()
    }
  }
  return result
}

/** 文件 URI 的主机必须是本机，防止普通网络地址变成文件读取。 */
export function parseClipboardFileUrls(value: string): string[] {
  return value
    .split(/\r?\n/u)
    .map((line) => line.trim())
    .filter((line) => line && !line.startsWith('#'))
    .map((line) => {
      const url = new URL(line)
      if (url.protocol !== 'file:' || (url.hostname && url.hostname !== 'localhost'))
        throw new Error('剪贴板中不是本地文件。')
      return fileURLToPath(url)
    })
}

/** Finder 使用系统 plist 编码；转换过程限制输入、输出、时间并检查退出码。 */
function decodeFileList(encoded: Buffer): Promise<string[]> {
  return new Promise((resolve, reject) => {
    const child = spawn('/usr/bin/plutil', ['-convert', 'json', '-o', '-', '--', '-'], {
      stdio: ['pipe', 'pipe', 'pipe'],
    })
    const chunks: Buffer[] = []
    let size = 0
    const timer = setTimeout(() => child.kill(), 2000)
    child.stdout.on('data', (chunk: Buffer) => {
      size += chunk.length
      if (size > 64 * 1024) child.kill()
      else chunks.push(chunk)
    })
    child.stderr.resume()
    child.once('error', (error) => {
      clearTimeout(timer)
      reject(error)
    })
    child.stdin.once('error', (error) => {
      child.kill()
      reject(error)
    })
    child.once('close', (code) => {
      clearTimeout(timer)
      if (code !== 0 || size > 64 * 1024) {
        reject(new Error('无法读取剪贴板文件列表，请重新复制。'))
        return
      }
      try {
        const paths: unknown = JSON.parse(Buffer.concat(chunks).toString('utf8'))
        if (!Array.isArray(paths) || !paths.every((path) => typeof path === 'string'))
          throw new Error('剪贴板文件列表无效。')
        resolve(paths)
      } catch (error) {
        reject(error)
      }
    })
    child.stdin.end(encoded)
  })
}
