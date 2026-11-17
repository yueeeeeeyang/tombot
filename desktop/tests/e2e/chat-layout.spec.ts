import { test, expect } from '@playwright/test'
import { readFile } from 'node:fs/promises'
import { resolve, extname } from 'node:path'
import type { AppWorkspace } from '../../src/renderer/src/features/workspace/model'
import type { DesktopBackend } from '../../src/renderer/src/features/desktop/backend'

// 使用确定的桥接夹具验证真实 Chromium 布局，服务契约由独立主进程和数据库测试覆盖。
test.beforeEach(async ({ page }) => {
  const workspace: AppWorkspace = {
    version: 5,
    activeAgentId: 'assistant',
    leadAgentId: 'assistant',
    agents: (
      [
        ['assistant', '小黑', 'bombay'],
        ['developer', '游戏程序', 'orange-tabby'],
      ] as const
    ).map(([id, name, avatar]) => ({
      id,
      name,
      avatar,
      description: '',
      group: '游戏开发',
      modelId: 'test-model',
      messages: [],
      draft: '',
      artifacts: [],
      computerUrl: '',
    })),
  }
  workspace.agents.forEach((agent) => {
    agent.conversationId = `chat-${agent.id}`
  })
  workspace.agents[0]!.messages = Array.from({ length: 35 }, (_, index) => ({
    id: `m-${index}`,
    role: index % 2 ? 'user' : 'assistant',
    content: `第 ${index + 1} 条消息。\n\n这里是一段用于滚动验证的正文，检查返回底部与布局稳定性。`,
    createdAt: '2026-11-17T01:00:00Z',
    status: 'sent',
    reactions: [],
  }))
  workspace.agents[0]!.messages.push({
    id: 'dm',
    role: 'assistant',
    content: '收到私信',
    createdAt: '2026-11-17T01:01:00Z',
    status: 'sent',
    reactions: [],
    dmNotice: {
      direction: 'received',
      peerId: 'developer',
      peerName: '游戏程序',
      conversationId: 'dm-chat',
    },
  })
  await page.addInitScript(
    ({ workspace }) => {
      const saved = localStorage.getItem('test-workspace')
      const current = saved ? JSON.parse(saved) : workspace
      const group = {
        id: 'group',
        name: '项目讨论',
        revision: 1,
        conversation_id: 'group-chat',
        avatar_presets: ['bombay', 'persian'],
        members: [
          { bot_id: 'assistant', name: '小黑' },
          { bot_id: 'developer', name: '游戏程序' },
        ],
      }
      const file = {
        id: 'file',
        conversation_id: 'group-chat',
        name: 'jedis-3.10.0.jar',
        media_type: 'application/java-archive',
        size: 8192,
        revision: 1,
        sha256: '',
        created_at: '2026-11-09T05:47:00Z',
      }
      const unavailable = async () => {
        throw new Error('布局测试不允许此业务操作')
      }
      const session = { phase: 'ready' as const, serverUrl: 'https://tom.example.test' }
      window.desktop = {
        platform: 'darwin',
        backend: {
          connect: unavailable,
          browserLogin: unavailable,
          trustServerCertificate: unavailable,
          logout: unavailable,
          createAgent: unavailable,
          deliverMessage: unavailable,
          uploadChatFile: unavailable,
          downloadChatFile: unavailable,
          renameChatFile: unavailable,
          deleteChatFile: unavailable,
          setReaction: unavailable,
          trySessionSync: () => session,
          session: async () => session,
          loadUserProfile: async () => ({
            username: 'tester',
            displayName: '测试用户',
            avatar: '',
          }),
          watchWindowFocus: () => () => undefined,
          watchEvents: () => () => undefined,
          tryWorkspaceSync: () => ({ workspace: current, pristine: false }),
          loadWorkspace: async () => ({ workspace: current, pristine: false }),
          savesSynchronously: true,
          saveWorkspace: (next) => {
            localStorage.setItem('test-workspace', JSON.stringify(next))
          },
          listGroups: async () => [group],
          groupDetail: async () => group,
          loadConversationPreviews: async (ids) =>
            ids.map((id) => ({
              conversation_id: id,
              sequence: 1,
              content: id === 'group-chat' ? '群聊的最后一条消息' : '机器人最新消息',
            })),
          listConversationMessages: async (id) => ({
            hasMore: false,
            messages: [
              {
                id: 'reply',
                sequence: 1,
                role: 'assistant',
                speaker: 'assistant',
                content:
                  id === 'dm-chat'
                    ? '第一段正文。\n\n第二段正文。\n\n- 列表一\n- 列表二'
                    : '@游戏程序 一起检查这个问题。',
                createdAt: '2026-11-17T01:00:00Z',
                reactions: [],
                attachments: [],
                mentions: [{ id: 'developer', name: '游戏程序' }],
              },
            ],
          }),
          conversationActivity: async () => ({ runs: [] }),
          loadBotActivities: async () => ({ runs: [] }),
          listTasks: async () => [],
          listChatFiles: async () => ({
            files: [file],
            next: '',
            used_bytes: 8192,
            max_bytes: 268435456,
            max_file_bytes: 16777216,
          }),
          previewChatFile: async () => ({ file }),
          loadAbout: async () => ({
            clientVersion: '0.1.0',
            serverUrl: session.serverUrl,
            serverVersion: 'test-build',
          }),
        } as DesktopBackend,
      }
    },
    { workspace },
  )
  const root = resolve(import.meta.dirname, '../../out/renderer')
  await page.route('http://tombot.test/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    const file = resolve(root, '.' + (path === '/' ? '/index.html' : path))
    if (!file.startsWith(root + '/')) throw new Error('测试资源路径无效')
    const contentType =
      (
        {
          '.html': 'text/html',
          '.js': 'application/javascript',
          '.css': 'text/css',
          '.png': 'image/png',
        } as Record<string, string>
      )[extname(file)] ?? 'application/octet-stream'
    await route.fulfill({ body: await readFile(file), contentType })
  })
  await page.setViewportSize({ width: 1200, height: 820 })
  await page.goto('http://tombot.test/')
  await expect(page.getByRole('textbox', { name: '给小黑发消息' })).toBeVisible()
})

test('返回底部一次点击生效且按钮居中，输入区域透明，私聊 Markdown 没有多余空行', async ({
  page,
}) => {
  const log = page.locator('.messages-scroll')
  for (let attempt = 0; attempt < 3; attempt++) {
    await log.hover()
    await page.mouse.wheel(0, -10000)
    const button = page.getByRole('button', { name: '回到最新消息' })
    await expect(button).toBeVisible()
    const box = (await button.boundingBox())!
    const composer = (await page.locator('.composer').boundingBox())!
    expect(Math.abs(box.x + box.width / 2 - composer.x - composer.width / 2)).toBeLessThan(1)
    expect(box.y + box.height).toBeLessThan(composer.y)
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
    await page.mouse.down()
    await expect(button).toHaveCSS(
      'transform',
      `matrix(0.97, 0, 0, 0.97, ${-box.width / 2}, ${-box.height})`,
    )
    const pressed = (await button.boundingBox())!
    expect(Math.abs(pressed.y + pressed.height / 2 - box.y - box.height / 2)).toBeLessThan(1)
    await page.mouse.up()
    await expect
      .poll(() =>
        log.evaluate((element) => element.scrollHeight - element.scrollTop - element.clientHeight),
      )
      .toBeLessThan(2)
  }
  await expect(page.locator('.composer-area')).toHaveCSS('background-color', 'rgba(0, 0, 0, 0)')
  await expect(page.locator('.composer-area')).toHaveCSS('padding-top', '0px')
  await page.locator('.dm-notice').click()
  const markdown = page.locator('.dm-bubble .message-markdown')
  await expect(markdown).toHaveCSS('white-space', 'normal')
  const paragraphs = await markdown.locator('p').evaluateAll((nodes) =>
    nodes.map((node) => {
      const b = node.getBoundingClientRect()
      return { top: b.top, bottom: b.bottom }
    }),
  )
  expect(paragraphs[1]!.top - paragraphs[0]!.bottom).toBeLessThan(18)
  await page.screenshot({ path: 'test-results/screenshots/dm-spacing.png' })
})

test('群聊提及、头像、资料卡和关于页符合布局要求', async ({ page }) => {
  await expect(page.getByRole('button', { name: '项目讨论', exact: true })).toContainText(
    '群聊的最后一条消息',
  )
  await page.getByRole('button', { name: '项目讨论', exact: true }).click()
  await expect(page.locator('.message-speaker .avatar')).toBeVisible()
  await expect(page.locator('.message-speaker')).toContainText('小黑')
  await expect(page.locator('.group-mentions')).toHaveCount(0)
  const input = page.getByRole('textbox', { name: '给项目讨论发消息' })
  await input.fill('@小')
  await input.press('Enter')
  await input.pressSequentially('请检查')
  await expect(input).toHaveValue('@小黑 请检查')
  await expect(page.getByRole('listbox', { name: '提及成员' })).toHaveCount(0)
  await page.getByRole('button', { name: '查看项目讨论群资料' }).click()
  await page.getByRole('tab', { name: '资料库' }).click()
  const card = page.locator('.library-file-card')
  const more = page.getByRole('button', { name: '更多操作 jedis-3.10.0.jar' })
  await page.mouse.move(600, 100)
  await expect(more).toHaveCSS('opacity', '0')
  await card.hover()
  await expect(more).toHaveCSS('opacity', '1')
  await expect(page.getByRole('tooltip')).toContainText('jedis-3.10.0.jar')
  await page.screenshot({ path: 'test-results/screenshots/library-hover.png' })
  await more.click()
  await expect(page.getByRole('menuitem')).toHaveCount(4)
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: '打开设置' }).click()
  await page.getByRole('button', { name: '关于', exact: true }).click()
  await expect(page.getByRole('region', { name: '关于' })).toContainText('test-build')
  await expect(page.getByRole('button', { name: '关于', exact: true })).toHaveCSS(
    'background-color',
    'rgb(231, 231, 231)',
  )
  await page.screenshot({ path: 'test-results/screenshots/about.png' })
})

test('原生拖拽群聊到机器人分组后刷新保持分类', async ({ page }) => {
  const group = page.getByRole('button', { name: '项目讨论', exact: true })
  await group.dragTo(page.getByRole('button', { name: '收起游戏开发分组' }))
  const section = page
    .locator('.agent-group')
    .filter({ has: page.getByRole('heading', { name: '游戏开发', exact: true }) })
  await expect(section.getByRole('button', { name: '项目讨论', exact: true })).toBeVisible()
  await page.reload()
  await expect(section.getByRole('button', { name: '项目讨论', exact: true })).toBeVisible()
})

test('设置错误位于标题与内容之间，重试按钮保持在提示右侧并能恢复', async ({ page }) => {
  await page.evaluate(() => {
    let attempts = 0
    window.desktop!.backend.loadAbout = async () => ({
      clientVersion: '0.1.0',
      serverUrl: 'https://tom.example.test',
      serverVersion: ++attempts === 1 ? '' : 'test-build',
      serverError: attempts === 1 ? '服务端版本读取失败，请重试。' : undefined,
    })
  })
  await page.getByRole('button', { name: '打开设置' }).click()
  await page.getByRole('button', { name: '关于', exact: true }).click()
  const pane = page.getByRole('region', { name: '关于' })
  const alert = pane.getByRole('alert')
  await expect(alert).toContainText('服务端版本读取失败')
  const heading = (await pane.getByRole('heading', { name: '关于' }).boundingBox())!
  const notice = (await alert.boundingBox())!
  const content = (await pane.locator('.settings-about').boundingBox())!
  expect(notice.y).toBeGreaterThanOrEqual(heading.y + heading.height)
  expect(notice.y + notice.height).toBeLessThan(content.y)
  const retry = alert.getByRole('button', { name: '重试读取版本' })
  const button = (await retry.boundingBox())!
  const message = (await alert.locator('span').boundingBox())!
  expect(button.x).toBeGreaterThan(message.x + message.width)
  expect(Math.abs(button.y + button.height / 2 - message.y - message.height / 2)).toBeLessThan(1)
  await page.screenshot({ path: 'test-results/screenshots/about-error.png' })
  await retry.click()
  await expect(pane).toContainText('test-build')
  await expect(alert).toHaveCount(0)
})

test('按住猫咪头像开始原生拖拽，预览清除压缩和倾斜并保持正方形', async ({ page }) => {
  await page.evaluate(() => {
    const original = DataTransfer.prototype.setDragImage
    DataTransfer.prototype.setDragImage = function (element, x, y) {
      const avatar = element.querySelector<HTMLElement>('.avatar')!
      const portrait = element.querySelector<HTMLElement>('.cat-portrait')!
      const box = portrait.getBoundingClientRect()
      document.documentElement.dataset.dragPreview = JSON.stringify({
        pressed: avatar.dataset.pressed,
        width: box.width,
        height: box.height,
        transforms: ['.cat-tilt', '.cat-motion'].map(
          (selector) => getComputedStyle(element.querySelector(selector)!).transform,
        ),
        animation: getComputedStyle(element.querySelector('.cat-motion')!).animationName,
      })
      original.call(this, element, x, y)
    }
  })
  const avatar = page.getByRole('button', { name: '游戏程序', exact: true }).locator('.avatar')
  await avatar.hover()
  const box = (await avatar.boundingBox())!
  await page.mouse.down()
  await expect(avatar).toHaveAttribute('data-pressed', 'true')
  await page.mouse.move(box.x + box.width / 2, box.y + box.height + 20, { steps: 5 })
  await expect.poll(() => page.locator('html').getAttribute('data-drag-preview')).toBeTruthy()
  const preview = JSON.parse((await page.locator('html').getAttribute('data-drag-preview'))!)
  expect(preview.pressed).toBe('true')
  expect(preview.transforms).toEqual(['none', 'none'])
  expect(preview.animation).toBe('none')
  expect(preview.width).toBe(56)
  expect(preview.height).toBe(56)
  await page.keyboard.press('Escape')
  await page.mouse.up()
  await expect(page.locator('.sidebar-drag-preview')).toHaveCount(0)
})
