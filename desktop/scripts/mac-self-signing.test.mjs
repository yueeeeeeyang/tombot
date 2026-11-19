import { expect, it } from 'vitest'
import { findSelfSigningIdentity, publisherRequirement } from './mac-self-signing.mjs'
import { macSigningMode } from './mac-release-signing.mjs'

const signer = 'A'.repeat(40)
const other = 'B'.repeat(40)
const identity = (hash, suffix = '') => '  1) ' + hash + ' "TomBot Desktop Updates"' + suffix

it('免费签名默认启用，显式指定的 Apple 渠道继续独立打包', () => {
  expect(macSigningMode({})).toBe('self-signed')
  expect(macSigningMode({ CSC_NAME: '团队' })).toBe('developer-id')
  expect(macSigningMode({ CSC_LINK: '证书', TOMBOT_MAC_SIGNING: 'self-signed' })).toBe(
    'self-signed',
  )
  expect(() => macSigningMode({ TOMBOT_MAC_SIGNING: 'unsigned' })).toThrow('TOMBOT_MAC_SIGNING')
})

it('自签名身份只接受精确名称，系统无需安装信任根', () => {
  const output = [
    identity(other).replace('TomBot Desktop Updates', 'Apple Development: 测试'),
    identity(signer, ' (CSSMERR_TP_NOT_TRUSTED)'),
    identity(signer),
  ].join('\n')
  expect(
    findSelfSigningIdentity('/test', (tool, args) => {
      expect(tool).toBe('/usr/bin/security')
      expect(args).toEqual(['find-identity', '-p', 'codesigning', '/test'])
      return output
    }),
  ).toBe(signer)
  expect(findSelfSigningIdentity(null, () => '0 identities found')).toBeUndefined()
})

it('重复、过期或撤销的自签证书在分发前报错', () => {
  expect(() =>
    findSelfSigningIdentity(null, () => identity(signer) + '\n' + identity(other)),
  ).toThrow('多个')
  for (const reason of ['CSSMERR_TP_CERT_EXPIRED', 'CSSMERR_TP_CERT_REVOKED'])
    expect(() =>
      findSelfSigningIdentity(null, () => identity(signer, ' (' + reason + ')')),
    ).toThrow('失效')
})

it('自动升级的签名要求同时固定产品标识和实际证书', () => {
  expect(publisherRequirement(signer)).toBe(
    'identifier "com.tombot.desktop" and certificate leaf = H"' + signer + '"',
  )
  expect(() => publisherRequirement('-')).toThrow('指纹')
  expect(publisherRequirement(signer)).not.toBe(publisherRequirement(other))
})
