import {
  _electron as electron,
  expect,
  test,
  type ElectronApplication,
  type Page,
} from '@playwright/test'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'
import { startSessionServer } from './sessionServer'

type SessionServer = Awaited<ReturnType<typeof startSessionServer>>

let testDirectory: string
let application: ElectronApplication | undefined
let servers: SessionServer[]
let pageErrors: string[]

test.beforeEach(async () => {
  servers = []
  pageErrors = []
  testDirectory = await mkdtemp(join(tmpdir(), 'tombot-session-test-'))
})

test.afterEach(async () => {
  try {
    await closeApp()
  } finally {
    try {
      await Promise.all(servers.map((server) => server.stop()))
    } finally {
      await rm(testDirectory, { recursive: true, force: true })
    }
  }
  expect(servers.flatMap((server) => server.unexpectedRequests)).toEqual([])
  expect(pageErrors).toEqual([])
})

async function startServer(): Promise<SessionServer> {
  const server = await startSessionServer(await mkdtemp(join(testDirectory, 'https-')))
  servers.push(server)
  return server
}

async function closeApp(): Promise<void> {
  const running = application
  application = undefined
  await running?.close()
}

async function openApp(profile: string, url?: string, defaultUserDirectory = false): Promise<Page> {
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      (entry): entry is [string, string] => entry[1] !== undefined,
    ),
  )
  delete env.ELECTRON_RUN_AS_NODE
  delete env.NODE_EXTRA_CA_CERTS
  delete env.NODE_TLS_REJECT_UNAUTHORIZED
  if (url) env.TOMBOT_SERVER_URL = url
  else delete env.TOMBOT_SERVER_URL
  const args = ['.', '--force-device-scale-factor=1']
  if (defaultUserDirectory) {
    // 只隔离系统应用目录，仍由应用决定默认的用户目录和会话文件路径。
    const entry = join(profile, 'launch.mjs')
    const main = pathToFileURL(join(import.meta.dirname, '../../out/main/index.js')).href
    await writeFile(
      entry,
      `import { app } from 'electron';\napp.setPath('appData', ${JSON.stringify(profile)});\nawait import(${JSON.stringify(main)});\n`,
    )
    args[0] = entry
  } else {
    args.push(`--user-data-dir=${profile}`)
  }
  application = await electron.launch({ args, env })
  // 只截取发给系统浏览器的地址。证书、IPC、主进程授权交换和本机回调均实际执行。
  await application.evaluate(({ shell }) => {
    shell.openExternal = async (address) => {
      Reflect.set(globalThis, '__sessionAuthorization', address)
    }
  })
  const page = await application.firstWindow()
  page.on('pageerror', (error) => pageErrors.push(error.message))
  return page
}

async function appRegion(page: Page, selector: string): Promise<string> {
  return page
    .locator(selector)
    .first()
    .evaluate((element) => getComputedStyle(element).getPropertyValue('-webkit-app-region').trim())
}

/** 门禁页可拖动窗口，但红绿灯与表单须继续响应点击。 */
async function expectSessionDrag(page: Page): Promise<void> {
  expect(await appRegion(page, '.session-gate')).toBe('drag')
  expect(await appRegion(page, '.session-traffic-lights')).toBe('no-drag')
  expect(await appRegion(page, '.session-card')).toBe('no-drag')
  const controls = await page
    .locator('.session-card :is(input, button, textarea)')
    .evaluateAll((elements) =>
      elements.map((element) =>
        getComputedStyle(element).getPropertyValue('-webkit-app-region').trim(),
      ),
    )
  expect(controls.length).toBeGreaterThan(0)
  expect(controls.every((region) => region === 'no-drag')).toBe(true)
}

async function confirmAddress(page: Page, url: string): Promise<void> {
  await expect(page.getByLabel('服务地址', { exact: true })).toHaveValue(url)
  await page.getByRole('button', { name: '连接', exact: true }).click()
  await expect(page.getByRole('heading', { name: '登录 TomBot' })).toBeVisible()
  await expect(page.locator('.session-server')).toContainText(url)
}

async function trustCertificate(page: Page, server: SessionServer): Promise<void> {
  await page.getByRole('button', { name: '在浏览器中登录', exact: true }).click()
  const fingerprint = page.getByLabel('证书 SHA-256 指纹')
  await expect(fingerprint).toBeVisible()
  expect(server.requests).toEqual([])
  await fingerprint.fill(server.fingerprint)
  await page.getByRole('button', { name: '验证并信任证书' }).click()
  await expect(page.getByRole('status')).toHaveText('证书已验证并保存，可以继续登录。')
  expect(server.requests).toEqual([])
}

async function beginLogin(page: Page): Promise<string> {
  const running = application
  if (!running) throw new Error('桌面应用尚未启动')
  await running.evaluate(() => {
    Reflect.deleteProperty(globalThis, '__sessionAuthorization')
  })
  await page.getByRole('button', { name: '在浏览器中登录', exact: true }).click()
  await expect(page.getByRole('button', { name: '等待浏览器登录' })).toBeDisabled()
  await expect
    .poll(() => running.evaluate(() => Boolean(Reflect.get(globalThis, '__sessionAuthorization'))))
    .toBe(true)
  return running.evaluate(() => String(Reflect.get(globalThis, '__sessionAuthorization')))
}

async function expectWorkspace(page: Page): Promise<void> {
  await expect(page.getByRole('textbox', { name: '给小黑发消息' })).toBeVisible()
}

async function login(page: Page, server: SessionServer): Promise<void> {
  await confirmAddress(page, server.url)
  await trustCertificate(page, server)
  const approval = await server.approve(await beginLogin(page))
  expect(approval.status).toBe(200)
  await approval.text()
  await expectWorkspace(page)
}

async function expectSavedSession(profile: string, url: string): Promise<void> {
  const session = JSON.parse(await readFile(join(profile, 'desktop-session.json'), 'utf8')) as {
    serverUrl?: string
    accessToken?: string
    refreshToken?: string
  }
  expect(session.serverUrl).toBe(url)
  // 只断言是否保存，失败日志不应显示会话凭据内容。
  expect(Boolean(session.accessToken && session.refreshToken)).toBe(true)
}

test('首次打开要求填写服务地址，不会自己去连本机', async () => {
  const server = await startServer()
  const page = await openApp(join(testDirectory, 'blank-profile'))
  await expect(page.getByRole('heading', { name: '连接服务', exact: true })).toBeVisible()
  await expect(page.getByLabel('服务地址')).toHaveValue('')
  await expect(page.getByRole('heading', { name: '正在连接服务' })).toHaveCount(0)
  await expect(page.getByText('正在确认这台服务是否可以访问。')).toHaveCount(0)
  await expectSessionDrag(page)
  await page.screenshot({ path: test.info().outputPath('session-address.png'), scale: 'css' })
  await closeApp()

  const prefilled = await openApp(join(testDirectory, 'prefilled-profile'), server.url)
  await expect(prefilled.getByLabel('服务地址')).toHaveValue(server.url)
  await expect(prefilled.getByRole('button', { name: '在浏览器中登录' })).toHaveCount(0)
  await expectSessionDrag(prefilled)
  await prefilled.screenshot({ path: test.info().outputPath('session-connect.png'), scale: 'css' })
  expect(server.requests).toEqual([])
})

test('连接失败后可以修改服务地址', async () => {
  const original = await startServer()
  const replacement = await startServer()
  const page = await openApp(join(testDirectory, 'profile'), original.url)
  await login(page, original)
  await original.stop()
  await page.reload()
  await expect(page.getByRole('heading', { name: '无法连接服务' })).toBeVisible()
  await expect(page.getByLabel('服务地址')).toHaveValue(original.url)
  await expect(page.getByRole('alert')).toContainText(/无法连接服务|服务未接受连接/)
  await page.getByLabel('服务地址').fill(replacement.url)
  await page.screenshot({
    path: test.info().outputPath('session-connect-failed.png'),
    scale: 'css',
  })
  await page.getByRole('button', { name: '重新连接' }).click()
  await expect(page.getByRole('heading', { name: '登录 TomBot' })).toBeVisible()
  await expect(page.locator('.session-server')).toContainText(replacement.url)
  await expect(page.getByRole('textbox', { name: '给小黑发消息' })).toHaveCount(0)
  await trustCertificate(page, replacement)
  const approval = await replacement.approve(await beginLogin(page))
  expect(approval.status).toBe(200)
  await approval.text()
  await expectWorkspace(page)
})

test('默认用户目录里也会记住服务地址，重启后直接连接', async () => {
  const server = await startServer()
  const configDirectory = await mkdtemp(join(testDirectory, 'app-data-'))
  await login(await openApp(configDirectory, server.url, true), server)
  await expectSavedSession(join(configDirectory, 'TomBot'), server.url)
  await closeApp()
  const reopened = await openApp(configDirectory, undefined, true)
  await expectWorkspace(reopened)
  await expect(reopened.getByRole('heading', { name: '连接服务', exact: true })).toHaveCount(0)
  expect(
    server.requests.filter((request) => request === 'POST /api/v2/auth/browser/start'),
  ).toHaveLength(1)
})

test('下次打开使用已保存的服务地址', async () => {
  const server = await startServer()
  const profile = join(testDirectory, 'profile')
  await login(await openApp(profile, server.url), server)
  await expectSavedSession(profile, server.url)
  await closeApp()
  const reopened = await openApp(profile)
  await expectWorkspace(reopened)
  await expect(reopened.getByRole('heading', { name: '连接服务', exact: true })).toHaveCount(0)
  expect(server.requests).toContain('GET /api/v2/auth/session')
  expect(
    server.requests.filter((request) => request === 'POST /api/v2/auth/browser/start'),
  ).toHaveLength(1)
})

test('系统浏览器授权完成后才能进入工作空间', async () => {
  const server = await startServer()
  const page = await openApp(join(testDirectory, 'profile'), server.url)
  await confirmAddress(page, server.url)
  await expectSessionDrag(page)
  await expect(page.locator('input[type=password]')).toHaveCount(0)
  await page.screenshot({ path: test.info().outputPath('session-login.png'), scale: 'css' })
  await trustCertificate(page, server)
  const approvalAddress = await beginLogin(page)
  await expect(page.getByRole('textbox', { name: '给小黑发消息' })).toHaveCount(0)
  expect(server.requests).toEqual(['POST /api/v2/auth/browser/start'])
  const approval = await server.approve(approvalAddress)
  expect(approval.status).toBe(200)
  await approval.text()
  await expectWorkspace(page)
  expect(await appRegion(page, '.sidebar-toolbar')).toBe('drag')
  expect(await appRegion(page, '.chat-header')).toBe('drag')
})

test('授权失败时保留门禁，重新登录完成后进入工作空间', async () => {
  const server = await startServer()
  const page = await openApp(join(testDirectory, 'profile'), server.url)
  await confirmAddress(page, server.url)
  await trustCertificate(page, server)
  const firstAddress = await beginLogin(page)
  await expect(page.getByRole('button', { name: '修改地址' })).toBeDisabled()
  const rejected = await server.approve(firstAddress, false)
  expect(rejected.status).toBe(400)
  await rejected.text()
  await expect(page.getByRole('alert')).toHaveText('登录回调无效，请重新发起登录。')
  await expect(page.getByRole('textbox', { name: '给小黑发消息' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: '在浏览器中登录' })).toBeEnabled()
  await expectSessionDrag(page)
  expect(server.requests).toEqual(['POST /api/v2/auth/browser/start'])
  await page.screenshot({
    path: test.info().outputPath('session-authorization-failed.png'),
    scale: 'css',
  })
  const secondAddress = await beginLogin(page)
  expect(secondAddress !== firstAddress).toBe(true)
  const approved = await server.approve(secondAddress)
  expect(approved.status).toBe(200)
  await approved.text()
  await expectWorkspace(page)
  expect(await appRegion(page, '.sidebar-toolbar')).toBe('drag')
  expect(await appRegion(page, '.chat-header')).toBe('drag')
})

test('连接失败页的空白处可拖动，按钮不参与拖动', async () => {
  const server = await startServer()
  const page = await openApp(join(testDirectory, 'profile'), server.url)
  await login(page, server)
  await server.stop()
  await page.reload()
  await expect(page.getByRole('heading', { name: '无法连接服务' })).toBeVisible()
  await expectSessionDrag(page)
})

test('登录后自动发现本机更新，确认保存后下载完整安装包', async () => {
  const server = await startServer()
  server.publishUpdate()
  const page = await openApp(join(testDirectory, 'profile'), server.url)
  await login(page, server)
  const dialog = page.getByRole('dialog', { name: '发现客户端新版本' })
  await expect(dialog).toContainText('99.1.0')
  await expect(dialog).toContainText('修复断线后的消息同步。')
  await expect(dialog.getByRole('button', { name: '立即升级' })).toBeDisabled()
  await expect(dialog).toContainText('当前为开发版')
  const target = join(testDirectory, 'test-update.zip')
  await application!.evaluate(({ dialog, shell }, path) => {
    dialog.showSaveDialog = async () => ({ filePath: path, canceled: false })
    shell.showItemInFolder = () => undefined
  }, target)
  await dialog.getByRole('button', { name: '下载安装包' }).click()
  await expect(dialog).toContainText('安装包已保存')
  expect((await readFile(target)).equals(server.installer)).toBe(true)
})

test('同一客户端切换服务后从新服务检查并安装更新，保护未保存修改并请求重启', async () => {
  test.skip(process.platform !== 'darwin', '验证 macOS 系统更新器交接')
  const original = await startServer()
  const server = await startServer()
  const bundle = join(testDirectory, 'TomBot.app', 'Contents')
  await mkdir(bundle, { recursive: true })
  await writeFile(
    join(bundle, 'Info.plist'),
    '<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.tombot.desktop</string><key>CFBundleShortVersionString</key><string>99.1.0</string></dict></plist>',
  )
  const zip = join(testDirectory, 'TomBot.zip')
  await promisify(execFile)(
    '/usr/bin/ditto',
    ['-c', '-k', '--keepParent', join(testDirectory, 'TomBot.app'), zip],
    { timeout: 10000 },
  )
  server.publishUpdate(await readFile(zip))
  const page = await openApp(join(testDirectory, 'profile'), original.url)
  await application!.evaluate(({ app, autoUpdater, dialog }) => {
    // 模拟已安装状态，并截取会实际更新测试主机的系统调用；连接地址由窗口中的用户操作确定。
    Object.defineProperty(app, 'isPackaged', { configurable: true, value: true })
    let feed = ''
    autoUpdater.setFeedURL = ({ url }) => {
      feed = url
    }
    autoUpdater.checkForUpdates = () => {
      void (async () => {
        const metadata = await (await fetch(feed)).json()
        const packageText = Buffer.from(await (await fetch(metadata.url)).arrayBuffer()).toString(
          'base64',
        )
        Reflect.set(globalThis, '__verifiedInstaller', packageText)
        autoUpdater.emit('update-downloaded')
      })().catch((error) => autoUpdater.emit('error', error))
    }
    autoUpdater.quitAndInstall = () => {
      Reflect.set(globalThis, '__installAndRestart', true)
    }
    dialog.showSaveDialog = async () => {
      throw new Error('自动升级不应出现保存对话框')
    }
  })
  await confirmAddress(page, original.url)
  await trustCertificate(page, original)
  await page.evaluate(() => window.dispatchEvent(new Event('online')))
  await expect
    .poll(() => original.requests.includes('GET /api/v2/client-updates/latest'))
    .toBe(true)
  await page.getByRole('button', { name: '修改地址' }).click()
  await page.getByLabel('服务地址', { exact: true }).fill(server.url)
  await login(page, server)
  const dialog = page.getByRole('dialog', { name: '发现客户端新版本' })
  const install = dialog.getByRole('button', { name: '立即升级' })
  await expect(install).toBeEnabled()
  expect(server.requests).toContain('GET /api/v2/client-updates/latest')
  const originalChecks = original.requests.length
  await page.evaluate(() =>
    window.addEventListener('beforeunload', (event) => event.preventDefault(), { once: true }),
  )
  await install.click()
  await expect(dialog.getByRole('alert')).toContainText('未保存的修改')
  expect(
    await application!.evaluate(() => Reflect.get(globalThis, '__installAndRestart')),
  ).toBeUndefined()
  await install.click()
  await expect(dialog.getByRole('status')).toHaveText('正在安装并重启客户端…')
  expect(await application!.evaluate(() => Reflect.get(globalThis, '__verifiedInstaller'))).toBe(
    server.installer.toString('base64'),
  )
  expect(await application!.evaluate(() => Reflect.get(globalThis, '__installAndRestart'))).toBe(
    true,
  )
  expect(original.requests).toHaveLength(originalChecks)
  expect(server.requests.some((route) => route.endsWith('/parts/0'))).toBe(true)
})
