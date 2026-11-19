import { execFileSync } from 'node:child_process'
import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { requireSelfSigningIdentity } from './mac-self-signing.mjs'

/** 默认使用 TomBot 自签名；已显式配置 Apple 发行身份的流水线沿用原渠道。 */
export function macSigningMode(env = process.env) {
  const mode =
    env.TOMBOT_MAC_SIGNING ?? (env.CSC_LINK || env.CSC_NAME ? 'developer-id' : 'self-signed')
  if (mode !== 'developer-id' && mode !== 'self-signed')
    throw new Error('TOMBOT_MAC_SIGNING 须为 self-signed 或 developer-id。')
  return mode
}

/** 在安装依赖前核验本地发行身份；CI 临时证书由打包器导入后核验。 */
export function checkLocalMacIdentity() {
  if (process.env.CSC_LINK) return
  if (macSigningMode() === 'self-signed') requireSelfSigningIdentity(process.env.CSC_KEYCHAIN)
  else findMacReleaseIdentity(process.env.CSC_KEYCHAIN)
}

/** 从指定钥匙串选择带私钥的有效发行身份，返回指纹，防止打包器回退到开发证书。 */
export function findMacReleaseIdentity(
  keychain,
  qualifier = process.env.CSC_NAME,
  run = execFileSync,
) {
  let identities
  try {
    identities = run(
      '/usr/bin/security',
      ['find-identity', '-v', '-p', 'codesigning', ...(keychain ? [keychain] : [])],
      { encoding: 'utf8', timeout: 15_000, stdio: 'pipe' },
    )
  } catch (cause) {
    throw new Error('无法读取 macOS 签名身份，请确认构建用户可以访问配置的钥匙串。', { cause })
  }
  const selector = qualifier?.trim() ?? ''
  const matches = new Set()
  for (const line of identities.split('\n')) {
    const identity = /^\s*\d+\)\s+([\da-f]{40})\s+"(Developer ID Application: [^"]+)"\s*$/i.exec(
      line,
    )
    if (
      identity &&
      (identity[2].includes(selector) || identity[1].toUpperCase() === selector.toUpperCase())
    )
      matches.add(identity[1].toUpperCase())
  }
  if (!matches.size)
    throw new Error(
      '未找到匹配的 Developer ID Application 发行身份。请导入发行证书及配套私钥，并检查 CSC_NAME 和所用钥匙串。',
    )
  if (matches.size > 1)
    throw new Error(
      '匹配到多个 Developer ID Application 发行身份。请用 CSC_NAME 指定本产品发行证书的完整指纹。',
    )
  return [...matches][0]
}

/** 在 macOS 打包前，用实际发行身份试签临时文件，同时核验私钥权限和系统安全时间戳。 */
export async function verifyMacReleaseSigning(
  { electronPlatformName, packager },
  run = execFileSync,
) {
  if (electronPlatformName !== 'darwin') return
  // CI 导入的证书可能只在打包器的临时钥匙串中可见。
  const { keychainFile } = await packager.codeSigningInfo.value
  const mac = packager.platformSpecificBuildOptions
  const identity = findMacReleaseIdentity(keychainFile, mac.identity ?? process.env.CSC_NAME, run)
  const directory = await mkdtemp(join(tmpdir(), 'tombot-mac-signing-'))
  try {
    const probe = join(directory, 'TomBot')
    await writeFile(probe, 'TomBot', { mode: 0o600 })
    try {
      run(
        '/usr/bin/codesign',
        [
          '--sign',
          identity,
          '--timestamp',
          ...(keychainFile ? ['--keychain', keychainFile] : []),
          probe,
        ],
        {
          timeout: 60_000,
          encoding: 'utf8',
          stdio: 'pipe',
          env: { ...process.env, LC_ALL: 'C' },
        },
      )
    } catch (error) {
      if (/timestamp/i.test(String(error.stderr)))
        throw new Error(
          'macOS 发行签名无法取得 Apple 安全时间戳。请检查构建机到 timestamp.apple.com 的网络、代理与防火墙后重试。',
          { cause: error },
        )
      if (error.code === 'ETIMEDOUT')
        throw new Error('macOS 发行试签超时。请检查钥匙串授权及 Apple 时间戳服务连接后重试。', {
          cause: error,
        })
      throw new Error(
        'macOS 发行试签失败。请检查发行证书的信任链、私钥访问权限及钥匙串解锁状态。',
        { cause: error },
      )
    }
    mac.identity = identity
  } finally {
    await rm(directory, { recursive: true, force: true })
  }
}
