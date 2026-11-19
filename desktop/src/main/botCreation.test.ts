import { createHash, randomBytes } from 'node:crypto'
import { EventEmitter } from 'node:events'
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { ulid } from '../shared/ulid'
import { createBot } from './botCreation'
import type { UserEvent } from '../renderer/src/features/desktop/backend'
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

function eventSender() {
  return Object.assign(new EventEmitter(), {
    id: 1,
    isDestroyed: () => false,
    send: vi.fn<(channel: string, event: UserEvent, watchId: string) => void>(),
  })
}

function openEventStream({ signal }: RequestInit, sequences: number[]) {
  let disconnect!: () => void
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      const encoder = new TextEncoder()
      controller.enqueue(encoder.encode('retry: 1000\n\n'))
      for (const sequence of sequences) {
        const event: UserEvent = {
          sequence,
          kind: 'message.created',
          resource_id: 'test',
          payload: {},
          created_at: new Date().toISOString(),
        }
        controller.enqueue(encoder.encode(`id: ${sequence}\ndata: ${JSON.stringify(event)}\n\n`))
      }
      disconnect = () => {
        signal?.removeEventListener('abort', disconnect)
        controller.error(new TypeError('事件连接已中断'))
      }
      signal?.addEventListener('abort', disconnect, { once: true })
    },
  })
  return {
    response: new Response(body, { headers: { 'content-type': 'text/event-stream' } }),
    disconnect,
  }
}

it.each(['取消订阅', '页面重载'])('%s 会关闭旧事件流，不能留下重复订阅', async (reason) => {
  const sender = eventSender()
  const fetchEvents = vi.fn(
    async (_url: string, options: RequestInit) => openEventStream(options, [1]).response,
  )
  vi.stubGlobal('fetch', fetchEvents)
  const watching = invoke('desktop:watch-events', { sender }, 'watch')
  await vi.waitFor(() => expect(sender.send).toHaveBeenCalledOnce())
  if (reason === '页面重载') sender.emit('did-start-navigation')
  else await invoke('desktop:unwatch-events', { sender }, 'watch')
  expect(await watching).toMatchObject({ ok: true })
  expect(fetchEvents.mock.calls[0]?.[1].signal?.aborted).toBe(true)
  expect(sender.listenerCount('destroyed')).toBe(0)
  expect(sender.listenerCount('did-start-navigation')).toBe(0)
})

describe('消息事件重连', () => {
  it('网络中断后按已确认序号继续订阅，刷新工作空间并跳过重复事件', async () => {
    vi.useFakeTimers()
    const sender = eventSender()
    let disconnect!: () => void
    const fetchEvents = vi
      .fn<(url: string, options: RequestInit) => Promise<Response>>()
      .mockImplementationOnce(async (_url, options) => {
        const stream = openEventStream(options, [7])
        disconnect = stream.disconnect
        return stream.response
      })
      .mockRejectedValueOnce(new TypeError('网络不可用'))
      .mockImplementationOnce(async (_url, options) => openEventStream(options, [7, 8]).response)
    vi.stubGlobal('fetch', fetchEvents)
    const watching = invoke('desktop:watch-events', { sender }, 'watch')
    await vi.advanceTimersByTimeAsync(0)
    expect(sender.send).toHaveBeenCalledOnce()
    disconnect()
    await vi.advanceTimersByTimeAsync(30_000)
    await invoke('desktop:unwatch-events', { sender }, 'watch')
    expect(await watching).toMatchObject({ ok: true })
    expect(fetchEvents.mock.calls.map(([url]) => new URL(url).searchParams.get('after'))).toEqual([
      '0',
      '7',
      '7',
    ])
    const events = sender.send.mock.calls.map(([channel, event, watchId]) => {
      expect(channel).toBe('desktop:user-event')
      expect(watchId).toBe('watch')
      return event
    })
    expect(
      events.filter(({ kind }) => kind === 'message.created').map(({ sequence }) => sequence),
    ).toEqual([7, 8])
    expect(events.filter(({ kind }) => kind === 'workspace.refresh')).toHaveLength(1)
  })

  it('连续失败达到上限后退出，恢复后的新订阅仍能刷新工作空间', async () => {
    vi.useFakeTimers()
    const sender = eventSender()
    const fetchEvents = vi
      .fn<(url: string, options: RequestInit) => Promise<Response>>()
      .mockRejectedValue(new TypeError('网络不可用'))
    vi.stubGlobal('fetch', fetchEvents)
    const onStopped = vi.fn()
    void invoke('desktop:watch-events', { sender }, 'first-watch').then(onStopped)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(fetchEvents).toHaveBeenCalledTimes(5)
    expect(onStopped).toHaveBeenCalledExactlyOnceWith(
      expect.objectContaining({ ok: false, code: 'EVENT_STREAM_UNAVAILABLE' }),
    )
    expect(sender.listenerCount('destroyed')).toBe(0)

    fetchEvents.mockImplementationOnce(
      async (_url, options) => openEventStream(options, []).response,
    )
    const watching = invoke('desktop:watch-events', { sender }, 'second-watch')
    await vi.advanceTimersByTimeAsync(0)
    await invoke('desktop:unwatch-events', { sender }, 'second-watch')
    expect(await watching).toMatchObject({ ok: true })
    expect(fetchEvents).toHaveBeenCalledTimes(6)
    expect(sender.send).toHaveBeenCalledExactlyOnceWith(
      'desktop:user-event',
      expect.objectContaining({ kind: 'workspace.refresh' }),
      'second-watch',
    )
  })

  it('退避中取消订阅会立即结束，并阻止后续请求', async () => {
    vi.useFakeTimers()
    const sender = eventSender()
    const fetchEvents = vi.fn().mockRejectedValue(new TypeError('网络不可用'))
    vi.stubGlobal('fetch', fetchEvents)
    const onStopped = vi.fn()
    void invoke('desktop:watch-events', { sender }, 'watch').then(onStopped)
    await vi.advanceTimersByTimeAsync(0)
    expect(fetchEvents).toHaveBeenCalledOnce()
    await invoke('desktop:unwatch-events', { sender }, 'watch')
    await vi.advanceTimersByTimeAsync(0)
    expect(onStopped).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({ ok: true }))
    expect(sender.listenerCount('destroyed')).toBe(0)
    await vi.advanceTimersByTimeAsync(30_000)
    expect(fetchEvents).toHaveBeenCalledOnce()
    expect(sender.send).not.toHaveBeenCalled()
  })
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
  registerDesktopIpc(vi.fn())
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

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
        expect(body.tool_permissions).toEqual([
          'computer_shell',
          'computer_browser',
          'computer_screenshot',
          'computer_input',
        ])
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

it('单聊上传走会话文件接口，发送附带文件标识并还原双方附件', async () => {
  const botId = ulid(),
    conversationId = ulid(),
    fileId = ulid(),
    messageId = ulid()
  const bytes = new TextEncoder().encode('附件内容')
  const file = {
    id: fileId,
    conversation_id: conversationId,
    name: '需求.txt',
    media_type: 'text/plain',
    size: bytes.length,
    sha256: createHash('sha256').update(bytes).digest('hex'),
    revision: 1,
    created_at: new Date().toISOString(),
  }
  const fetcher = vi.fn(async (url: string, options: RequestInit) => {
    const path = new URL(url).pathname
    if (path === '/api/v2/workspace')
      return Response.json({
        snapshot: {
          bots: [{ id: botId, conversation_id: conversationId, name: '小黑', revision: 1 }],
          groups: [],
          workspace: { revision: 1 },
          event_sequence: 0,
        },
      })
    if (path === `/api/v2/conversations/${conversationId}/files`) {
      expect(options.method).toBe('POST')
      expect(JSON.parse(String(options.body))).toMatchObject({
        client_request_id: fileId,
        name: '需求.txt',
      })
      return Response.json({ file })
    }
    if (path.endsWith('/activity')) return Response.json({ runs: [] })
    if (path === `/api/v2/conversations/${conversationId}/messages`) {
      if (options.method === 'POST') {
        expect(JSON.parse(String(options.body))).toMatchObject({ file_ids: [fileId] })
        return Response.json({ submission: { run_id: ulid(), message_id: messageId } })
      }
      return Response.json({
        messages: [
          {
            id: messageId,
            sequence: 1,
            sender_kind: 'system',
            created_at: file.created_at,
            blocks: [
              { kind: 'text', text: '需求.txt', user_message: true },
              { kind: 'chat_file', chat_file: file },
            ],
          },
        ],
      })
    }
    throw new Error('出现非会话附件请求')
  })
  vi.stubGlobal('fetch', fetcher)
  const uploaded = await invoke(
    'desktop:upload',
    {},
    { botId, name: file.name, type: 'text/plain', bytes, requestId: fileId },
  )
  const path = `chat-files/${conversationId}/${fileId}`
  expect(uploaded).toEqual({ ok: true, value: { path, sha256: file.sha256 } })
  expect(
    await invoke(
      'desktop:deliver',
      {},
      {
        agentId: botId,
        content: '需求',
        requestId: ulid(),
        clientRequestId: messageId,
        attachments: [{ path, sha256: file.sha256 }],
      },
    ),
  ).toMatchObject({ ok: true, value: { acceptedMessageId: messageId } })
  expect(await invoke('desktop:agent-conversation', {}, botId)).toMatchObject({
    ok: true,
    value: {
      messages: [
        { role: 'assistant', attachments: [{ path, name: file.name, sha256: file.sha256 }] },
      ],
    },
  })
})
