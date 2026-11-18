import { execFile } from 'node:child_process'
import { createHash, randomBytes, X509Certificate } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { createServer } from 'node:https'
import { join } from 'node:path'
import { promisify } from 'node:util'
import type { ServerSnapshot } from '../../src/main/workspaceSnapshot'

const exec = promisify(execFile)
const bot = '01ARZ3NDEKTSV4RRFFQ69G5FAV'
const conversation = '01ARZ3NDEKTSV4RRFFQ69G5FAW'
const principal = { user_id: '01ARZ3NDEKTSV4RRFFQ69G5FAX', username: 'session-test' }
const snapshot: ServerSnapshot = {
  workspace: { active_bot_id: bot, primary_bot_id: bot, revision: 1 },
  bots: [
    {
      id: bot,
      name: '小黑',
      description: '',
      avatar_ref: 'bombay',
      category: '我的助手',
      system_prompt: '',
      tool_permissions: [],
      approval_mode: 'standard',
      accent_hue: 0,
      desktop_id: '01ARZ3NDEKTSV4RRFFQ69G5FAY',
      conversation_id: conversation,
      has_custom_avatar: false,
      is_archived: false,
      revision: 1,
    },
  ],
  groups: [],
  event_sequence: 0,
  has_more_bots: false,
  has_more_groups: false,
}

interface Authorization {
  callback: URL
  state: string
  challenge: string
  code: string | undefined
}

function reply(response: ServerResponse, status: number, content: unknown): void {
  response.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(content))
}

async function readBody(request: IncomingMessage): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = []
  let size = 0
  for await (const chunk of request) {
    const bytes = Buffer.from(chunk as Uint8Array)
    size += bytes.byteLength
    if (size > 4096) throw new Error('测试请求超过大小限制')
    chunks.push(bytes)
  }
  return JSON.parse(Buffer.concat(chunks).toString('utf8')) as Record<string, unknown>
}

/** 为 Electron 会话测试提供临时 HTTPS 和精简的 v2 协议替身。真实网页登录由服务端集成测试覆盖。 */
export async function startSessionServer(directory: string) {
  const keyFile = join(directory, 'test-key.pem')
  const certificateFile = join(directory, 'test-certificate.pem')
  await exec(
    'openssl',
    [
      'req',
      '-x509',
      '-newkey',
      'rsa:2048',
      '-nodes',
      '-days',
      '1',
      '-sha256',
      '-subj',
      '/CN=127.0.0.1',
      '-addext',
      'subjectAltName=IP:127.0.0.1',
      '-keyout',
      keyFile,
      '-out',
      certificateFile,
    ],
    { timeout: 10_000 },
  )
  const [key, cert] = await Promise.all([readFile(keyFile), readFile(certificateFile)])
  const fingerprint = new X509Certificate(cert).fingerprint256.replaceAll(':', '').toLowerCase()
  const authorizations = new Map<string, Authorization>()
  const requests: string[] = []
  const unexpectedRequests: string[] = []
  const credentials = {
    access_token: randomBytes(32).toString('base64url'),
    refresh_token: randomBytes(32).toString('base64url'),
    access_expires_at: new Date(Date.now() + 3_600_000).toISOString(),
    principal,
  }

  async function serve(request: IncomingMessage, response: ServerResponse, route: string) {
    if (route === 'POST /api/v2/auth/browser/start') {
      const { redirect_uri: address, state, code_challenge: challenge } = await readBody(request)
      if (
        request.headers.authorization ||
        typeof address !== 'string' ||
        typeof state !== 'string' ||
        !/^[A-Za-z0-9_-]{43}$/.test(state) ||
        typeof challenge !== 'string' ||
        !/^[A-Za-z0-9_-]{43}$/.test(challenge)
      )
        throw new Error('浏览器授权请求格式异常')
      const callback = new URL(address)
      if (
        callback.protocol !== 'http:' ||
        callback.hostname !== '127.0.0.1' ||
        !callback.port ||
        callback.pathname !== '/callback' ||
        callback.search ||
        callback.hash ||
        callback.username ||
        callback.password
      )
        throw new Error('授权回调不是独立的本机回环地址')
      const id = randomBytes(16).toString('hex')
      authorizations.set(id, { callback, state, challenge, code: undefined })
      return reply(response, 200, { authorization_url: `/authorize?authorization=${id}` })
    }
    if (route === 'POST /api/v2/auth/browser/exchange') {
      const { code, code_verifier: verifier, state } = await readBody(request)
      const entry =
        typeof code === 'string' &&
        [...authorizations.entries()].find(([, value]) => value.code === code)
      if (
        request.headers.authorization ||
        !entry ||
        state !== entry[1].state ||
        typeof verifier !== 'string' ||
        createHash('sha256').update(verifier).digest('base64url') !== entry[1].challenge
      )
        throw new Error('授权码、状态或挑战应答无效')
      authorizations.delete(entry[0])
      return reply(response, 200, credentials)
    }
    if (request.headers.authorization !== `Bearer ${credentials.access_token}`) {
      throw new Error('已登录端点缺少对应凭据')
    }
    switch (route) {
      case 'GET /api/v2/auth/session':
        return reply(response, 200, { principal })
      case 'GET /api/v2/auth/profile':
        return reply(response, 200, {
          profile: { username: principal.username, display_name: '会话测试', avatar: '' },
        })
      case 'GET /api/v2/workspace':
        return reply(response, 200, { snapshot })
      case 'GET /api/v2/groups':
        return reply(response, 200, { groups: [] })
      case 'GET /api/v2/activity':
      case `GET /api/v2/conversations/${conversation}/activity`:
        return reply(response, 200, { runs: [] })
      case `GET /api/v2/conversations/${conversation}/messages`:
        return reply(response, 200, { messages: [] })
      case 'POST /api/v2/conversations/previews':
        return reply(response, 200, {
          previews: [{ conversation_id: conversation, sequence: 0, content: '' }],
        })
      case 'GET /api/v2/events':
        response.writeHead(200, { 'content-type': 'text/event-stream' })
        response.flushHeaders()
        return
      default:
        throw new Error('测试未定义此端点')
    }
  }

  const server = createServer({ key, cert }, (request, response) => {
    const route = `${request.method} ${new URL(request.url ?? '/', 'https://127.0.0.1').pathname}`
    requests.push(route)
    void serve(request, response, route).catch(() => {
      unexpectedRequests.push(route)
      reply(response, 500, { code: 'FIXTURE_REQUEST_INVALID', message: '会话夹具收到异常请求。' })
    })
  })
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', () => {
      server.off('error', reject)
      resolve()
    })
  })
  const bound = server.address()
  if (!bound || typeof bound === 'string') throw new Error('测试服务没有分配到端口')
  const url = `https://127.0.0.1:${bound.port}`
  let shutdown: Promise<void> | undefined
  return {
    url,
    fingerprint,
    requests,
    unexpectedRequests,
    async approve(address: string, validState = true): Promise<Response> {
      const page = new URL(address)
      const request = authorizations.get(page.searchParams.get('authorization') ?? '')
      if (page.origin !== url || page.pathname !== '/authorize' || !request || request.code) {
        throw new Error('外部浏览器未收到有效授权地址')
      }
      request.code = randomBytes(32).toString('base64url')
      request.callback.searchParams.set('code', request.code)
      request.callback.searchParams.set(
        'state',
        validState ? request.state : randomBytes(32).toString('base64url'),
      )
      return fetch(request.callback, { redirect: 'error', signal: AbortSignal.timeout(5000) })
    },
    stop(): Promise<void> {
      return (shutdown ??= new Promise<void>((resolve, reject) => {
        server.close((error) => (error ? reject(error) : resolve()))
        server.closeAllConnections()
      }))
    },
  }
}
