import { stat } from 'node:fs/promises'
import { describe, expect, it, vi } from 'vitest'
import { findMacReleaseIdentity, verifyMacReleaseSigning } from './mac-release-signing.mjs'

const development = '1'.repeat(40)
const distribution = 'A'.repeat(40)
const another = 'B'.repeat(40)
const devLine = `1) ${development} "Apple Development: 测试"`
const releaseLine = `2) ${distribution} "Developer ID Application: 测试甲"`
const otherLine = `3) ${another} "Developer ID Application: 测试乙"`
const keychainFile = '/test/临时发行钥匙串'

describe('macOS 发行签名', () => {
  it('只匹配有效发行身份，支持明确选择名称或指纹', () => {
    const list = () => [devLine, releaseLine, otherLine].join('\n')
    expect(() => findMacReleaseIdentity(null, '', list)).toThrow('多个')
    expect(findMacReleaseIdentity(null, distribution.toLowerCase(), list)).toBe(distribution)
    expect(findMacReleaseIdentity(null, '测试乙', list)).toBe(another)
    expect(() => findMacReleaseIdentity(null, development, list)).toThrow('未找到')
  })

  it('开发证书、签发安装器的证书及失效证书均不能签发客户端', () => {
    const list = () =>
      [
        devLine,
        releaseLine.replace('Application', 'Installer'),
        releaseLine + ' (CSSMERR_TP_CERT_EXPIRED)',
      ].join('\n')
    expect(() => findMacReleaseIdentity(null, '', list)).toThrow('Developer ID Application')
    expect(() =>
      findMacReleaseIdentity(null, '', () => {
        throw new Error()
      }),
    ).toThrow('无法读取')
  })

  it.each([
    ['成功', null, ''],
    ['时间戳不可用', { stderr: 'The timestamp service is not available.' }, 'Apple 安全时间戳'],
    ['超时', { code: 'ETIMEDOUT' }, '试签超时'],
    ['钥匙串锁定', { stderr: 'User interaction is not allowed.' }, '私钥访问权限'],
  ])('用同一钥匙串和强制时间戳试签并清理临时文件：%s', async (_case, failure, message) => {
    const mac = { identity: '测试甲' }
    const packager = {
      platformSpecificBuildOptions: mac,
      codeSigningInfo: { value: Promise.resolve({ keychainFile }) },
    }
    let probe
    const run = (command, args) => {
      if (command === '/usr/bin/security') {
        expect(args).toEqual(['find-identity', '-v', '-p', 'codesigning', keychainFile])
        return devLine + '\n' + releaseLine
      }
      expect(command).toBe('/usr/bin/codesign')
      expect(args.slice(0, -1)).toEqual([
        '--sign',
        distribution,
        '--timestamp',
        '--keychain',
        keychainFile,
      ])
      probe = args.at(-1)
      if (failure) throw Object.assign(new Error(), failure)
    }
    const result = verifyMacReleaseSigning({ electronPlatformName: 'darwin', packager }, run)
    if (failure) {
      await expect(result).rejects.toThrow(message)
      expect(mac.identity).toBe('测试甲')
    } else {
      await result
      expect(mac.identity).toBe(distribution)
    }
    await expect(stat(probe)).rejects.toMatchObject({ code: 'ENOENT' })
  })

  it('其他平台无需访问 Apple 钥匙串', async () => {
    const run = vi.fn()
    await verifyMacReleaseSigning({ electronPlatformName: 'win32' }, run)
    await verifyMacReleaseSigning({ electronPlatformName: 'linux' }, run)
    expect(run).not.toHaveBeenCalled()
  })
})
