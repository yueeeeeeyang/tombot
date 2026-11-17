import { randomBytes } from 'node:crypto'
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { ulid } from '../shared/ulid'
import { createBot } from './botCreation'
import type { Agent } from '../renderer/src/features/workspace/model'

const mocks = vi.hoisted(() => ({
  handlers: new Map<string, (...args: unknown[]) => Promise<unknown>>(),
  readFile: vi.fn(),
  writeFile: vi.fn(),
}))
vi.mock('./serverTransport', () => ({
  ServerTransport: class {
    fetch(url: string, options: RequestInit) {
      return fetch(url, options)
    }
  },
}))
vi.mock('electron', () => ({
  app: { getPath: () => '/测试用户目录' },
  ipcMain: {
    on: (name: string, handler: (...args: unknown[]) => Promise<unknown>) =>
      mocks.handlers.set(name, handler),
    handle: (name: string, handler: (...args: unknown[]) => Promise<unknown>) =>
      mocks.handlers.set(name, handler),
  },
  shell: {},
  clipboard: {},
}))
vi.mock('node:fs/promises', () => ({
  readFile: mocks.readFile,
  writeFile: mocks.writeFile,
  mkdir: vi.fn(),
  chmod: vi.fn(),
  lstat: vi.fn(),
  open: vi.fn(),
}))

const profile = { name: '小黑', avatar: 'bombay', group: '未分组' } as const

it.each(['取消订阅', '页面重载'])('%s 会关闭旧事件流，不能留下重复订阅', async (reason) => {
  const { EventEmitter } = await import('node:events')
  const sender = Object.assign(new EventEmitter(), {
    id: 1,
    isDestroyed: () => false,
    send: vi.fn(),
  })
  let aborted = false
  vi.stubGlobal(
    'fetch',
    vi.fn(async (_url: string, options: RequestInit) => {
      const body = new ReadableStream({
        start(controller) {
          controller.enqueue(
            new TextEncoder().encode(
              'id: 1\ndata: {"sequence":1,"kind":"message.created","resource_id":"test","payload":{}}\n\n',
            ),
          )
          options.signal?.addEventListener(
            'abort',
            () => {
              aborted = true
              controller.error(new DOMException('已取消', 'AbortError'))
            },
            { once: true },
          )
        },
      })
      return new Response(body, { headers: { 'content-type': 'text/event-stream' } })
    }),
  )
  const watching = invoke('desktop:watch-events', { sender }, 'watch')
  await vi.waitFor(() => expect(sender.send).toHaveBeenCalledOnce())
  if (reason === '页面重载') sender.emit('did-start-navigation')
  else await invoke('desktop:unwatch-events', { sender }, 'watch')
  expect(await watching).toMatchObject({ ok: true })
  expect(aborted).toBe(true)
  expect(sender.listenerCount('destroyed')).toBe(0)
  expect(sender.listenerCount('did-start-navigation')).toBe(0)
})
const png =
  'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aS1sAAAAASUVORK5CYII='

beforeEach(async () => {
  vi.resetModules()
  mocks.handlers.clear()
  mocks.readFile.mockResolvedValue(
    JSON.stringify({
      serverUrl: 'https://desktop.example.test',
      accessToken: randomBytes(32).toString('base64url'),
      refreshToken: randomBytes(32).toString('base64url'),
      accessExpiresAt: new Date(Date.now() + 300_000).toISOString(),
      username: 'test-user',
    }),
  )
  const { registerDesktopIpc } = await import('./desktopServer')
  registerDesktopIpc()
})
afterEach(() => vi.unstubAllGlobals())

async function invoke(name: string, ...args: unknown[]) {
  const handler = mocks.handlers.get(name)
  if (!handler) throw new Error('测试通道未注册')
  return handler(...args)
}

it('当前用户资料经主进程读取，只把昵称、用户名和头像交给界面', async () => {
  const fetchProfile = vi.fn(async () =>
    Response.json({ profile: { username: 'owner', display_name: '真实昵称', avatar: png } }),
  )
  vi.stubGlobal('fetch', fetchProfile)
  expect(await invoke('desktop:user-profile')).toEqual({
    ok: true,
    value: { username: 'owner', displayName: '真实昵称', avatar: png },
  })
  expect(fetchProfile.mock.calls).toHaveLength(1)
  expect(fetchProfile).toHaveBeenCalledWith(
    'https://desktop.example.test/api/v2/auth/profile',
    expect.objectContaining({ method: 'GET' }),
  )
})

it.each([
  {
    profile: {
      username: 'owner',
      display_name: '昵称',
      avatar: 'https://foreign.example.test/avatar.png',
    },
  },
  { profile: { username: 'owner', display_name: '昵称', avatar: 123 } },
  {},
])('用户资料不完整或包含远程头像时明确拒绝', async (response) => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => Response.json(response)),
  )
  expect(await invoke('desktop:user-profile')).toMatchObject({
    ok: false,
    code: 'RESPONSE_INVALID',
  })
})

describe('服务端创建猫咪与投递', () => {
  it('表情请求携带明确状态与幂等标识，并返回服务端版本和历史标记', async () => {
    const messageId = ulid()
    const clientRequestId = ulid()
    const change = vi.fn(async (_url: string, options: RequestInit) => {
      expect(options.method).toBe('PUT')
      expect(JSON.parse(String(options.body))).toEqual({
        client_request_id: clientRequestId,
        emoji: '👍',
        is_active: false,
      })
      return Response.json({
        message: {
          id: messageId,
          sender_kind: 'bot',
          blocks: [{ kind: 'text', text: '完成了' }],
          created_at: new Date().toISOString(),
          sequence: 2,
          reactions: [{ emoji: '❤️', count: 1 }],
          reaction_revision: 3,
        },
      })
    })
    vi.stubGlobal('fetch', change)
    expect(
      await invoke(
        'desktop:reaction',
        {},
        { messageId, emoji: '👍', isActive: false, clientRequestId },
      ),
    ).toMatchObject({
      ok: true,
      value: { id: messageId, reactions: [{ emoji: '❤️', count: 1 }], reactionRevision: 3 },
    })
    expect(change).toHaveBeenCalledWith(
      `https://desktop.example.test/api/v2/messages/${messageId}/reaction`,
      expect.anything(),
    )
  })

  it('表情接口返回空消息时明确报错，不能当作更新成功', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => Response.json({})),
    )
    expect(
      await invoke(
        'desktop:reaction',
        {},
        { messageId: ulid(), emoji: '👍', isActive: true, clientRequestId: ulid() },
      ),
    ).toMatchObject({ ok: false, code: 'RESPONSE_INVALID' })
  })

  it.each([
    ['failed', 'EXECUTION_FAILED', '消息已接收，但服务端执行失败'],
    ['canceled', 'CANCELED', '这次运行已取消'],
    ['unknown', 'TOOL_RESULT_UNKNOWN', '工具执行结果未知'],
  ])('运行 %s 不影响消息受理，后续历史从独立接口恢复', async (status, code) => {
    const botId = ulid(),
      conversationId = ulid(),
      runId = ulid(),
      messageId = ulid(),
      replyToId = ulid()
    const paths: string[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string, options: RequestInit) => {
        const path = new URL(url).pathname.replace('/api/v2', '')
        paths.push(`${options.method} ${path}`)
        if (path === '/workspace')
          return Response.json({
            snapshot: {
              bots: [{ id: botId, conversation_id: conversationId, name: '小黑', revision: 1 }],
              groups: [],
              workspace: { revision: 1 },
              event_sequence: 0,
            },
          })
        if (path === `/conversations/${conversationId}/activity`) return Response.json({ runs: [] })
        if (path === `/runs/${runId}`) return Response.json({ run: { status, error_code: code } })
        if (path === `/conversations/${conversationId}/messages`) {
          if (options.method === 'POST') {
            expect(JSON.parse(String(options.body))).toEqual({
              client_request_id: messageId,
              content: '你好',
              reply_to_id: replyToId,
            })
            return Response.json(
              { submission: { run_id: runId, message_id: messageId } },
              { status: 202 },
            )
          }
          return Response.json({
            messages: [
              {
                id: messageId,
                client_request_id: messageId,
                reply_to: { message_id: replyToId, excerpt: '之前的消息' },
                sender_kind: 'user',
                blocks: [{ kind: 'text', text: '你好' }],
                created_at: new Date().toISOString(),
              },
            ],
          })
        }
        throw new Error('出现未预期的请求路径')
      }),
    )
    expect(
      await invoke(
        'desktop:deliver',
        { sender: { send: vi.fn() } },
        {
          agentId: botId,
          content: '你好',
          requestId: ulid(),
          clientRequestId: messageId,
          replyToId,
        },
      ),
    ).toMatchObject({
      ok: true,
      value: {
        acceptedMessageId: messageId,
      },
    })
    expect(paths.filter((path) => path.startsWith('POST '))).toHaveLength(1)
    expect(paths).not.toContain(`GET /runs/${runId}`)
    expect(await invoke('desktop:agent-conversation', {}, botId)).toMatchObject({
      ok: true,
      value: {
        messages: [
          {
            id: messageId,
            requestId: messageId,
            role: 'user',
            content: '你好',
            replyTo: { messageId: replyToId, excerpt: '之前的消息' },
          },
        ],
        runs: [],
      },
    })
  })

  it('刷新首只猫咪修订后保存选择，并把消息发到服务端分配的会话', async () => {
    const requestId = ulid(),
      botId = ulid(),
      conversationId = ulid(),
      runId = ulid(),
      messageId = ulid()
    const bot = {
      id: botId,
      conversation_id: conversationId,
      revision: 1,
      name: '小黑',
      description: '',
      category: '未分组',
      avatar_ref: 'bombay',
      is_archived: false,
    }
    const calls: Array<{ method: string; path: string; body: Record<string, unknown> }> = []
    let isCreated = false
    const fetcher = vi.fn(async (url: string, options: RequestInit) => {
      const path = new URL(url).pathname.replace('/api/v2', '')
      const body = options.body ? JSON.parse(String(options.body)) : {}
      calls.push({ method: options.method!, path, body })
      if (path === '/bots' && options.method === 'POST') {
        isCreated = true
        expect(body.client_request_id).toBe(requestId)
        expect(body.system_prompt).toMatch(/assistant/)
        expect(body.approval_mode).toBe('standard')
        expect(body.tool_permissions).toEqual(['computer_shell'])
        return Response.json({ bot, is_created: true }, { status: 201 })
      }
      if (path === '/workspace' && options.method === 'GET') {
        return Response.json({
          snapshot: {
            bots: isCreated ? [bot] : [],
            groups: [],
            workspace: {
              active_bot_id: isCreated ? botId : null,
              primary_bot_id: isCreated ? botId : null,
              revision: isCreated ? 2 : 1,
            },
            event_sequence: 0,
          },
        })
      }
      if (path === '/workspace' && options.method === 'PUT') {
        expect(body).toEqual({ expected_revision: 2, active_bot_id: botId, primary_bot_id: botId })
        return Response.json({ workspace: { revision: 3 } })
      }
      if (path === `/conversations/${conversationId}/messages`) {
        if (options.method === 'POST') {
          expect(body).toEqual({
            client_request_id: expect.stringMatching(/^[0-7][0-9A-HJKMNP-TV-Z]{25}$/),
            content: '你好',
          })
          return Response.json(
            { submission: { run_id: runId, message_id: messageId } },
            { status: 202 },
          )
        }
        return Response.json({
          messages: [
            {
              id: messageId,
              sender_kind: 'bot',
              blocks: [{ kind: 'text', text: '你好，我是小黑。' }],
              created_at: new Date().toISOString(),
            },
          ],
        })
      }
      if (path === `/runs/${runId}`) return Response.json({ run: { status: 'succeeded' } })
      throw new Error('出现未预期的请求路径')
    })
    vi.stubGlobal('fetch', fetcher)
    await invoke('desktop:load-workspace')
    const created = (await invoke('desktop:create-agent', undefined, profile, requestId)) as {
      ok: boolean
      value: Agent
    }
    expect(created.ok).toBe(true)
    expect(created.value.id).toBe(botId)
    expect(created.value.id).not.toBe(requestId)
    expect(
      await invoke('desktop:save-workspace', undefined, {
        version: 5,
        agents: [created.value],
        activeAgentId: botId,
        leadAgentId: botId,
      }),
    ).toEqual({ ok: true, value: undefined })
    const send = vi.fn()
    expect(
      await invoke(
        'desktop:deliver',
        { sender: { send } },
        { agentId: botId, content: '你好', requestId: ulid(), clientRequestId: messageId },
      ),
    ).toMatchObject({
      ok: true,
      value: { acceptedMessageId: messageId },
    })
    expect(calls.filter((call) => call.path === '/bots')).toHaveLength(1)
    expect(calls.findIndex((call) => call.path === '/bots')).toBeLessThan(
      calls.findIndex((call) => call.method === 'PUT'),
    )
    expect(
      calls.find((call) => call.method === 'POST' && call.path.endsWith('/messages'))?.body
        .client_request_id,
    ).toBe(messageId)
    expect(send).not.toHaveBeenCalled()
  })

  it('创建被拒时不刷新目录或提交选择，错误原样返回表单', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(
        Response.json({ code: 'BOT_LIMIT', message: '机器人数量已达到上限' }, { status: 409 }),
      )
    vi.stubGlobal('fetch', fetcher)
    expect(await invoke('desktop:create-agent', undefined, profile, ulid())).toMatchObject({
      ok: false,
      code: 'BOT_LIMIT',
      message: '机器人数量已达到上限',
    })
    expect(fetcher).toHaveBeenCalledTimes(1)
  })

  it('输入或服务端身份不合法时不能宣称创建成功', async () => {
    const call = vi
      .fn()
      .mockResolvedValue({ bot: { id: 'local-uuid', conversation_id: ulid(), revision: 1 } })
    await expect(createBot(call, profile, crypto.randomUUID())).rejects.toThrow('请求标识无效')
    await expect(createBot(call, { ...profile, name: '' }, ulid())).rejects.toThrow('助手名称')
    expect(call).not.toHaveBeenCalled()
    await expect(createBot(call, profile, ulid())).rejects.toThrow('没有确认猫咪及其会话')
  })

  it('头像上传失败重试仍用原创建请求，并采用服务返回的最新修订', async () => {
    const requestId = ulid(),
      botId = ulid(),
      conversationId = ulid()
    const bot = {
      id: botId,
      conversation_id: conversationId,
      revision: 1,
      has_custom_avatar: false,
    }
    const call = vi
      .fn()
      .mockResolvedValueOnce({ bot })
      .mockRejectedValueOnce(new Error('连接中断'))
      .mockResolvedValueOnce({ bot: { ...bot, revision: 2 } })
      .mockResolvedValueOnce({ bot: { ...bot, revision: 3, has_custom_avatar: true } })
    const custom = { ...profile, customAvatar: png, description: '设计搭档' }
    await expect(createBot(call, custom, requestId)).rejects.toThrow('连接中断')
    expect(await createBot(call, custom, requestId)).toBe(botId)
    expect(call.mock.calls[0]).toEqual(call.mock.calls[2])
    expect(call.mock.calls[3]).toEqual([
      'PUT',
      `/bots/${botId}/avatar`,
      { expected_revision: 2, content_base64: png.split(',')[1] },
    ])
  })

  it('已保存的自定义头像从受认证接口恢复，超大响应明确失败', async () => {
    const bot = {
      id: ulid(),
      conversation_id: ulid(),
      revision: 2,
      name: '小黑',
      category: '未分组',
      avatar_ref: 'bombay',
      has_custom_avatar: true,
    }
    let oversized = false
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string, options: RequestInit) => {
        expect((options.headers as Record<string, string>).authorization).toMatch(/^Bearer /)
        if (url.includes('/workspace?'))
          return Response.json({
            snapshot: { bots: [bot], groups: [], workspace: { revision: 2 }, event_sequence: 0 },
          })
        expect(url).toContain(`/bots/${bot.id}/avatar`)
        return new Response(
          oversized ? new Uint8Array(73 * 1024) : Buffer.from(png.split(',')[1]!, 'base64'),
          { headers: { 'content-type': 'image/png' } },
        )
      }),
    )
    expect(await invoke('desktop:load-workspace')).toMatchObject({
      ok: true,
      value: { workspace: { agents: [{ customAvatar: png }] } },
    })
    oversized = true
    expect(await invoke('desktop:load-workspace')).toMatchObject({
      ok: false,
      message: '猫咪头像超过大小限制。',
    })
  })
})

it('私信请求保留发言者，主动正文和双方入口接入既有消息界面', async () => {
  const conversationId = ulid()
  const sender = ulid()
  const dmConversation = ulid()
  vi.stubGlobal(
    'fetch',
    vi.fn(async () =>
      Response.json({
        messages: [
          {
            id: ulid(),
            sequence: 3,
            sender_kind: 'system',
            blocks: [{ kind: 'text', text: '查询结果', user_message: true }],
            created_at: '2026-11-17T02:00:00Z',
          },
          {
            id: ulid(),
            sequence: 2,
            sender_kind: 'system',
            blocks: [
              {
                kind: 'dm_notice',
                text: '收到小黑的消息',
                dm_notice: {
                  direction: 'received',
                  peer_id: sender,
                  peer_name: '小黑',
                  conversation_id: dmConversation,
                },
              },
            ],
            created_at: '2026-11-17T01:59:00Z',
          },
          {
            id: ulid(),
            sequence: 1,
            sender_kind: 'system',
            blocks: [{ kind: 'text', text: '查询天气', bot_message: { sender_id: sender } }],
            created_at: '2026-11-17T01:58:00Z',
          },
        ],
      }),
    ),
  )
  expect(await invoke('desktop:conversation-messages', {}, { conversationId })).toMatchObject({
    ok: true,
    value: {
      messages: [
        { speaker: sender, content: '查询天气', isNotice: false },
        {
          dmNotice: {
            direction: 'received',
            peerId: sender,
            peerName: '小黑',
            conversationId: dmConversation,
          },
        },
        { content: '查询结果', isNotice: false },
      ],
    },
  })
})

it('全局工作状态使用已登录用户的权威活动接口', async () => {
  const request = vi.fn(async () =>
    Response.json({ runs: [{ id: 'run', bot_id: 'bot', status: 'running' }] }),
  )
  vi.stubGlobal('fetch', request)
  expect(await invoke('desktop:bot-activities')).toMatchObject({
    ok: true,
    value: { runs: [{ id: 'run', bot_id: 'bot' }] },
  })
  expect(request).toHaveBeenCalledWith(
    'https://desktop.example.test/api/v2/activity',
    expect.objectContaining({ method: 'GET' }),
  )
})
