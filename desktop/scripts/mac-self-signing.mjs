import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

const commonName = 'TomBot Desktop Updates'
const appIdentifier = 'com.tombot.desktop'
const missingIdentity =
  '缺少 TomBot 自签名身份。首次发行请执行 npm --prefix desktop run signing:init；已有发行版本请恢复原证书及私钥。'

/** 自签名身份不依赖系统根信任，以精确名称和指纹选择，拒绝过期、重复或丢失私钥的身份。 */
export function findSelfSigningIdentity(keychain, run = execFileSync) {
  const output = run(
    '/usr/bin/security',
    ['find-identity', '-p', 'codesigning', ...(keychain ? [keychain] : [])],
    { encoding: 'utf8', timeout: 15_000, stdio: 'pipe' },
  )
  const matches = new Set()
  for (const line of output.split('\n')) {
    const entry = /^\s*\d+\)\s+([a-f0-9]{40})\s+"([^"]+)"(.*)$/i.exec(line)
    if (entry?.[2] !== commonName) continue
    const status = entry[3].trim()
    if (status && status !== '(CSSMERR_TP_NOT_TRUSTED)')
      throw new Error('TomBot 自签名证书已失效，请检查证书有效期及钥匙串状态。')
    matches.add(entry[1].toUpperCase())
  }
  if (matches.size > 1)
    throw new Error(
      '有多个同名 TomBot 自签名身份，请用 CSC_KEYCHAIN 指定仅包含原发行身份的钥匙串。',
    )
  return [...matches][0]
}

/** 发布时只读取已初始化的身份，避免迁移构建机或丢失钥匙串后自动更换发布者。 */
export function requireSelfSigningIdentity(keychain) {
  const identity = findSelfSigningIdentity(keychain)
  if (!identity) throw new Error(missingIdentity)
  return identity
}

/** 只在首次发行时创建私钥并导入构建用户钥匙串，整个过程不修改系统信任设置。 */
export async function initializeSelfSigningIdentity(keychain = process.env.CSC_KEYCHAIN) {
  if (process.platform !== 'darwin') throw new Error('macOS 发行身份需要在 Mac 上初始化。')
  const identity = findSelfSigningIdentity(keychain)
  if (identity) return identity
  const lock = join(tmpdir(), 'tombot-signing-init-' + process.getuid())
  try {
    await mkdir(lock, { mode: 0o700 })
  } catch (cause) {
    if (cause.code === 'EEXIST')
      throw new Error(
        'TomBot 发行身份正在初始化。若此前被强制中断，请确认初始化已退出后清理锁目录 ' + lock,
        { cause },
      )
    throw cause
  }
  try {
    return findSelfSigningIdentity(keychain) ?? (await createIdentity(keychain))
  } finally {
    await rm(lock, { recursive: true })
  }
}

/** 已持有初始化锁时创建身份，保留已有证书供恢复。 */
async function createIdentity(keychain) {
  // 钥匙串中若已有孤立证书，应恢复配套私钥，绝不能以同名新证书覆盖。
  const certificates = execFileSync(
    '/usr/bin/security',
    ['find-certificate', '-a', '-c', commonName, '-Z', ...(keychain ? [keychain] : [])],
    { encoding: 'utf8', timeout: 15_000 },
  )
  if (certificates.trim()) throw new Error('已有 TomBot 发行证书，请恢复其配套私钥后重新打包。')
  const directory = await mkdtemp(join(tmpdir(), 'tombot-signing-'))
  const passphrase = randomBytes(32).toString('base64url')
  const settings = {
    cwd: directory,
    timeout: 30_000,
    stdio: 'pipe',
    env: { ...process.env, TOMBOT_TEMPORARY_P12: passphrase },
  }
  try {
    await writeFile(
      join(directory, 'certificate.cnf'),
      [
        '[req]',
        'prompt=no',
        'distinguished_name=dn',
        'x509_extensions=usage',
        '[dn]',
        'CN=' + commonName,
        '[usage]',
        'basicConstraints=critical,CA:FALSE',
        'keyUsage=critical,digitalSignature',
        'extendedKeyUsage=critical,codeSigning',
        'subjectKeyIdentifier=hash',
      ].join('\n') + '\n',
      { mode: 0o600 },
    )
    // 暂存私钥也加密，导入时仅授权系统 codesign 使用。
    execFileSync(
      '/usr/bin/openssl',
      [
        'req',
        '-newkey',
        'rsa:3072',
        '-x509',
        '-sha256',
        '-days',
        '7300',
        '-config',
        'certificate.cnf',
        '-keyout',
        'signer.pem',
        '-out',
        'signer.crt',
        '-passout',
        'env:TOMBOT_TEMPORARY_P12',
      ],
      settings,
    )
    execFileSync(
      '/usr/bin/openssl',
      [
        'pkcs12',
        '-export',
        '-inkey',
        'signer.pem',
        '-in',
        'signer.crt',
        '-out',
        'identity.p12',
        '-passin',
        'env:TOMBOT_TEMPORARY_P12',
        '-passout',
        'env:TOMBOT_TEMPORARY_P12',
      ],
      settings,
    )
    // 错误对象的命令行可能含临时口令，避免将原始错误传播到日志。
    try {
      execFileSync(
        '/usr/bin/security',
        [
          'import',
          'identity.p12',
          '-f',
          'pkcs12',
          ...(keychain ? ['-k', keychain] : []),
          '-P',
          passphrase,
          '-T',
          '/usr/bin/codesign',
        ],
        settings,
      )
    } catch {
      throw new Error('TomBot 发行身份导入失败，请检查目标钥匙串的解锁状态和写入权限。')
    }
    return requireSelfSigningIdentity(keychain)
  } finally {
    await rm(directory, { recursive: true, force: true })
  }
}

/** 将发布者证书与应用标识写入主程序的指定要求。Squirrel 用已安装版本的要求验证每次升级。 */
export function publisherRequirement(identity) {
  if (!/^[0-9A-F]{40}$/.test(identity)) throw new Error('TomBot 自签名证书指纹无效。')
  return 'identifier "' + appIdentifier + '" and certificate leaf = H"' + identity + '"'
}

/** 保留打包器的逐文件权限与加固配置，通过系统签名器对所有嵌套代码签名。 */
export async function signSelfSignedApp(options, packager) {
  const { signAsync } = await import('@electron/osx-sign')
  const keychain = (await packager.codeSigningInfo.value).keychainFile
  const identity = requireSelfSigningIdentity(keychain)
  const requirement = publisherRequirement(identity)
  await signAsync({
    ...options,
    identity,
    identityValidation: false,
    preAutoEntitlements: false,
    optionsForFile: (file) => ({
      ...options.optionsForFile(file),
      timestamp: 'none',
      ...(file === options.app ? { requirements: '=designated => ' + requirement } : {}),
    }),
  })
}

/** 归档前独立检查包与发布者，打包器跳过签名或身份被替换时阻止发布。 */
export async function verifySelfSignedApp({ electronPlatformName, appOutDir, packager }) {
  if (electronPlatformName !== 'darwin') return
  const keychain = (await packager.codeSigningInfo.value).keychainFile
  const requirement = publisherRequirement(requireSelfSigningIdentity(keychain))
  const app = join(appOutDir, packager.appInfo.productFilename + '.app')
  execFileSync(
    '/usr/bin/codesign',
    ['--verify', '--deep', '--strict', '--all-architectures', '-R=' + requirement, app],
    { timeout: 120_000, stdio: 'pipe' },
  )
  console.info('macOS 自签名验证通过，应用=' + packager.appInfo.productFilename)
}

if (process.argv[1] && pathToFileURL(process.argv[1]).href === import.meta.url) {
  initializeSelfSigningIdentity()
    .then((identity) => console.info('TomBot 自签名身份已就绪，证书指纹 ' + identity))
    .catch((error) => {
      console.error('初始化 macOS 发行身份失败：' + error.message)
      process.exitCode = 1
    })
}
