import {
  _electron as electron,
  expect,
  test,
  type ElectronApplication,
  type Page,
} from '@playwright/test'
import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import type { NativeImage } from 'electron'

let application: ElectronApplication | undefined
let window: Page
let profilePath = ''
let browserErrors: string[]

/** 启动隔离的 Electron 窗口。Dock 没有读回接口，图标测试通过临时入口记录设置调用。 */
async function startDesktop(iconProbe?: 'observe' | 'empty'): Promise<void> {
  browserErrors = []
  profilePath = await mkdtemp(join(tmpdir(), 'tombot-console-e2e-'))
  let entry = '.'
  if (iconProbe) {
    entry = join(profilePath, 'entry.cjs')
    const builtMain = new URL('../../out/main/index.js', import.meta.url)
    await writeFile(
      entry,
      `const { app, nativeImage } = require('electron')
const setIcon = app.dock.setIcon.bind(app.dock)
app.dock.setIcon = (icon) => {
  setIcon(icon)
  globalThis.tombotDockIcon = icon
}
if (${iconProbe === 'empty'}) {
  const createFromPath = nativeImage.createFromPath.bind(nativeImage)
  nativeImage.createFromPath = (file) => {
    if (!require('node:path').basename(file).startsWith('tombot-icon')) return createFromPath(file)
    globalThis.tombotEmptyIcon = true
    return nativeImage.createEmpty()
  }
}
import(${JSON.stringify(builtMain.href)}).catch((error) => {
  console.error(error)
  app.exit(1)
})`,
    )
  }
  const env: Record<string, string> = {}
  for (const [key, value] of Object.entries(process.env)) {
    if (value !== undefined && key !== 'TOMBOT_SERVER_URL') env[key] = value
  }
  application = await electron.launch({
    args: [entry, '--force-device-scale-factor=1', `--user-data-dir=${profilePath}`],
    env,
  })
  window = await application.firstWindow()
  window.on('pageerror', (error) => browserErrors.push(error.message))
  await expect(window.getByRole('heading', { name: '连接服务', exact: true })).toBeVisible()
}

/** 保留真实主进程、预加载和渲染构建，业务服务通过确定的 IPC 回包替代。 */
async function connectConsoleFixture(): Promise<void> {
  await application!.evaluate(({ ipcMain }) => {
    const agent = {
      id: 'assistant',
      name: '小黑',
      avatar: 'bombay',
      description: '',
      group: '',
      modelId: 'test-model',
      messages: [],
      draft: '',
      artifacts: [],
      conversationId: 'test-chat',
    }
    const responses = {
      'desktop:session': { phase: 'ready', serverUrl: 'https://tom.example.test' },
      'desktop:connect': { phase: 'ready', serverUrl: 'https://tom.example.test' },
      'desktop:user-profile': { username: 'tester', displayName: '测试用户', avatar: '' },
      'desktop:load-workspace': {
        workspace: { version: 5, activeAgentId: agent.id, leadAgentId: agent.id, agents: [agent] },
        pristine: false,
      },
      'desktop:watch-events': null,
      'desktop:groups': [],
      'desktop:tasks': [],
      'desktop:conversation-previews': [],
      'desktop:agent-conversation': { messages: [], hasMore: false },
      'desktop:bot-activities': { runs: [] },
    }
    for (const [channel, value] of Object.entries(responses)) {
      ipcMain.removeHandler(channel)
      ipcMain.handle(channel, () => ({ ok: true, value }))
    }
  })
  await window.getByRole('textbox', { name: '服务地址' }).fill('https://tom.example.test')
  await window.getByRole('button', { name: '连接', exact: true }).click()
  await expect(window.getByRole('textbox', { name: '给小黑发消息' })).toBeVisible()
}

test.afterEach(async () => {
  await application?.close()
  application = undefined
  if (profilePath) await rm(profilePath, { recursive: true, force: true })
  expect(browserErrors).toEqual([])
})

test('客户端启动时将随构建分发的孟买猫图标应用到 Dock', async () => {
  test.skip(process.platform !== 'darwin', 'Dock 只在 macOS 上提供')
  await startDesktop('observe')
  const sourceIcon = fileURLToPath(
    new URL('../../src/main/assets/tombot-icon.png', import.meta.url),
  )
  const matches = await application!.evaluate(({ nativeImage }, path) => {
    const dockIcon = Reflect.get(globalThis, 'tombotDockIcon') as NativeImage | undefined
    const source = nativeImage.createFromPath(path)
    return !source.isEmpty() && dockIcon?.toBitmap().equals(source.toBitmap())
  }, sourceIcon)
  expect(matches).toBe(true)
})

test('客户端图标无法读取时仍能打开登录界面', async () => {
  test.skip(process.platform !== 'darwin', '使用 macOS Dock 观察图标回退')
  await startDesktop('empty')
  const iconProbe = await application!.evaluate(() => ({
    wasRead: Reflect.get(globalThis, 'tombotEmptyIcon') === true,
    wasApplied: Reflect.has(globalThis, 'tombotDockIcon'),
  }))
  expect(iconProbe).toEqual({ wasRead: true, wasApplied: false })
})

test('noVNC 通过预加载桥接当前猫咪，观看和接管切换后可交还', async () => {
  await startDesktop()
  await connectConsoleFixture()
  // 本用例只替代云端 RFB 字节，保留真实 Electron IPC、预加载和 noVNC。
  await application!.evaluate(({ ipcMain }) => {
    const record = {
      id: '01ARZ3NDEKTSV4RRFFQ69G5FAV',
      bot_id: 'assistant',
      state: 'viewing',
      error_code: '',
      expires_at: new Date(Date.now() + 30000).toISOString(),
    }
    for (const [channel, call] of Object.entries({
      'desktop:console-open': (...args: unknown[]) => {
        if (args[1] !== record.bot_id) throw new Error('观看了错误猫咪')
        record.state = 'opening'
        return { ...record }
      },
      'desktop:console-status': () => {
        if (record.state === 'opening') record.state = 'viewing'
        return { ...record }
      },
      'desktop:console-close': () => {
        record.state = 'closed'
        return { ...record }
      },
      'desktop:console-control': (...args: unknown[]) => {
        record.state = args[2] ? 'active' : 'viewing'
        return { ...record }
      },
    })) {
      ipcMain.removeHandler(channel)
      ipcMain.handle(channel, (...args) => ({ ok: true, value: call(...args) }))
    }
    let phase = 0,
      pending: number[] = [],
      painted = false
    ipcMain.removeAllListeners('desktop:console-stream-open')
    ipcMain.removeAllListeners('desktop:console-stream-write')
    ipcMain.on('desktop:console-stream-open', (event, _id, streamId) => {
      phase = 0
      pending = []
      painted = false
      event.sender.send('desktop:console-stream', streamId, { kind: 'open' })
      event.sender.send('desktop:console-stream', streamId, {
        kind: 'bytes',
        bytes: new TextEncoder().encode('RFB 003.008\n'),
      })
    })
    ipcMain.on('desktop:console-stream-write', (event, streamId, bytes: Uint8Array) => {
      const emit = (bytes: Uint8Array) =>
        event.sender.send('desktop:console-stream', streamId, { kind: 'bytes', bytes })
      pending.push(...bytes)
      while (pending.length) {
        if (phase === 0) {
          if (pending.length < 12) return
          pending.splice(0, 12)
          phase++
          emit(new Uint8Array([1, 1]))
          continue
        }
        if (phase === 1) {
          pending.shift()
          phase++
          emit(new Uint8Array(4))
          continue
        }
        if (phase === 2) {
          pending.shift()
          phase++
          const header = new Uint8Array(27),
            v = new DataView(header.buffer)
          v.setUint16(0, 1280)
          v.setUint16(2, 800)
          header[4] = 32
          header[5] = 24
          header[7] = 1
          v.setUint16(8, 255)
          v.setUint16(10, 255)
          v.setUint16(12, 255)
          header[14] = 16
          header[15] = 8
          v.setUint32(20, 3)
          header.set(new TextEncoder().encode('Bot'), 24)
          emit(header)
          continue
        }
        const type = pending[0]
        let size = type === 0 ? 20 : type === 3 ? 10 : type === 4 ? 8 : type === 5 ? 6 : 0
        if (type === 2) {
          if (pending.length < 4) return
          size = 4 + 4 * ((pending[2]! << 8) | pending[3]!)
        }
        if (!size) throw new Error('Electron 夹具收到未知 RFB 消息')
        if (pending.length < size) return
        pending.splice(0, size)
        if (type === 3 && !painted) {
          painted = true
          const frame = new Uint8Array(20),
            v = new DataView(frame.buffer)
          v.setUint16(2, 1)
          v.setUint16(8, 1)
          v.setUint16(10, 1)
          frame.fill(220, 16)
          emit(frame)
        }
      }
    })
  })
  await window.getByRole('button', { name: '查看小黑资料', exact: true }).click()
  const panel = window.getByRole('complementary', { name: '小黑' })
  await panel.getByRole('button', { name: '小黑的电脑', exact: true }).click()
  const viewer = window.getByRole('dialog', { name: '小黑的电脑', exact: true })
  const canvas = viewer.locator('.computer-screen canvas')
  await expect(viewer).toBeVisible()
  await expect(canvas).toBeVisible()
  await expect(viewer.getByText('仅观看')).toBeVisible()
  await expect
    .poll(() =>
      canvas.evaluate((canvas) => {
        return Array.from(
          (canvas as HTMLCanvasElement).getContext('2d')!.getImageData(0, 0, 1, 1).data,
        )
      }),
    )
    .toEqual([220, 220, 220, 255])
  const nativeWindow = await application!.browserWindow(window)
  // macOS 会先更新页面全屏状态，再完成原生窗口的进入动画。
  const entered = nativeWindow.evaluate(
    (native) => new Promise<void>((resolve) => native.once('enter-full-screen', resolve)),
  )
  await viewer.getByRole('button', { name: '全屏查看' }).click()
  await entered
  const left = nativeWindow.evaluate(
    (native) => new Promise<void>((resolve) => native.once('leave-full-screen', resolve)),
  )
  await viewer.getByRole('button', { name: '退出全屏' }).click()
  await left
  await expect(viewer.getByRole('button', { name: '全屏查看' })).toBeVisible()
  await expect(viewer.getByRole('textbox', { name: '输入到电脑' })).toHaveCount(0)
  await viewer.getByRole('button', { name: '取得控制权', exact: true }).click()
  await expect(viewer.getByText('用户正在操作，猫咪已暂停')).toBeVisible()
  await expect(viewer.getByRole('textbox', { name: '输入到电脑' })).toBeVisible()
  await viewer.getByRole('button', { name: '交还控制权' }).click()
  await expect(canvas).toBeVisible()
  await expect(viewer.getByRole('textbox', { name: '输入到电脑' })).toHaveCount(0)
  await viewer.getByRole('button', { name: '关闭观看' }).click()
  await expect(viewer).toHaveCount(0)
  await expect(panel.getByText('观看已结束')).toBeVisible()
})
