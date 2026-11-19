import { mkdtemp, open, readFile, rename, rm } from 'node:fs/promises'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import { dirname, isAbsolute, join } from 'node:path'
import { app, autoUpdater } from 'electron'
import { saveClientUpdate, type ClientRelease } from './clientUpdates'
import { stageMacUpdate, verifyMacBundle } from './macClientUpdate'
import { prepareWindowsUpdate } from './windowsClientUpdate'

export type FinishUpdate = (install: () => Promise<void>) => Promise<void>
type Environment = {
  packaged: boolean
  appImage?: string
  platform: NodeJS.Platform
}

/** 根据当前连接的传输协议和本机安装格式判断是否支持立即升级。 */
export function installationIssue(
  env: Environment,
  origin: string,
  release: ClientRelease,
): string | undefined {
  if (!env.packaged) return '当前为开发版，正式安装的客户端支持立即升级。'
  if (!origin.startsWith('https://')) return '立即升级需要通过 HTTPS 连接当前服务端。'
  if (env.platform === 'darwin' && release.extension === '.zip') return
  if (env.platform === 'win32' && release.extension === '.exe') return
  if (
    env.platform === 'linux' &&
    release.extension === '.AppImage' &&
    env.appImage &&
    isAbsolute(env.appImage)
  )
    return
  return '此安装格式不支持立即升级，可单独下载安装包。'
}

/** 读取本机安装形态，供检查更新和安装时使用同一套判断。 */
export function installedEnvironment(): Environment {
  return {
    packaged: app.isPackaged,
    platform: process.platform,
    appImage: process.env.APPIMAGE,
  }
}

/** AppImage 以 ELF 标记和架构区分，同盘原子替换能在写入失败时保留运行版本。 */
export async function verifyAppImage(file: string, arch: string, version: string) {
  const handle = await open(file, 'r')
  try {
    const header = Buffer.alloc(20)
    const { bytesRead } = await handle.read(header)
    const machine = arch === 'arm64' ? 183 : arch === 'x64' ? 62 : 0
    if (
      !machine ||
      bytesRead !== header.length ||
      header.readUInt32BE(0) !== 0x7f454c46 ||
      header[4] !== 2 ||
      header[5] !== 1 ||
      header.readUIntBE(8, 3) !== 0x414902 ||
      header.readUInt16LE(18) !== machine
    )
      throw new Error('AppImage 安装包格式或处理器架构校验失败。')
  } finally {
    await handle.close()
  }
  // 只提取发行入口以核对产品与版本，准备安装时无需启动第二个图形客户端。
  const directory = dirname(file)
  try {
    await promisify(execFile)(file, ['--appimage-extract', 'tombot-desktop.desktop'], {
      cwd: directory,
      timeout: 30_000,
      maxBuffer: 16 * 1024,
    })
    const entry = await readFile(join(directory, 'squashfs-root', 'tombot-desktop.desktop'), 'utf8')
    const lines = entry.split(/\r?\n/)
    const build = lines
      .find((line) => line.startsWith('X-AppImage-Version='))
      ?.slice('X-AppImage-Version='.length)
    if (
      lines.includes('Name=TomBot') &&
      (build === version ||
        (build?.startsWith(version + '.') && /^\d+$/.test(build.slice(version.length + 1))))
    )
      return
  } catch {
    throw new Error('无法读取 AppImage 的发行信息，当前版本已保留。')
  }
  throw new Error('新 AppImage 的产品或实际版本与发布信息不符。')
}

/** 下载、逐片校验并交接给系统安装器。Windows 安装器退出后由独立进程清理暂存目录。 */
export async function installClientUpdate(
  transport: Parameters<typeof saveClientUpdate>[0],
  origin: string,
  release: ClientRelease,
  env: Environment,
  assertCurrent: () => void,
  finish: FinishUpdate,
  progress: (message: string) => void,
  signal?: AbortSignal,
) {
  const issue = installationIssue(env, origin, release)
  if (issue) throw new Error(issue)
  const directory = await mkdtemp(
    join(
      env.platform === 'linux' ? dirname(env.appImage!) : app.getPath('temp'),
      '.tombot-update-',
    ),
  )
  const file = join(directory, 'TomBot' + release.extension)
  let workerOwnsFile = false
  try {
    await saveClientUpdate(
      transport,
      origin,
      release,
      file,
      (bytes) => {
        assertCurrent()
        progress('已下载并校验 ' + Math.floor((bytes / release.size) * 100) + '%')
      },
      signal,
    )
    progress('完整性校验通过，正在准备安装…')
    assertCurrent()
    let commit: () => Promise<void>
    if (env.platform === 'darwin') {
      await verifyMacBundle(file, release.version)
      commit = async () => {
        await stageMacUpdate(autoUpdater, release, file)
        await rm(directory, { recursive: true, force: true })
        autoUpdater.quitAndInstall()
      }
    } else if (env.platform === 'win32') {
      const launch = await prepareWindowsUpdate(file, release.version)
      commit = async () => {
        await launch()
        workerOwnsFile = true
        app.quit()
      }
    } else {
      const destination = env.appImage!
      await verifyAppImage(file, process.arch, release.version)
      commit = async () => {
        await rm(join(directory, 'squashfs-root'), { recursive: true })
        await rename(file, destination)
        await rm(directory, { recursive: true, force: true })
        app.relaunch({ execPath: destination })
        app.quit()
      }
    }
    assertCurrent()
    await finish(async () => {
      assertCurrent()
      progress('正在安装，完成后将自动重启…')
      await commit()
    })
  } finally {
    if (!workerOwnsFile) await rm(directory, { recursive: true, force: true })
  }
}
