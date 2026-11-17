import type { ApplicationAbout } from '../renderer/src/features/desktop/backend'

/** 版本查询只读公共状态，不发送会话凭据；失败仍保留客户端版本和已连接地址。 */
export async function loadApplicationAbout(
  clientVersion: string,
  serverUrl: string,
  transport: { fetch: (url: string, options: RequestInit) => Promise<Response> },
): Promise<ApplicationAbout> {
  try {
    const response = await transport.fetch(`${serverUrl}/api/v2/version`, {
      method: 'GET',
      credentials: 'omit',
      redirect: 'error',
      signal: AbortSignal.timeout(10000),
    })
    if (!response.ok) throw new Error('服务端版本读取失败，请重试。')
    const payload = (await response.json()) as { server_version?: unknown }
    if (typeof payload.server_version !== 'string' || !payload.server_version.trim())
      throw new Error('服务端未提供版本信息。')
    return { clientVersion, serverUrl, serverVersion: payload.server_version }
  } catch (failure) {
    return {
      clientVersion,
      serverUrl,
      serverVersion: '',
      serverError: failure instanceof Error ? failure.message : '服务端版本读取失败，请重试。',
    }
  }
}
