import { spawnSync } from 'node:child_process'
import { cp, lstat, mkdir, mkdtemp, readFile, rename, rm } from 'node:fs/promises'
import { basename, dirname, join, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'
import { parseArgs } from 'node:util'
import { checkLocalMacIdentity } from './mac-release-signing.mjs'
import { checkLocalWindowsIdentity } from './windows-signing.mjs'

const desktop = resolve(import.meta.dirname, '..')
const targets = new Map([
  ['macos', { flag: '--mac', extensions: ['dmg', 'zip'] }],
  ['windows', { flag: '--win', extensions: ['exe'] }],
  ['linux', { flag: '--linux', extensions: ['AppImage'] }],
])
const architectures = ['x64', 'arm64']
const help = `从仓库根目录执行 npm --prefix desktop run dist:all -- [选项]

  --platform <all|macos|windows|linux>  默认 all，包含 macOS 时需要在 Mac 上执行
  --arch <all|x64|arm64>                默认 all
  --output <目录>                       成品的父目录，默认 desktop/release
  --help                                查看用法

版本取自 desktop/package.json。首次发行免费 macOS 或 Windows 版请先运行 npm --prefix desktop run signing:init。`

/** 根据命令选项生成平台和架构组合，无法构建请求的目标时提前报错。 */
export function selectPackages(platform, arch, host) {
  const systems = platform === 'all' ? [...targets.keys()] : [platform]
  const selected = arch === 'all' ? architectures : [arch]
  if (!systems.every((system) => targets.has(system))) throw new Error('不支持的目标平台。')
  if (!selected.every((value) => architectures.includes(value)))
    throw new Error('不支持的处理器架构。')
  if (systems.includes('macos') && host !== 'darwin') {
    throw new Error('macOS 发行包需要在 Mac 上签名。可用 --platform 选择其他平台。')
  }
  return systems.flatMap((system) => selected.map((architecture) => ({ system, architecture })))
}

/** 仅暂存桌面构建输入与跨目录依赖，保证六个目标使用同一次编译的客户端。 */
async function stageProject(workspace) {
  const project = join(workspace, 'desktop')
  await mkdir(project)
  for (const name of [
    'package.json',
    'package-lock.json',
    'src',
    'tests',
    'tsconfig.json',
    'electron.vite.config.ts',
    'electron-builder.config.mjs',
    'scripts/mac-release-signing.mjs',
    'scripts/mac-self-signing.mjs',
    'scripts/windows-signing.mjs',
    'windows-verifier',
  ]) {
    await cp(join(desktop, name), join(project, name), { recursive: true })
  }
  const root = dirname(desktop)
  for (const name of ['ui', 'server/internal/execution/reaction_emojis.json']) {
    await cp(join(root, name), join(workspace, name), { recursive: true })
  }
  return project
}

/** 在工作副本内运行有超时的构建步骤，子进程异常退出立即终止整批打包。 */
function run(command, args, cwd, label, options = {}) {
  console.info(label)
  const result = spawnSync(command, args, {
    cwd,
    stdio: 'inherit',
    timeout: 60 * 60 * 1000,
    ...options,
  })
  if (result.error || result.status !== 0) {
    throw new Error(`${label}失败（${result.error?.code ?? result.signal ?? result.status}）。`, {
      cause: result.error,
    })
  }
}

/** 通过 npm 进入时沿用同一 npm 程序。直接调用脚本时，Windows 的 npm.cmd 只接收固定参数。 */
function runNpm(args, project, label) {
  if (process.env.npm_execpath) {
    run(process.execPath, [process.env.npm_execpath, '--prefix', project, ...args], project, label)
  } else {
    run('npm', args, project, label, { shell: process.platform === 'win32' })
  }
}

/** 逐架构调用锁定的打包器，使每个 NSIS 安装包只包含管理平台所选的架构。 */
function buildPackage(project, work, { system, architecture }) {
  const { flag } = targets.get(system)
  run(
    process.execPath,
    [
      join(project, 'node_modules/electron-builder/cli.js'),
      flag,
      '--' + architecture,
      '--config',
      'electron-builder.config.mjs',
      '--publish',
      'never',
      '-c.directories.output=' + work,
      '-c.artifactName=TomBot-${version}-' + system + '-' + architecture + '.${ext}',
    ],
    project,
    `构建 ${system} / ${architecture}`,
  )
}

/** 核对所选目标的成品后整批原子交付；失败只清理本次创建的暂存目录。 */
export async function packageClients(
  output,
  selection,
  { prepare = stageProject, npm = runNpm, pack = buildPackage } = {},
) {
  await mkdir(output, { recursive: true })
  const workspace = await mkdtemp(join(output, '.build-'))
  try {
    const project = await prepare(workspace)
    const { version } = JSON.parse(await readFile(join(project, 'package.json'), 'utf8'))
    if (
      typeof version !== 'string' ||
      !/^(?:0|[1-9]\d{0,5})(?:\.(?:0|[1-9]\d{0,5})){2}$/.test(version)
    ) {
      throw new Error('客户端发行版本必须是管理平台支持的三段式版本号。')
    }
    await npm(
      ['ci', '--include=dev', '--prefer-offline', '--no-audit', '--no-fund'],
      project,
      '安装锁定依赖',
    )
    await npm(['run', 'build'], project, '编译桌面客户端 ' + version)
    const products = join(workspace, 'packages')
    const work = join(workspace, 'builder')
    await mkdir(products)
    for (const target of selection) {
      await pack(project, work, target)
      for (const ext of targets.get(target.system).extensions) {
        const name = `TomBot-${version}-${target.system}-${target.architecture}.${ext}`
        const file = join(work, name)
        const stat = await lstat(file)
        if (!stat.isFile() || !stat.size) throw new Error('打包器未生成有效的 ' + name)
        await rename(file, join(products, name))
      }
      await rm(work, { recursive: true })
    }
    const destination = join(
      output,
      'TomBot-' + version + '-' + basename(workspace).slice('.build-'.length),
    )
    await rename(products, destination)
    return destination
  } finally {
    await rm(workspace, { recursive: true, force: true, maxRetries: 3 })
  }
}

/** 解析打包目标与输出目录，参数错误时在安装依赖前退出。 */
async function main() {
  const { values } = parseArgs({
    options: {
      platform: { type: 'string', default: 'all' },
      arch: { type: 'string', default: 'all' },
      output: { type: 'string' },
      help: { type: 'boolean', short: 'h' },
    },
  })
  if (values.help) {
    console.info(help)
    return
  }
  const selection = selectPackages(values.platform, values.arch, process.platform)
  // 通过 CSC_LINK 注入的身份要等打包器导入后核验，本机钥匙串可在安装依赖前检查。
  if (selection.some(({ system }) => system === 'macos')) checkLocalMacIdentity()
  if (selection.some(({ system }) => system === 'windows')) checkLocalWindowsIdentity()
  const output = values.output
    ? resolve(process.env.INIT_CWD ?? process.cwd(), values.output)
    : join(desktop, 'release')
  const destination = await packageClients(output, selection)
  console.info('客户端安装包已生成：' + destination)
}
if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  main().catch((error) => {
    console.error('客户端打包失败：' + error.message)
    process.exitCode = 1
  })
}
