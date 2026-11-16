import { createHash, randomUUID, X509Certificate } from 'node:crypto'
import { mkdir, readFile, rename, rm, writeFile } from 'node:fs/promises'
import { isIP } from 'node:net'
import { join } from 'node:path'
import { checkServerIdentity, connect } from 'node:tls'
import { Agent } from 'undici'
import { normalizeServerAddress } from '../shared/serverAddress'

const messages: Record<string, string> = {
  CERTIFICATE_UNTRUSTED: '服务端 HTTPS 证书尚未受信任。请向管理员核对证书指纹后连接。',
  CERTIFICATE_CHANGED: '服务端证书与已信任的证书不同，请向管理员重新核对指纹。',
  CERTIFICATE_EXPIRED: '服务端证书已过期或尚未生效，请检查系统时间并联系管理员更新证书。',
  CERTIFICATE_HOSTNAME: '服务端证书与服务地址不匹配，请确认地址或联系管理员更新证书。',
  CERTIFICATE_FINGERPRINT_INVALID: '请输入完整的 64 位 SHA-256 证书指纹。',
  CERTIFICATE_FINGERPRINT_MISMATCH: '指纹与服务端证书不一致，未建立信任。请向管理员核对。',
  CERTIFICATE_STORE_INVALID: '已保存的证书信任记录无法读取，请重新核对证书指纹。',
  CERTIFICATE_STORE_FAILED: '无法保存证书信任，请检查客户端目录权限后重试。',
  CERTIFICATE_HTTPS_REQUIRED: '只有 HTTPS 服务需要核对证书。',
  CONNECTION_TIMEOUT: '连接服务超时，请确认地址和网络后重试。',
  CONNECTION_REFUSED: '服务未接受连接，请确认服务已启动且端口可访问。',
  CONNECTION_DNS: '无法解析服务地址，请检查域名和网络。',
  OFFLINE: '无法连接服务。请确认地址和网络后重试。',
}

class ServerConnectionError extends Error {
  constructor(readonly code: string) {
    super(messages[code])
    this.name = 'ServerConnectionError'
  }
}

/** 只返回固定分类，不把底层异常中的地址、请求或凭据带到界面。 */
export function connectionFailure(error: unknown): Error {
  let cause = error
  for (let depth = 0; cause && typeof cause === 'object' && depth < 8; depth++) {
    if (cause instanceof ServerConnectionError) return cause
    const current = cause as { code?: string; name?: string; cause?: unknown }
    const code = current.code ?? ''
    if (
      [
        'DEPTH_ZERO_SELF_SIGNED_CERT',
        'SELF_SIGNED_CERT_IN_CHAIN',
        'UNABLE_TO_VERIFY_LEAF_SIGNATURE',
        'UNABLE_TO_GET_ISSUER_CERT_LOCALLY',
        'UNABLE_TO_GET_ISSUER_CERT',
      ].includes(code)
    ) {
      return new ServerConnectionError('CERTIFICATE_UNTRUSTED')
    }
    if (['CERT_HAS_EXPIRED', 'CERT_NOT_YET_VALID'].includes(code))
      return new ServerConnectionError('CERTIFICATE_EXPIRED')
    if (code === 'ERR_TLS_CERT_ALTNAME_INVALID')
      return new ServerConnectionError('CERTIFICATE_HOSTNAME')
    if (['ETIMEDOUT', 'UND_ERR_CONNECT_TIMEOUT'].includes(code) || current.name === 'TimeoutError')
      return new ServerConnectionError('CONNECTION_TIMEOUT')
    if (code === 'ECONNREFUSED') return new ServerConnectionError('CONNECTION_REFUSED')
    if (['ENOTFOUND', 'EAI_AGAIN'].includes(code))
      return new ServerConnectionError('CONNECTION_DNS')
    cause = current.cause
  }
  return new ServerConnectionError('OFFLINE')
}

function normalizeFingerprint(input: unknown): string {
  const fingerprint =
    typeof input === 'string' ? input.trim().replaceAll(':', '').toLowerCase() : ''
  if (!/^[a-f0-9]{64}$/.test(fingerprint))
    throw new ServerConnectionError('CERTIFICATE_FINGERPRINT_INVALID')
  return fingerprint
}

function validateCertificate(
  certificate: X509Certificate,
  origin: string,
  fingerprint: string,
): void {
  if (normalizeFingerprint(certificate.fingerprint256) !== fingerprint)
    throw new ServerConnectionError('CERTIFICATE_FINGERPRINT_MISMATCH')
  const now = Date.now()
  if (Date.parse(certificate.validFrom) > now || Date.parse(certificate.validTo) <= now)
    throw new ServerConnectionError('CERTIFICATE_EXPIRED')
  const hostname = new URL(origin).hostname.replace(/^\[|\]$/g, '')
  const matches = isIP(hostname) ? certificate.checkIP(hostname) : certificate.checkHost(hostname)
  if (!matches) throw new ServerConnectionError('CERTIFICATE_HOSTNAME')
}

/** 仅进行有界 TLS 握手读取公钥证书；核对管理员提供的指纹前不发送任何 HTTP 请求。 */
async function readPeerCertificate(origin: string): Promise<X509Certificate> {
  const url = new URL(origin)
  const hostname = url.hostname.replace(/^\[|\]$/g, '')
  return new Promise((resolve, reject) => {
    const socket = connect({
      host: hostname,
      port: Number(url.port || 443),
      servername: isIP(hostname) ? undefined : hostname,
      rejectUnauthorized: false,
    })
    const timer = setTimeout(
      () => socket.destroy(new ServerConnectionError('CONNECTION_TIMEOUT')),
      5000,
    )
    socket.once('close', () => clearTimeout(timer))
    socket.once('error', reject)
    socket.once('secureConnect', () => {
      try {
        resolve(new X509Certificate(socket.getPeerCertificate().raw))
      } catch (error) {
        reject(error)
      } finally {
        socket.destroy()
      }
    })
  })
}

/**
 * 服务连接只信任系统证书或用户核对过的单个源站证书。
 * 信任独立于登录凭据保存，所有请求仍检查证书链、有效期、主机名和固定指纹。
 */
export class ServerTransport {
  private readonly agents = new Map<string, Promise<Agent | undefined>>()

  constructor(private readonly directory: string) {}

  private path(origin: string): string {
    return join(this.directory, createHash('sha256').update(origin).digest('hex') + '.json')
  }

  private async loadAgent(origin: string): Promise<Agent | undefined> {
    let raw: string
    try {
      raw = await readFile(this.path(origin), 'utf8')
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') return undefined
      throw new ServerConnectionError('CERTIFICATE_STORE_INVALID')
    }
    try {
      const record = JSON.parse(raw) as { origin: string; certificate: string; fingerprint: string }
      if (record.origin !== origin || typeof record.certificate !== 'string')
        throw new ServerConnectionError('CERTIFICATE_STORE_INVALID')
      const fingerprint = normalizeFingerprint(record.fingerprint)
      validateCertificate(new X509Certificate(record.certificate), origin, fingerprint)
      return new Agent({
        connect: {
          ca: record.certificate,
          rejectUnauthorized: true,
          // tomd 签发的是非 CA 叶证书；Electron 需要显式允许将已核对的叶证书作为信任锚。
          // 下方仍校验主机和完整证书指纹，不能把它扩展成任意签发者的信任。
          allowPartialTrustChain: true,
          maxCachedSessions: 0,
          timeout: 5000,
          checkServerIdentity: (hostname, certificate) => {
            const error = checkServerIdentity(hostname, certificate)
            if (error) return error
            if (normalizeFingerprint(certificate.fingerprint256) !== fingerprint)
              return new ServerConnectionError('CERTIFICATE_CHANGED')
            return undefined
          },
        },
      })
    } catch (error) {
      if (error instanceof ServerConnectionError && error.code === 'CERTIFICATE_EXPIRED')
        throw error
      throw new ServerConnectionError('CERTIFICATE_STORE_INVALID')
    }
  }

  /** 使用当前源站的信任发请求；禁止重定向携带凭据离开该源站，不自动重试。 */
  async fetch(url: string, options: RequestInit): Promise<Response> {
    const origin = new URL(url).origin
    let loading = this.agents.get(origin)
    if (!loading) {
      loading = this.loadAgent(origin)
      this.agents.set(origin, loading)
    }
    try {
      const dispatcher = await loading
      const request = {
        ...options,
        dispatcher,
        credentials: 'omit' as const,
        redirect: 'error' as const,
      }
      return await fetch(url, request)
    } catch (error) {
      if (options.signal?.aborted && options.signal.reason?.name !== 'TimeoutError') throw error
      throw connectionFailure(error)
    }
  }

  /** 核对指纹、主机和有效期后原子保存公共证书；失败保留原来的信任。 */
  async trustCertificate(address: string, input: unknown): Promise<void> {
    const origin = normalizeServerAddress(address)
    if (new URL(origin).protocol !== 'https:')
      throw new ServerConnectionError('CERTIFICATE_HTTPS_REQUIRED')
    const fingerprint = normalizeFingerprint(input)
    let certificate: X509Certificate
    try {
      certificate = await readPeerCertificate(origin)
      validateCertificate(certificate, origin, fingerprint)
    } catch (error) {
      throw connectionFailure(error)
    }
    const temporary = this.path(origin) + '.' + randomUUID()
    try {
      await mkdir(this.directory, { recursive: true, mode: 0o700 })
      await writeFile(
        temporary,
        JSON.stringify({ origin, fingerprint, certificate: certificate.toString() }),
        { flag: 'wx', mode: 0o600 },
      )
      await rename(temporary, this.path(origin))
    } catch {
      try {
        await rm(temporary, { force: true })
      } catch (error) {
        // 父路径不是目录时临时文件不可能已创建，保留上面的保存失败分类。
        if ((error as NodeJS.ErrnoException).code !== 'ENOTDIR')
          throw new ServerConnectionError('CERTIFICATE_STORE_FAILED')
      }
      throw new ServerConnectionError('CERTIFICATE_STORE_FAILED')
    }
    const previous = this.agents.get(origin)
    this.agents.delete(origin)
    if (previous) {
      // 损坏的旧记录不阻止用已核验的证书修复；连接池销毁失败则必须上报。
      const [result] = await Promise.allSettled([previous])
      if (result?.status === 'fulfilled') await result.value?.destroy()
    }
  }

  /** 释放所有连接，供进程收尾和测试使用。 */
  async close(): Promise<void> {
    const results = await Promise.allSettled(this.agents.values())
    this.agents.clear()
    await Promise.all(
      results.map((result) =>
        result.status === 'fulfilled' ? result.value?.destroy() : undefined,
      ),
    )
  }
}
