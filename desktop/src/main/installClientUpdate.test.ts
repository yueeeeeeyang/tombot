import type { AutoUpdater } from 'electron'
import { createHash } from 'node:crypto'
import { EventEmitter } from 'node:events'
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { installClientUpdate, installationIssue, type FinishUpdate } from './installClientUpdate'
import { stageMacUpdate, verifyMacBundle } from './macClientUpdate'
import { prepareWindowsUpdate } from './windowsClientUpdate'
import type { ClientRelease } from './clientUpdates'

const runtime = vi.hoisted(() => ({
  quit: vi.fn(),
  relaunch: vi.fn(),
  exec: vi.fn(),
  spawn: vi.fn(),
}))
vi.mock('electron', () => ({ app: runtime, autoUpdater: new EventEmitter() }))
vi.mock('node:child_process', async (original) => {
  const { promisify } = await import('node:util')
  return {
    ...(await original()),
    execFile: Object.assign(vi.fn(), { [promisify.custom]: runtime.exec }),
    spawn: runtime.spawn,
  }
})

const origin = 'https://updates.example.test'
const archive = Buffer.alloc(48)
archive.writeUInt32BE(0x7f454c46)
archive[4] = 2
archive[5] = 1
archive.writeUIntBE(0x414902, 8, 3)
archive.writeUInt16LE(process.arch === 'arm64' ? 183 : 62, 18)
const release: ClientRelease = {
  id: '00000000000000000000000001',
  version: '1.10.0',
  platform: 'linux-' + process.arch,
  extension: '.AppImage',
  notes: '改进重连',
  size: archive.length,
  chunks: [createHash('sha256').update(archive).digest('hex')],
}
let directory: string
let installed: string
beforeEach(async () => {
  vi.clearAllMocks()
  directory = await mkdtemp(join(tmpdir(), 'tombot-install-test-'))
  installed = join(directory, '我的客户端.AppImage')
  await writeFile(installed, '原版本')
})
afterEach(async () => {
  await rm(directory, { recursive: true, force: true })
  vi.unstubAllEnvs()
})

it('同一正式客户端可从任一当前连接的 HTTPS 服务升级，并核对本机安装格式', () => {
  const env = { packaged: true, platform: 'linux' as const, appImage: installed }
  expect(installationIssue(env, origin, release)).toBeUndefined()
  expect(installationIssue({ ...env, packaged: false }, origin, release)).toMatch(/开发版/)
  expect(installationIssue(env, 'https://other.example.test:9443', release)).toBeUndefined()
  expect(installationIssue(env, 'http://localhost', release)).toMatch(/HTTPS/)
  expect(installationIssue({ ...env, appImage: '' }, origin, release)).toMatch(/格式/)
  expect(
    installationIssue({ ...env, platform: 'win32' }, origin, { ...release, extension: '.exe' }),
  ).toBeUndefined()
  expect(
    installationIssue({ ...env, platform: 'darwin' }, origin, { ...release, extension: '.dmg' }),
  ).toMatch(/格式/)
})

it.each([
  '成功',
  '断线',
  '摘要错误',
  '架构错误',
  '实际版本错误',
  '无法读取发行信息',
  '切换服务',
  '有未保存的修改',
])('AppImage 升级交接：%s', async (scenario) => {
  const bytes = Buffer.from(archive)
  if (scenario === '架构错误') bytes.writeUInt16LE(0, 18)
  const manifest = { ...release, chunks: [createHash('sha256').update(bytes).digest('hex')] }
  if (scenario === '摘要错误') bytes[7] = 9
  runtime.exec.mockImplementation(async (_file, _args, { cwd }: { cwd: string }) => {
    await mkdir(join(cwd, 'squashfs-root'))
    await writeFile(
      join(cwd, 'squashfs-root', 'tombot-desktop.desktop'),
      'Name=TomBot\nX-AppImage-Version=' +
        (scenario === '实际版本错误' ? '0.1.0' : release.version),
    )
  })
  if (scenario === '无法读取发行信息') runtime.exec.mockRejectedValueOnce(new Error('启动失败'))
  const fetch = vi.fn(async () =>
    scenario === '断线' ? new Response(null, { status: 503 }) : new Response(bytes),
  )
  const finish = vi.fn<FinishUpdate>(async (install) => {
    if (scenario === '有未保存的修改') throw new Error(scenario)
    await install()
  })
  const check = () => {
    if (scenario === '切换服务') throw new Error(scenario)
  }
  const progress = vi.fn()
  const operation = installClientUpdate(
    { fetch },
    origin,
    manifest,
    { packaged: true, platform: 'linux', appImage: installed },
    check,
    finish,
    progress,
  )
  if (scenario === '成功') {
    await operation
    expect(runtime.relaunch).toHaveBeenCalledWith({ execPath: installed })
    expect(runtime.quit).toHaveBeenCalledOnce()
    expect(await readFile(installed)).toEqual(archive)
    expect(progress).toHaveBeenCalledWith('已下载并校验 100%')
  } else {
    await expect(operation).rejects.toThrow()
    expect(await readFile(installed, 'utf8')).toBe('原版本')
    expect(runtime.relaunch).not.toHaveBeenCalled()
    expect(runtime.quit).not.toHaveBeenCalled()
  }
  expect(await readdir(directory)).toEqual(['我的客户端.AppImage'])
})

it.each([true, false])(
  'macOS 从私有本机地址读取已校验的包并等待系统签名结果，通过=%s',
  async (accepted) => {
    await writeFile(installed, archive)
    let url = ''
    let requested!: Promise<void>
    const updater = Object.assign(new EventEmitter(), {
      setFeedURL: (options: { url: string }) => {
        url = options.url
      },
      checkForUpdates: () => {
        requested = (async () => {
          expect(new URL(url).hostname).toBe('127.0.0.1')
          expect((await fetch(new URL('/unexpected', url))).status).toBe(404)
          const feed = await (await fetch(url)).json()
          expect(feed.name).toBe(release.version)
          expect(Buffer.from(await (await fetch(feed.url)).arrayBuffer())).toEqual(archive)
          updater.emit(accepted ? 'update-downloaded' : 'error', new Error('签名结果'))
        })()
      },
    })
    const operation = stageMacUpdate(updater as unknown as AutoUpdater, release, installed)
    if (accepted) await operation
    else await expect(operation).rejects.toThrow('macOS 未能验证')
    await requested
    expect(updater.listenerCount('update-downloaded')).toBe(0)
    await expect(fetch(url)).rejects.toThrow()
  },
)

it('Windows 签名不匹配时不启动安装，验证成功后才向独立进程交接', async () => {
  vi.stubEnv('SystemRoot', directory)
  runtime.exec.mockRejectedValueOnce(new Error('签名无效'))
  await expect(prepareWindowsUpdate(installed, release.version)).rejects.toThrow('签名')
  expect(runtime.spawn).not.toHaveBeenCalled()
  runtime.exec.mockResolvedValue({})
  const launch = await prepareWindowsUpdate(installed, release.version)
  expect(runtime.spawn).not.toHaveBeenCalled()
  const worker = Object.assign(new EventEmitter(), { unref: vi.fn() })
  runtime.spawn.mockReturnValue(worker)
  const started = launch()
  worker.emit('spawn')
  await started
  expect(runtime.spawn).toHaveBeenCalledWith(
    expect.any(String),
    expect.any(Array),
    expect.objectContaining({
      detached: true,
      windowsHide: true,
      env: expect.objectContaining({
        TOMBOT_INSTALLER: installed,
        TOMBOT_INSTALL_VERSION: release.version,
        TOMBOT_CURRENT_EXE: process.execPath,
        TOMBOT_VERIFY_EXE: join(dirname(process.execPath), 'resources', 'tombot-verify.exe'),
        TOMBOT_PREVIOUS_PID: String(process.pid),
      }),
    }),
  )
  expect(worker.unref).toHaveBeenCalledOnce()
})

it.each(['匹配', '其他应用', '版本错误', '清单损坏'])(
  'macOS 交接前核对包内的产品与版本：%s',
  async (scenario) => {
    runtime.exec.mockResolvedValueOnce({ stdout: Buffer.from('plist') }).mockResolvedValueOnce({
      stdout:
        scenario === '清单损坏'
          ? ''
          : JSON.stringify({
              CFBundleIdentifier:
                scenario === '其他应用' ? 'com.example.app' : 'com.tombot.desktop',
              CFBundleShortVersionString: scenario === '版本错误' ? '0.1.0' : release.version,
            }),
    })
    const verified = verifyMacBundle(installed, release.version)
    if (scenario === '匹配') await expect(verified).resolves.toBeUndefined()
    else await expect(verified).rejects.toThrow(/清单|版本/)
  },
)
