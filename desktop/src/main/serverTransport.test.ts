import { execFileSync } from 'node:child_process'
import { X509Certificate } from 'node:crypto'
import { mkdtemp, readFile, readdir, rm, stat, writeFile } from 'node:fs/promises'
import { createServer, type Server } from 'node:https'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { setTimeout, clearTimeout } from 'node:timers'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ServerTransport, connectionFailure } from './serverTransport'

let directory: string
const servers: Server[] = []
const transports: ServerTransport[] = []

beforeEach(async () => {
  directory = await mkdtemp(join(tmpdir(), 'tombot-certificate-test-'))
  vi.stubGlobal('setTimeout', setTimeout)
  vi.stubGlobal('clearTimeout', clearTimeout)
})
afterEach(async () => {
  await Promise.all(transports.splice(0).map((transport) => transport.close()))
  await Promise.all(
    servers.splice(0).map(
      (server) =>
        new Promise<void>((resolve, reject) => {
          server.closeAllConnections()
          server.close((error) => (error ? reject(error) : resolve()))
        }),
    ),
  )
  await rm(directory, { recursive: true, force: true })
  vi.unstubAllGlobals()
})

function transport(): ServerTransport {
  const result = new ServerTransport(join(directory, 'trust'))
  transports.push(result)
  return result
}

/** 临时签发与 tomd 一致的非 CA 叶证书；私钥不进入仓库或测试输出。 */
async function certificate(hostname = '127.0.0.1') {
  const prefix = join(directory, 'certificate-' + servers.length)
  execFileSync(
    'openssl',
    [
      'req',
      '-x509',
      '-newkey',
      'ec',
      '-pkeyopt',
      'ec_paramgen_curve:prime256v1',
      '-nodes',
      '-days',
      '1',
      '-subj',
      '/CN=certificate.test',
      '-addext',
      'subjectAltName=IP:' + hostname,
      '-addext',
      'basicConstraints=critical,CA:FALSE',
      '-keyout',
      prefix + '.key',
      '-out',
      prefix + '.pem',
    ],
    { timeout: 5000, stdio: 'ignore' },
  )
  const cert = await readFile(prefix + '.pem')
  return {
    cert,
    key: await readFile(prefix + '.key'),
    fingerprint: new X509Certificate(cert).fingerprint256,
  }
}

async function listen(material: Awaited<ReturnType<typeof certificate>>, redirect?: () => string) {
  let requests = 0
  const server = createServer(material, (_request, response) => {
    requests++
    if (redirect) response.writeHead(302, { location: redirect() })
    response.end('正常')
  })
  servers.push(server)
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })
  const address = server.address()
  if (!address || typeof address === 'string') throw new Error('测试服务器未监听')
  return { origin: `https://127.0.0.1:${address.port}`, server, requests: () => requests }
}

describe('按服务地址验证证书', () => {
  it('未信任前不发业务请求，核对后严格校验并持久保存，重启后可复用', async () => {
    const material = await certificate()
    const service = await listen(material)
    const client = transport()
    await expect(
      client.fetch(service.origin, { signal: AbortSignal.timeout(3000) }),
    ).rejects.toMatchObject({ code: 'CERTIFICATE_UNTRUSTED' })
    expect(service.requests()).toBe(0)
    await client.trustCertificate(service.origin, material.fingerprint)
    expect(service.requests()).toBe(0)
    expect(await (await client.fetch(service.origin, {})).text()).toBe('正常')
    const files = await readdir(join(directory, 'trust'))
    expect(files).toHaveLength(1)
    expect((await stat(join(directory, 'trust', files[0]!))).mode & 0o777).toBe(0o600)
    await client.close()
    expect(await (await transport().fetch(service.origin, {})).text()).toBe('正常')
    expect(service.requests()).toBe(2)
  })

  it('指纹格式和匹配失败不保存信任、不发请求', async () => {
    const material = await certificate()
    const service = await listen(material)
    const client = transport()
    await expect(client.trustCertificate(service.origin, '短指纹')).rejects.toMatchObject({
      code: 'CERTIFICATE_FINGERPRINT_INVALID',
    })
    await expect(client.trustCertificate(service.origin, '0'.repeat(64))).rejects.toMatchObject({
      code: 'CERTIFICATE_FINGERPRINT_MISMATCH',
    })
    await expect(readdir(join(directory, 'trust'))).rejects.toMatchObject({ code: 'ENOENT' })
    expect(service.requests()).toBe(0)
  })

  it('指纹一致也不能接受主机不匹配或已过期的证书', async () => {
    const material = await certificate('127.0.0.2')
    const service = await listen(material)
    const client = transport()
    await expect(
      client.trustCertificate(service.origin, material.fingerprint),
    ).rejects.toMatchObject({ code: 'CERTIFICATE_HOSTNAME' })
    vi.spyOn(Date, 'now').mockReturnValue(Date.now() + 2 * 24 * 60 * 60 * 1000)
    await expect(
      client.trustCertificate(service.origin, material.fingerprint),
    ).rejects.toMatchObject({ code: 'CERTIFICATE_EXPIRED' })
    expect(service.requests()).toBe(0)
  })

  it('相同证书不能自动扩展到其他端口，也不跟随跨站重定向', async () => {
    const material = await certificate()
    const other = await listen(material)
    const service = await listen(material, () => other.origin)
    const client = transport()
    await client.trustCertificate(service.origin, material.fingerprint)
    await expect(client.fetch(other.origin, {})).rejects.toMatchObject({
      code: 'CERTIFICATE_UNTRUSTED',
    })
    await expect(client.fetch(service.origin, {})).rejects.toBeInstanceOf(Error)
    expect(other.requests()).toBe(0)
    expect(service.requests()).toBe(1)
  })

  it('证书更换后旧信任拒绝请求，重新核对才能恢复', async () => {
    const material = await certificate()
    const service = await listen(material)
    const client = transport()
    await client.trustCertificate(service.origin, material.fingerprint)
    expect(await (await client.fetch(service.origin, {})).text()).toBe('正常')
    await client.close()
    const replacement = await certificate()
    service.server.setSecureContext(replacement)
    await expect(client.fetch(service.origin, {})).rejects.toMatchObject({
      code: 'CERTIFICATE_UNTRUSTED',
    })
    expect(service.requests()).toBe(1)
    await client.trustCertificate(service.origin, replacement.fingerprint)
    expect(await (await client.fetch(service.origin, {})).text()).toBe('正常')
  })

  it('损坏的信任记录不能回退到默认信任，重新核对可以修复', async () => {
    const material = await certificate()
    const service = await listen(material)
    const client = transport()
    await client.trustCertificate(service.origin, material.fingerprint)
    const [file] = await readdir(join(directory, 'trust'))
    await writeFile(join(directory, 'trust', file!), '{')
    await expect(client.fetch(service.origin, {})).rejects.toMatchObject({
      code: 'CERTIFICATE_STORE_INVALID',
    })
    expect(service.requests()).toBe(0)
    await client.trustCertificate(service.origin, material.fingerprint)
    expect(await (await client.fetch(service.origin, {})).text()).toBe('正常')
  })

  it('保存失败明确报错，不能返回信任成功', async () => {
    const material = await certificate()
    const service = await listen(material)
    await writeFile(join(directory, 'trust'), '阻止创建目录')
    await expect(
      transport().trustCertificate(service.origin, material.fingerprint),
    ).rejects.toMatchObject({ code: 'CERTIFICATE_STORE_FAILED' })
    expect(service.requests()).toBe(0)
  })

  it.each([
    ['CERT_HAS_EXPIRED', 'CERTIFICATE_EXPIRED'],
    ['ERR_TLS_CERT_ALTNAME_INVALID', 'CERTIFICATE_HOSTNAME'],
    ['ECONNREFUSED', 'CONNECTION_REFUSED'],
    ['ENOTFOUND', 'CONNECTION_DNS'],
    ['UND_ERR_CONNECT_TIMEOUT', 'CONNECTION_TIMEOUT'],
    ['DEPTH_ZERO_SELF_SIGNED_CERT', 'CERTIFICATE_UNTRUSTED'],
  ])('区分底层连接错误：%s', (code, expected) => {
    const error = new Error('底层信息不可直接展示', {
      cause: Object.assign(new Error('不可展示'), { code }),
    })
    expect(connectionFailure(error)).toMatchObject({ code: expected, isRequestUnsent: true })
    expect(connectionFailure(error).message).not.toContain('不可展示')
  })

  it('区分连接前不可达与发送后可能发生的超时和断流', () => {
    expect(connectionFailure({ code: 'ENETUNREACH', syscall: 'connect' })).toMatchObject({
      code: 'OFFLINE',
      isRequestUnsent: true,
    })
    expect(connectionFailure({ name: 'TimeoutError' })).toMatchObject({
      code: 'CONNECTION_TIMEOUT',
      isRequestUnsent: false,
    })
    expect(connectionFailure({ code: 'UND_ERR_SOCKET' })).toMatchObject({
      code: 'OFFLINE',
      isRequestUnsent: false,
    })
  })
})
