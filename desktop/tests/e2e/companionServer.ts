import { execFile, spawn, type ChildProcess } from 'node:child_process'
import { createServer } from 'node:net'
import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { promisify } from 'node:util'

const exec = promisify(execFile)
const serverRoot = join(import.meta.dirname, '../../../server')

export interface Companion {
  url: string
  setupCode: string
  stop: () => Promise<void>
}

function goBinary(): string {
  if (process.env.GO_BIN && existsSync(process.env.GO_BIN)) return process.env.GO_BIN
  if (existsSync('/tmp/go/go/bin/go')) return '/tmp/go/go/bin/go'
  return 'go'
}

/** 构建一次桌面配套服务。模块已在本机缓存时，重复构建只做链接。 */
export async function buildCompanion(): Promise<string> {
  const binary = join(serverRoot, 'bin/tomd')
  await exec(goBinary(), ['build', '-o', binary, './cmd/tomd'], {
    cwd: serverRoot,
    env: { ...process.env, PATH: `/tmp/go/go/bin:${process.env.PATH ?? ''}` },
  })
  return binary
}

const fixtureSteps = [
  { id: 'detect_os', title: '检测操作系统', phase: 'detecting' },
  { id: 'prepare_dirs', title: '准备数据与工作目录', phase: 'preparing' },
  { id: 'install_fonts', title: '安装中文字体', phase: 'preparing' },
  { id: 'ensure_browser', title: '准备托管浏览器', phase: 'preparing' },
]

/** 为单个测试启动独立数据目录。夹具只模拟引导进度，产品默认仍会安装字体和浏览器。 */
export async function startCompanion(
  binary: string,
  options?: { failFirst?: boolean },
): Promise<Companion> {
  const directory = await mkdtemp(join(tmpdir(), 'tombot-server-'))
  const fixturePath = join(directory, 'bootstrap.json')
  const steps = fixtureSteps.map((step) =>
    options?.failFirst && step.id === 'ensure_browser'
      ? { ...step, fail_first: true, detail: '夹具没有浏览器，测试不会下载 Chrome' }
      : step,
  )
  await writeFile(fixturePath, JSON.stringify({ steps }))
  const port = await listenPort()
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    TOM_BOOTSTRAP_FIXTURE: fixturePath,
    TOM_ADDR: `127.0.0.1:${port}`,
    TOM_DATA_DIR: join(directory, 'data'),
    TOM_WORKSPACE_DIR: join(directory, 'workspace'),
  }
  delete env.TOM_DESKTOP_COMPANION
  const child = spawn(binary, [], {
    env,
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  let stdout = ''
  let stderr = ''
  child.stdout?.setEncoding('utf8')
  child.stderr?.setEncoding('utf8')
  child.stdout?.on('data', (chunk: string) => {
    stdout += chunk
  })
  child.stderr?.on('data', (chunk: string) => {
    stderr += chunk
  })
  const url = `http://127.0.0.1:${port}`
  try {
    const setupCode = await waitForSetupCode(child, () => stdout)
    await waitForSession(url, child, () => stderr)
    await waitForBootstrap(url, child, () => stderr, options?.failFirst ? 'failed' : 'ready')
    return {
      url,
      setupCode,
      stop: async () => {
        await stopProcess(child)
        await rm(directory, { recursive: true, force: true })
      },
    }
  } catch (error) {
    await stopProcess(child)
    await rm(directory, { recursive: true, force: true })
    const reason = error instanceof Error ? error.message : '服务没有启动'
    throw new Error(`${reason}。服务错误输出：${stderr.slice(-500) || '无'}`, { cause: error })
  }
}

function listenPort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = createServer()
    server.once('error', reject)
    server.listen(0, '127.0.0.1', () => {
      const address = server.address()
      if (!address || typeof address === 'string') {
        server.close()
        reject(new Error('没有分配到端口'))
        return
      }
      server.close((error) => (error ? reject(error) : resolve(address.port)))
    })
  })
}

async function waitForSetupCode(child: ChildProcess, readStdout: () => string): Promise<string> {
  const started = Date.now()
  while (Date.now() - started < 15_000) {
    if (child.exitCode !== null) throw new Error(`服务提前退出，代码 ${child.exitCode}`)
    const matched = /一次性初始化码[^：]*：(\S+)/.exec(readStdout())
    if (matched?.[1]) return matched[1]
    await delay(50)
  }
  throw new Error('等待初始化码超时')
}

async function waitForSession(
  url: string,
  child: ChildProcess,
  readStderr: () => string,
): Promise<void> {
  const started = Date.now()
  let lastError = '尚未连接'
  while (Date.now() - started < 15_000) {
    if (child.exitCode !== null) {
      throw new Error(`服务提前退出，代码 ${child.exitCode}，${readStderr().slice(-300)}`)
    }
    try {
      const response = await fetch(`${url}/api/v1/auth/status`, {
        signal: AbortSignal.timeout(1_000),
      })
      if (response.ok) return
      lastError = `HTTP ${response.status}`
    } catch (error) {
      lastError = error instanceof Error ? error.message : '连接失败'
    }
    await delay(50)
  }
  throw new Error(`等待服务就绪超时：${lastError}`)
}

async function waitForBootstrap(
  url: string,
  child: ChildProcess,
  readStderr: () => string,
  phase: string,
): Promise<void> {
  const started = Date.now()
  let last = '尚未返回引导状态'
  while (Date.now() - started < 15_000) {
    if (child.exitCode !== null) {
      throw new Error(`服务提前退出，代码 ${child.exitCode}，${readStderr().slice(-300)}`)
    }
    try {
      const response = await fetch(`${url}/api/v1/bootstrap/status`, {
        signal: AbortSignal.timeout(1_000),
      })
      if (response.ok) {
        const body = (await response.json()) as { phase?: string }
        if (body.phase === phase) return
        last = body.phase ?? '没有阶段'
      } else {
        last = `HTTP ${response.status}`
      }
    } catch (error) {
      last = error instanceof Error ? error.message : '连接失败'
    }
    await delay(50)
  }
  throw new Error(`等待引导进入${phase}超时：${last}`)
}

async function stopProcess(child: ChildProcess): Promise<void> {
  if (child.exitCode !== null) return
  child.kill('SIGTERM')
  const exited = new Promise<void>((resolve) => child.once('exit', () => resolve()))
  const timer = delay(2_000).then(() => {
    if (child.exitCode === null) child.kill('SIGKILL')
  })
  await Promise.race([exited, timer])
  await exited
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}
