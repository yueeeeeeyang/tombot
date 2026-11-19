import { execFile, spawn } from 'node:child_process'
import { once } from 'node:events'
import { dirname, join } from 'node:path'
import { promisify } from 'node:util'

// 所有路径和版本号经独立环境字段传入。命令文本固定，目录中的空格和引号不会变成指令。
const verifyInstaller = `
$ErrorActionPreference = 'Stop'
& $env:TOMBOT_VERIFY_EXE $env:TOMBOT_CURRENT_EXE $env:TOMBOT_INSTALLER
if ($LASTEXITCODE -ne 0) { throw '安装包的签名或发布者不匹配' }
$metadata = [Diagnostics.FileVersionInfo]::GetVersionInfo($env:TOMBOT_INSTALLER)
if ($metadata.ProductName -ne 'TomBot' -or $metadata.ProductVersion -ne $env:TOMBOT_INSTALL_VERSION) { throw '产品或版本不匹配' }
`

// 安装器必须等旧进程释放可执行文件。独立进程负责观察安装结果并启动安装目录中的客户端。
const installAndRestart = `
$ErrorActionPreference = 'Stop'
try {
  # 从最终验签到安装器退出持有只读共享句柄，防止交接期间文件被替换。
  $guard = [IO.File]::Open($env:TOMBOT_INSTALLER, 'Open', 'Read', 'Read')
  ${verifyInstaller}
  $previous = Get-Process -Id $env:TOMBOT_PREVIOUS_PID -ErrorAction SilentlyContinue
  if ($previous -and !$previous.WaitForExit(60000)) { throw '旧进程仍在运行' }
  $installer = Start-Process -FilePath $env:TOMBOT_INSTALLER -ArgumentList '/S','--updated' -PassThru
  if (!$installer.WaitForExit(600000) -or $installer.ExitCode -ne 0) { throw '安装程序失败' }
  $installed = [Diagnostics.FileVersionInfo]::GetVersionInfo($env:TOMBOT_CURRENT_EXE)
  if (('{0}.{1}.{2}' -f $installed.ProductMajorPart,$installed.ProductMinorPart,$installed.ProductBuildPart) -ne $env:TOMBOT_INSTALL_VERSION) { throw '安装后的版本不匹配' }
  if ($installed.ProductName -ne 'TomBot') { throw '安装后的产品不匹配' }
  Start-Process -FilePath $env:TOMBOT_CURRENT_EXE
} catch {
  Add-Type -AssemblyName PresentationFramework
  [void][System.Windows.MessageBox]::Show('升级未完成，请重新打开客户端后重试。', 'TomBot')
} finally {
  if ($guard) { $guard.Dispose() }
  Remove-Item -LiteralPath $env:TOMBOT_INSTALLER -Force
  [IO.Directory]::Delete([IO.Path]::GetDirectoryName($env:TOMBOT_INSTALLER))
}
`

/** 验证 Windows 签名、产品及版本。返回的交接函数启动每用户 NSIS 静默安装。 */
export async function prepareWindowsUpdate(file: string, version: string) {
  const systemRoot = process.env.SystemRoot
  if (!systemRoot) throw new Error('无法定位 Windows 系统目录。')
  const executable = join(systemRoot, 'System32', 'WindowsPowerShell', 'v1.0', 'powershell.exe')
  const options = {
    windowsHide: true,
    env: {
      ...process.env,
      TOMBOT_INSTALLER: file,
      TOMBOT_VERIFY_EXE: join(dirname(process.execPath), 'resources', 'tombot-verify.exe'),
      TOMBOT_CURRENT_EXE: process.execPath,
      TOMBOT_PREVIOUS_PID: String(process.pid),
      TOMBOT_INSTALL_VERSION: version,
    },
  }
  const args = ['-NoLogo', '-NoProfile', '-NonInteractive', '-Command']
  try {
    await promisify(execFile)(executable, [...args, verifyInstaller], {
      ...options,
      timeout: 60_000,
    })
  } catch {
    throw new Error('Windows 安装包的发布者签名、产品名称或版本校验失败。')
  }
  return async () => {
    const worker = spawn(executable, [...args, installAndRestart], {
      ...options,
      detached: true,
      stdio: 'ignore',
    })
    await once(worker, 'spawn')
    worker.unref()
  }
}
