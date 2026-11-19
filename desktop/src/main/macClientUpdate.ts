import type { AutoUpdater } from 'electron'
import { randomUUID } from 'node:crypto'
import { createReadStream } from 'node:fs'
import { writeFile } from 'node:fs/promises'
import { dirname, join } from 'node:path'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import { createServer } from 'node:http'
import { once } from 'node:events'
import { pipeline } from 'node:stream/promises'
import type { ClientRelease } from './clientUpdates'

/** 限长读取 ZIP 内的应用清单，避免将误传的产品或版本交给系统签名更新器。 */
export async function verifyMacBundle(file: string, version: string) {
  const run = promisify(execFile)
  const plist = join(dirname(file), 'Info.plist')
  const limits = { timeout: 30_000, maxBuffer: 256 * 1024 }
  try {
    const { stdout } = await run('/usr/bin/unzip', ['-p', file, 'TomBot.app/Contents/Info.plist'], {
      ...limits,
      encoding: 'buffer',
    })
    await writeFile(plist, stdout, { flag: 'wx', mode: 0o600 })
    const result = await run('/usr/bin/plutil', ['-convert', 'json', '-o', '-', plist], limits)
    const metadata = JSON.parse(result.stdout)
    if (
      metadata.CFBundleIdentifier === 'com.tombot.desktop' &&
      metadata.CFBundleShortVersionString === version
    )
      return
  } catch {
    throw new Error('无法读取 macOS 更新包中的 TomBot 应用清单。')
  }
  throw new Error('macOS 更新包的应用标识或实际版本与发布信息不符。')
}

/** 仅向本机 Squirrel 提供已校验的 ZIP。系统确认签名和暂存成功后才能退出。 */
export async function stageMacUpdate(updater: AutoUpdater, release: ClientRelease, file: string) {
  const route = '/' + randomUUID()
  let origin = ''
  const server = createServer((request, response) => {
    if (request.method !== 'GET' || request.headers.host !== origin.slice(7)) {
      response.writeHead(403).end()
    } else if (request.url === route) {
      response.writeHead(200, { 'Content-Type': 'application/json' }).end(
        JSON.stringify({
          url: origin + route + '/update.zip',
          name: release.version,
          notes: release.notes,
        }),
      )
    } else if (request.url === route + '/update.zip') {
      response.writeHead(200, { 'Content-Type': 'application/zip', 'Content-Length': release.size })
      void pipeline(createReadStream(file), response).catch(() => response.destroy())
    } else {
      response.writeHead(404).end()
    }
  })
  server.setTimeout(60_000, (socket) => socket.destroy())
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), 5 * 60_000)
  try {
    const listening = once(server, 'listening', { signal: controller.signal })
    server.listen(0, '127.0.0.1')
    await listening
    const address = server.address()
    if (!address || typeof address === 'string') throw new Error('无法启动本机升级服务。')
    origin = 'http://127.0.0.1:' + address.port
    updater.setFeedURL({ url: origin + route, serverType: 'default' })
    // 先监听，避免系统在请求返回前已经报告签名错误或准备完成。
    await new Promise<void>((resolve, reject) => {
      const ready = () => finish()
      const failed = () =>
        finish(new Error('macOS 未能验证或暂存更新，请确认应用签名和安装目录的写入权限。'))
      const missing = () => finish(new Error('macOS 未识别到可安装的新版本。'))
      const expired = () => finish(new Error('系统准备升级超时，请重试。'))
      function finish(error?: Error) {
        updater.off('update-downloaded', ready)
        updater.off('error', failed)
        updater.off('update-not-available', missing)
        controller.signal.removeEventListener('abort', expired)
        if (error) reject(error)
        else resolve()
      }
      updater.once('update-downloaded', ready)
      updater.once('error', failed)
      updater.once('update-not-available', missing)
      controller.signal.addEventListener('abort', expired, { once: true })
      try {
        updater.checkForUpdates()
      } catch {
        failed()
      }
    })
  } finally {
    clearTimeout(timeout)
    if (server.listening) {
      const closed = once(server, 'close')
      server.close()
      server.closeAllConnections()
      await closed
    }
  }
}
