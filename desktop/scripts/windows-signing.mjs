import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { accessSync } from 'node:fs'
import { chmod, copyFile, mkdir, mkdtemp, rename, rm, writeFile } from 'node:fs/promises'
import { homedir, tmpdir, userInfo } from 'node:os'
import { dirname, join } from 'node:path'

const service = 'TomBot Windows Updates'
const signingRoot =
  process.platform === 'darwin'
    ? join(homedir(), 'Library', 'Application Support')
    : process.env.LOCALAPPDATA || process.env.XDG_CONFIG_HOME || join(homedir(), '.config')
const identityDir = join(signingRoot, 'TomBot Signing', 'Windows')
const pfxPath = join(identityDir, 'publisher.pfx')
const options = { encoding: 'utf8', stdio: 'pipe', timeout: 30000 }
const openssl = process.platform === 'darwin' ? '/usr/bin/openssl' : 'openssl'

/** 本机身份只在显式初始化时生成，恢复构建机时可继续使用备份的 PFX。 */
export function checkLocalWindowsIdentity(directory = identityDir) {
  if (process.env.WIN_CSC_LINK) return
  try {
    accessSync(join(directory, 'publisher.pfx'))
  } catch (cause) {
    throw new Error(
      '缺少 Windows 发行身份。首次发行请运行 npm --prefix desktop run signing:init，已有客户端请恢复原 PFX。',
      { cause },
    )
  }
}

/** 仅访问本产品自己的钥匙串项目，CI 可直接通过标准环境变量注入口令。 */
function passphrase() {
  if (process.env.WIN_CSC_KEY_PASSWORD) return process.env.WIN_CSC_KEY_PASSWORD
  if (process.platform === 'darwin') {
    try {
      return execFileSync(
        '/usr/bin/security',
        ['find-generic-password', '-s', service, '-a', userInfo().username, '-w'],
        options,
      ).trimEnd()
    } catch {
      throw new Error('无法读取 Windows 发行口令，请解锁钥匙串或配置 WIN_CSC_KEY_PASSWORD。')
    }
  }
  throw new Error('请在构建环境配置 WIN_CSC_KEY_PASSWORD 以保护和读取 Windows 发行身份。')
}

/** 生成长期使用的加密 PKCS#12；密钥留在用户目录，打包工作副本仅接触公开程序。 */
export async function initializeWindowsIdentity(directory = identityDir) {
  if (process.env.WIN_CSC_LINK) {
    console.info('Windows 将使用 WIN_CSC_LINK 指定的发行证书。')
    return
  }
  const identity = join(directory, 'publisher.pfx')
  await mkdir(dirname(directory), { recursive: true, mode: 0o700 })
  try {
    await mkdir(directory, { mode: 0o700 })
  } catch (cause) {
    if (cause.code !== 'EEXIST') throw cause
    checkLocalWindowsIdentity(directory)
    const password = passphrase()
    try {
      execFileSync(
        openssl,
        ['pkcs12', '-in', identity, '-noout', '-passin', 'env:TOMBOT_PFX_PASSPHRASE'],
        { ...options, env: { ...process.env, TOMBOT_PFX_PASSPHRASE: password } },
      )
    } catch {
      throw new Error('无法打开 Windows 发行身份，请检查 PFX 与口令是否匹配。')
    }
    console.info('已复用现有 Windows 发行身份。')
    return
  }
  let temp
  try {
    temp = await mkdtemp(join(tmpdir(), 'tombot-windows-sign-'))
    const storeInKeychain = process.platform === 'darwin' && !process.env.WIN_CSC_KEY_PASSWORD
    const password = storeInKeychain ? randomBytes(32).toString('base64url') : passphrase()
    const env = { ...process.env, TOMBOT_PFX_PASSPHRASE: password }
    const run = (args) => execFileSync(openssl, args, { ...options, cwd: temp, env })
    await writeFile(
      join(temp, 'certificate.cnf'),
      '[req]\nprompt=no\ndistinguished_name=dn\nx509_extensions=extensions\n[dn]\nCN=TomBot Windows Updates\n[extensions]\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=critical,codeSigning\nsubjectKeyIdentifier=hash\n',
      { mode: 0o600 },
    )
    run([
      'req',
      '-x509',
      '-newkey',
      'rsa:3072',
      '-sha256',
      '-days',
      '7300',
      '-config',
      'certificate.cnf',
      '-keyout',
      'private.pem',
      '-out',
      'publisher.crt',
      '-passout',
      'env:TOMBOT_PFX_PASSPHRASE',
    ])
    run([
      'pkcs12',
      '-export',
      '-inkey',
      'private.pem',
      '-in',
      'publisher.crt',
      '-out',
      'publisher.pfx',
      '-keypbe',
      'AES-256-CBC',
      '-certpbe',
      'AES-256-CBC',
      '-macalg',
      'sha256',
      '-passin',
      'env:TOMBOT_PFX_PASSPHRASE',
      '-passout',
      'env:TOMBOT_PFX_PASSPHRASE',
    ])
    const pending = join(directory, 'publisher.pending')
    await copyFile(join(temp, 'publisher.pfx'), pending)
    await chmod(pending, 0o600)
    await rename(pending, identity)
    if (storeInKeychain) {
      // 禁止覆盖已有口令，原 PFX 缺失时需先恢复备份。
      try {
        execFileSync(
          '/usr/bin/security',
          ['add-generic-password', '-s', service, '-a', userInfo().username, '-w', password],
          options,
        )
      } catch {
        throw new Error('无法保存 Windows 发行口令。若已有同名钥匙串项目，请恢复原 PFX。')
      }
    }
    console.info('Windows 自签名身份已生成：' + identity)
  } catch (cause) {
    await rm(directory, { recursive: true, force: true })
    throw cause
  } finally {
    if (temp) await rm(temp, { recursive: true, force: true })
  }
}

/** 每个平台由独立打包进程运行，Windows 使用自己的签名身份和 SHA-256 Authenticode。 */
export function configureWindowsSigning() {
  if (process.env.WIN_CSC_LINK) return
  checkLocalWindowsIdentity()
  process.env.WIN_CSC_KEY_PASSWORD = passphrase()
  process.env.WIN_CSC_LINK = pfxPath
}

/** 使用构建机的 Go 工具为本地发行身份生成 Authenticode 签名，口令只通过环境传递。 */
export function signWindowsFile({ path, cscInfo }, packager) {
  execFileSync('go', ['run', '-mod=readonly', './cmd/sign', path, cscInfo.file], {
    cwd: join(packager.projectDir, 'windows-verifier'),
    stdio: 'inherit',
    timeout: 180000,
  })
}

/** NSIS 的安装入口和独立验签器统一使用 x86，两个架构的客户端均可直接调用。 */
export async function packWindowsVerifier({ electronPlatformName, appOutDir, packager }) {
  if (electronPlatformName !== 'win32') return
  const binary = join(appOutDir, 'resources', 'tombot-verify.exe')
  execFileSync(
    'go',
    ['build', '-mod=readonly', '-trimpath', '-buildvcs=false', '-ldflags=-s -w', '-o', binary, '.'],
    {
      cwd: join(packager.projectDir, 'windows-verifier'),
      env: { ...process.env, CGO_ENABLED: '0', GOOS: 'windows', GOARCH: '386' },
      stdio: 'inherit',
      timeout: 300000,
    },
  )
  await packager.signIf(binary)
}
