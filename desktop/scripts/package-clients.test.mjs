import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { packageClients, selectPackages } from './package-clients.mjs'
import config from '../electron-builder.config.mjs'
import {
  checkLocalWindowsIdentity,
  initializeWindowsIdentity,
  packWindowsVerifier,
} from './windows-signing.mjs'

let output
beforeEach(async () => {
  output = await mkdtemp(join(tmpdir(), 'tombot-package-test-'))
})
afterEach(async () => {
  await rm(output, { recursive: true, force: true })
})

const all = selectPackages('all', 'all', 'darwin')
const linux = selectPackages('linux', 'arm64', 'linux')
const script = join(import.meta.dirname, 'package-clients.mjs')

/** 让真实 CLI 调用一个本机进程验证配置、架构、成品命名及禁止自动发布的约定。 */
async function prepare(workspace, version = '1.10.0') {
  const cli = join(workspace, 'node_modules/electron-builder')
  await mkdir(cli, { recursive: true })
  await writeFile(join(workspace, 'package.json'), JSON.stringify({ version }))
  await writeFile(
    join(cli, 'cli.js'),
    `
    const assert = require('node:assert/strict')
    const fs = require('node:fs')
    const path = require('node:path')
    const args = process.argv.slice(2)
    const [platform, arch] = args
    assert.deepEqual(args.slice(2, 6), ['--config', 'electron-builder.config.mjs', '--publish', 'never'])
    assert.ok(['--x64','--arm64'].includes(arch))
    const out = args[6].slice('-c.directories.output='.length)
    fs.mkdirSync(out)
    fs.mkdirSync(path.join(out, 'unpacked'))
    for (const ext of {'--mac':['dmg','zip'], '--win':['exe'], '--linux':['AppImage']}[platform]) {
      const name = args[7].slice('-c.artifactName='.length).replace('\${version}', '1.10.0').replace('\${ext}', ext)
      fs.writeFileSync(path.join(out,name), platform + '/' + arch)
      fs.writeFileSync(path.join(out,name + '.blockmap'), '附属文件')
    }
  `,
  )
  return workspace
}

it('六个目标共用一次依赖安装和编译，只交付八个带版本、平台和架构的安装包', async () => {
  const npm = vi.fn()
  const destination = await packageClients(output, all, { prepare, npm })
  expect(npm.mock.calls.map(([args]) => args.slice(0, 2))).toEqual([
    ['ci', '--include=dev'],
    ['run', 'build'],
  ])
  const names = await readdir(destination)
  const expected = []
  for (const arch of ['x64', 'arm64']) {
    for (const [system, ext, flag] of [
      ['macos', 'dmg', 'mac'],
      ['macos', 'zip', 'mac'],
      ['windows', 'exe', 'win'],
      ['linux', 'AppImage', 'linux'],
    ]) {
      const name = `TomBot-1.10.0-${system}-${arch}.${ext}`
      expected.push(name)
      expect(await readFile(join(destination, name), 'utf8')).toBe('--' + flag + '/--' + arch)
    }
  }
  expect(names.sort()).toEqual(expected.sort())
  expect(await readdir(output)).toHaveLength(1)
})

it('并行打包同版本互不覆盖，允许选择单一平台和架构', async () => {
  const options = { prepare, npm: vi.fn() }
  const paths = await Promise.all([
    packageClients(output, linux, options),
    packageClients(output, linux, options),
  ])
  expect(new Set(paths).size).toBe(2)
  for (const path of paths)
    expect(await readdir(path)).toEqual(['TomBot-1.10.0-linux-arm64.AppImage'])
})

it.each(['依赖失败', '打包失败', '缺少 ZIP', '空 ZIP'])(
  '整批失败保留已有成品并清理暂存：%s',
  async (reason) => {
    await writeFile(join(output, '已有版本'), '保留')
    const npm = () => {
      if (reason === '依赖失败') throw new Error(reason)
    }
    const pack = async (_project, work) => {
      if (reason === '打包失败') throw new Error(reason)
      await mkdir(work)
      await writeFile(join(work, 'TomBot-1.10.0-macos-x64.dmg'), '已完成的安装包')
      if (reason === '空 ZIP') await writeFile(join(work, 'TomBot-1.10.0-macos-x64.zip'), '')
    }
    await expect(packageClients(output, all, { prepare, npm, pack })).rejects.toThrow()
    expect(await readdir(output)).toEqual(['已有版本'])
    expect(await readFile(join(output, '已有版本'), 'utf8')).toBe('保留')
  },
)

it('非法版本、未知目标和无法签名的主机均明确报错', async () => {
  expect(() => selectPackages('linux', 'x86', 'linux')).toThrow('架构')
  expect(() => selectPackages('android', 'arm64', 'linux')).toThrow('平台')
  expect(() => selectPackages('all', 'arm64', 'win32')).toThrow('Mac')
  const npm = vi.fn()
  await expect(
    packageClients(output, linux, { prepare: (dir) => prepare(dir, '1.0.0-beta'), npm }),
  ).rejects.toThrow('版本')
  expect(npm).not.toHaveBeenCalled()
  expect(await readdir(output)).toEqual([])
})

it('无服务地址配置也可加载正式打包配置，帮助只列构建参数', () => {
  const options = { encoding: 'utf8', env: {}, timeout: 5000 }
  expect(
    execFileSync(
      process.execPath,
      [join(import.meta.dirname, '../electron-builder.config.mjs')],
      options,
    ),
  ).toBe('')
  const help = execFileSync(process.execPath, [script, '--help'], options)
  expect(help).toContain('--platform')
  expect(help).not.toContain('--origin')
})

it('应用和安装器共同使用 TomBot 的高清 PNG 图标', async () => {
  expect(config.icon).toBe('src/main/assets/tombot-icon.png')
  for (const platform of ['mac', 'win', 'linux']) {
    expect(config[platform].icon ?? config.icon).toBe(config.icon)
  }
  expect(config.nsis.installerIcon ?? config.icon).toBe(config.icon)
  const png = await readFile(join(import.meta.dirname, '..', config.icon))
  expect(png.subarray(1, 4).toString()).toBe('PNG')
  expect([png.readUInt32BE(16), png.readUInt32BE(20)]).toEqual([1024, 1024])
})

it('两个 Windows 架构共用可随 NSIS 运行的 x86 验签器，并在归档前为它签名', async () => {
  await mkdir(join(output, 'resources'))
  const binary = join(output, 'resources', 'tombot-verify.exe')
  const signIf = vi.fn()
  const packager = { projectDir: join(import.meta.dirname, '..'), signIf }
  await packWindowsVerifier({ electronPlatformName: 'linux', appOutDir: output, packager })
  expect(signIf).not.toHaveBeenCalled()
  await packWindowsVerifier({ electronPlatformName: 'win32', appOutDir: output, packager })
  expect(signIf).toHaveBeenCalledWith(binary)
  const pe = await readFile(binary)
  const header = pe.readUInt32LE(0x3c)
  expect(pe.subarray(header, header + 4).toString()).toBe('PE\0\0')
  expect(pe.readUInt16LE(header + 4)).toBe(0x014c)
}, 310_000)

it('首次创建加密的 Windows 发行身份，后续复用，错误口令和缺失文件会中止', async () => {
  const identity = join(output, 'Windows')
  vi.stubEnv('WIN_CSC_LINK', '')
  vi.stubEnv('WIN_CSC_KEY_PASSWORD', randomBytes(32).toString('hex'))
  try {
    expect(() => checkLocalWindowsIdentity(identity)).toThrow('缺少')
    await initializeWindowsIdentity(identity)
    const path = join(identity, 'publisher.pfx')
    const original = await readFile(path)
    await initializeWindowsIdentity(identity)
    vi.stubEnv('WIN_CSC_KEY_PASSWORD', randomBytes(32).toString('hex'))
    await expect(initializeWindowsIdentity(identity)).rejects.toThrow('口令')
    expect(await readFile(path)).toEqual(original)
    await rm(path)
    await expect(initializeWindowsIdentity(identity)).rejects.toThrow('恢复原 PFX')
    expect(await readdir(identity)).toEqual([])
  } finally {
    vi.unstubAllEnvs()
  }
})
