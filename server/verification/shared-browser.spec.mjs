import { createRequire } from 'node:module'
import { mkdtemp, rm } from 'node:fs/promises'
import { createServer } from 'node:http'
import { tmpdir } from 'node:os'
import { join, isAbsolute } from 'node:path'

const require = createRequire(new URL('../../admin/package.json', import.meta.url))
const { test, expect, chromium } = require('@playwright/test')

// 验证真实浏览器引擎的共享状态和独立窗口能力，不把此测试作为桌面权限网关的实现证明。
test('共享浏览器配置支持不同窗口并发输入与同站点账号状态共享', async ({}, testInfo) => {
  const executablePath = process.env.TOM_TEST_BROWSER_BINARY
  if (!executablePath || !isAbsolute(executablePath)) {
    throw new Error('必须指定本次验收使用的 Chromium 二进制 TOM_TEST_BROWSER_BINARY')
  }
  const profile = await mkdtemp(join(tmpdir(), 'tombot-shared-browser-'))
  const server = createServer((request, response) => {
    const url = new URL(request.url, 'http://127.0.0.1')
    response.setHeader('Cache-Control', 'no-store')
    if (url.pathname === '/state') {
      response.setHeader('Content-Type', 'application/json')
      const account =
        /(?:^|; )fixture_account=(first|second)(?:;|$)/.exec(request.headers.cookie ?? '')?.[1] ??
        'guest'
      response.end(JSON.stringify({ account }))
      return
    }
    if (url.pathname === '/login') {
      const account = url.searchParams.get('account')
      if (request.method !== 'POST' || !['first', 'second'].includes(account)) {
        response.writeHead(400).end()
        return
      }
      response.setHeader(
        'Set-Cookie',
        `fixture_account=${account}; HttpOnly; SameSite=Strict; Path=/; Max-Age=3600`,
      )
      response.end('登录完成')
      return
    }
    if (url.pathname === '/logout') {
      if (request.method !== 'POST') {
        response.writeHead(405).end()
        return
      }
      response.setHeader(
        'Set-Cookie',
        'fixture_account=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0',
      )
      response.end('退出完成')
      return
    }
    const owner = url.searchParams.get('owner') === 'A' ? '甲' : '乙'
    const color = owner === '甲' ? '#e2f3ff' : '#ffecd9'
    response.setHeader('Content-Type', 'text/html; charset=utf-8')
    response.end(
      `<!doctype html><html lang="zh"><meta charset="utf-8"><title>机器人${owner}窗口</title><style>body{font:24px sans-serif;margin:32px;background:${color}}input{display:block;width:800px;height:50px;font:24px sans-serif}p{max-width:820px;line-height:1.8}</style><h1>机器人${owner}的独立浏览器窗口</h1><label>并发输入<input id="command" aria-label="并发输入"></label><p>此窗口的输入和画面归属机器人${owner}，网站账号状态与另一窗口共享。</p></html>`,
    )
  })
  let origin
  const launch = () =>
    chromium.launchPersistentContext(profile, {
      executablePath,
      headless: true,
      chromiumSandbox: true,
      viewport: { width: 960, height: 640 },
    })
  let context
  try {
    await new Promise((resolve, reject) => {
      server.once('error', reject)
      server.listen(0, '127.0.0.1', resolve)
    })
    origin = `http://127.0.0.1:${server.address().port}`
    context = await launch()
    await context.route('**/*', (route) =>
      route.request().url().startsWith(`${origin}/`) ? route.continue() : route.abort(),
    )
    const browser = await context.browser().newBrowserCDPSession()
    async function window(owner) {
      const [page, target] = await Promise.all([
        context.waitForEvent('page'),
        browser.send('Target.createTarget', {
          url: `${origin}/?owner=${owner}`,
          newWindow: true,
          background: true,
          width: 1280,
          height: 900,
        }),
      ])
      await page.waitForLoadState('domcontentloaded')
      const session = await context.newCDPSession(page)
      const actual = await session.send('Target.getTargetInfo')
      expect(actual.targetInfo.targetId).toBe(target.targetId)
      await session.send('Emulation.setFocusEmulationEnabled', { enabled: true })
      const window = await browser.send('Browser.getWindowForTarget', { targetId: target.targetId })
      return { page, session, target: target.targetId, windowId: window.windowId }
    }
    const first = await window('A')
    const second = await window('B')
    expect(first.target).not.toBe(second.target)
    expect(first.windowId).not.toBe(second.windowId)
    for (const window of [first, second]) {
      expect(
        await window.page.evaluate(() => ({
          width: innerWidth,
          height: innerHeight,
          headings: document.querySelectorAll('h1').length,
        })),
      ).toEqual({ width: 960, height: 640, headings: 1 })
    }
    async function enter(window, text) {
      const bounds = await window.page.locator('#command').boundingBox()
      const point = { x: bounds.x + 15, y: bounds.y + 15, button: 'left', clickCount: 1 }
      await window.session.send('Input.dispatchMouseEvent', { type: 'mousePressed', ...point })
      await window.session.send('Input.dispatchMouseEvent', { type: 'mouseReleased', ...point })
      for (let index = 0; index < 20; index++) {
        await window.session.send('Input.insertText', { text })
      }
    }
    await Promise.all([enter(first, '甲'), enter(second, '乙')])
    await expect(first.page.locator('#command')).toHaveValue('甲'.repeat(20))
    await expect(second.page.locator('#command')).toHaveValue('乙'.repeat(20))
    await Promise.all([
      first.page.screenshot({ path: testInfo.outputPath('bot-a.png') }),
      second.page.screenshot({ path: testInfo.outputPath('bot-b.png') }),
    ])
    async function account(page) {
      return page.evaluate(async () => (await (await fetch('/state')).json()).account)
    }
    async function login(page, value) {
      return page.evaluate(
        async (value) => (await fetch(`/login?account=${value}`, { method: 'POST' })).ok,
        value,
      )
    }
    expect(await login(first.page, 'first')).toBe(true)
    expect(await account(second.page)).toBe('first')
    await first.page.evaluate(() => localStorage.setItem('shared-marker', '共同可见'))
    expect(await second.page.evaluate(() => localStorage.getItem('shared-marker'))).toBe('共同可见')
    async function indexedState(write) {
      const database = await new Promise((resolve, reject) => {
        const request = indexedDB.open('shared-fixture', 1)
        request.onupgradeneeded = () => request.result.createObjectStore('state')
        request.onsuccess = () => resolve(request.result)
        request.onerror = () => reject(request.error)
      })
      try {
        return await new Promise((resolve, reject) => {
          const transaction = database.transaction('state', write ? 'readwrite' : 'readonly')
          const store = transaction.objectStore('state')
          const request = write ? store.put('共享站点状态', 'marker') : store.get('marker')
          transaction.oncomplete = () => resolve(write ? true : request.result)
          transaction.onerror = () => reject(transaction.error)
          transaction.onabort = () => reject(transaction.error)
        })
      } finally {
        database.close()
      }
    }
    expect(await first.page.evaluate(indexedState, true)).toBe(true)
    expect(await second.page.evaluate(indexedState, false)).toBe('共享站点状态')
    expect(await login(second.page, 'second')).toBe(true)
    expect(await account(first.page)).toBe('second')
    expect(
      await first.page.evaluate(async () => (await fetch('/logout', { method: 'POST' })).ok),
    ).toBe(true)
    expect(await account(second.page)).toBe('guest')
    await first.page.close()
    await expect(second.page.locator('#command')).toHaveValue('乙'.repeat(20))
    expect(await login(second.page, 'second')).toBe(true)
    await context.close()
    context = await launch()
    const restored = await context.newPage()
    await restored.goto(origin)
    expect(await account(restored)).toBe('second')
    expect(await restored.evaluate(() => localStorage.getItem('shared-marker'))).toBe('共同可见')
  } finally {
    try {
      if (context) await context.close()
    } finally {
      server.closeAllConnections()
      try {
        if (server.listening)
          await new Promise((resolve, reject) =>
            server.close((error) => (error ? reject(error) : resolve())),
          )
      } finally {
        await rm(profile, { recursive: true })
      }
    }
  }
})
