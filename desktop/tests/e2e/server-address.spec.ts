import { _electron as electron, expect, test, type ElectronApplication } from '@playwright/test'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

/** 使用独立用户目录验证真实地址保存与重启，不接触日常客户端的会话。 */
test('首次指定服务，登录页可修改，重启后重新确认已保存地址', async () => {
  const testInfo = test.info()
  const profile = await mkdtemp(join(tmpdir(), 'tombot-address-test-'))
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      (entry): entry is [string, string] => entry[1] !== undefined,
    ),
  )
  delete env.ELECTRON_RUN_AS_NODE
  delete env.TOMBOT_SERVER_URL
  const launch = () => electron.launch({ args: ['.', '--user-data-dir=' + profile], env })
  let application: ElectronApplication | undefined
  try {
    application = await launch()
    let window = await application.firstWindow()
    await expect(window.getByLabel('服务地址', { exact: true })).toHaveValue('')
    await expect(window.getByRole('button', { name: '在浏览器中登录' })).toHaveCount(0)
    await window.getByRole('button', { name: '连接', exact: true }).click()
    await expect(window.getByRole('alert')).toHaveText('请输入服务地址。')
    const first = 'https://first.example.test:9443'
    await window.getByLabel('服务地址', { exact: true }).fill(first)
    await window.getByRole('button', { name: '连接', exact: true }).click()
    await expect(window.getByRole('heading', { name: '登录 TomBot' })).toBeVisible()
    await expect(window.locator('.session-server')).toContainText(first)
    await window.evaluate(() => {
      document.documentElement.dataset.theme = 'light'
    })
    await window.screenshot({
      path: testInfo.outputPath('login-server-light.png'),
      animations: 'disabled',
    })

    await window.getByRole('button', { name: '修改地址' }).click()
    await expect(window.getByLabel('服务地址', { exact: true })).toHaveValue(first)
    const changed = 'https://' + 'long-server-name-'.repeat(3) + 'team.example.test:9443'
    await window.getByLabel('服务地址', { exact: true }).fill(changed + '/')
    await window.getByRole('button', { name: '连接', exact: true }).click()
    await expect(window.locator('.session-server')).toContainText(changed)
    await expect(window.getByText(first, { exact: true })).toHaveCount(0)
    await application.evaluate(({ BrowserWindow }) => {
      BrowserWindow.getAllWindows()[0]!.setContentSize(860, 640)
    })
    await window.evaluate(() => {
      document.documentElement.dataset.theme = 'dark'
    })
    expect(
      await window
        .locator('.session-server')
        .evaluate((element) => element.scrollWidth <= element.clientWidth),
    ).toBe(true)
    await window.screenshot({
      path: testInfo.outputPath('login-server-dark.png'),
      animations: 'disabled',
    })
    await application.close()
    application = undefined

    application = await launch()
    window = await application.firstWindow()
    await expect(window.getByRole('heading', { name: '连接服务' })).toBeVisible()
    await expect(window.getByLabel('服务地址', { exact: true })).toHaveValue(changed)
    await expect(window.getByRole('button', { name: '在浏览器中登录' })).toHaveCount(0)
    await window.getByRole('button', { name: '连接', exact: true }).click()
    await expect(window.locator('.session-server')).toContainText(changed)
    await expect(window.getByRole('button', { name: '在浏览器中登录' })).toBeEnabled()
  } finally {
    if (application) await application.close()
    await rm(profile, { recursive: true, force: true })
  }
})
