import {
  app,
  autoUpdater,
  BrowserWindow,
  dialog,
  Menu,
  nativeImage,
  type NativeImage,
  type WebContents,
} from 'electron'
import { copyFileSync, existsSync, mkdirSync } from 'node:fs'
import { join } from 'node:path'
import iconPath from './assets/tombot-icon.png?asset'
import { registerDesktopIpc } from './desktopServer'

function usesCustomUserDataDir(): boolean {
  return process.argv.some((arg) => arg === '--user-data-dir' || arg.startsWith('--user-data-dir='))
}

/**
 * 单实例锁会立刻解析 userData。名称若留到 ready 之后才改，锁用的是包名目录，
 * 会话文件却写进 TomBot，下次从另一处读，地址就像没保存。
 */
function pinUserDataDirectory(): void {
  app.setName('TomBot')
  if (usesCustomUserDataDir()) return
  const root = join(app.getPath('appData'), 'TomBot')
  mkdirSync(root, { recursive: true })
  const target = join(root, 'desktop-session.json')
  if (!existsSync(target)) {
    for (const legacy of ['tombot-desktop', 'Electron']) {
      const file = join(app.getPath('appData'), legacy, 'desktop-session.json')
      if (!existsSync(file)) continue
      copyFileSync(file, target)
      break
    }
  }
  app.setPath('userData', root)
}

/** 图标损坏时仍允许访问聊天与登录界面，并记录默认图标的回退原因。 */
function loadApplicationIcon(): NativeImage | undefined {
  const icon = nativeImage.createFromPath(iconPath)
  if (!icon.isEmpty()) return icon
  console.warn('无法读取客户端图标，将使用系统默认图标。应用=TomBot')
  return undefined
}

/** 创建隔离的本地客户端窗口；加载失败时报告错误并退出，避免留下空白窗口。 */
async function createWindow(icon: NativeImage | undefined): Promise<void> {
  const window = new BrowserWindow({
    width: 1073,
    height: 780,
    minWidth: 860,
    minHeight: 640,
    show: false,
    title: 'TomBot',
    icon,
    backgroundColor: '#fcfcfc',
    titleBarStyle: process.platform === 'darwin' ? 'hiddenInset' : 'default',
    trafficLightPosition: { x: 16, y: 16 },
    webPreferences: {
      preload: join(import.meta.dirname, '../preload/index.cjs'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      webSecurity: true,
    },
  })

  const contents = window.webContents
  // 只允许本地客户端主框架响应用户手势进入全屏，其他网页权限仍由主进程拒绝。
  function allowsFullscreen(
    requester: WebContents | null,
    permission: string,
    details: { isMainFrame: boolean; requestingUrl?: string },
  ): boolean {
    return (
      permission === 'fullscreen' &&
      requester === contents &&
      !contents.isDestroyed() &&
      details.isMainFrame &&
      details.requestingUrl === contents.getURL()
    )
  }
  contents.session.setPermissionRequestHandler((requester, permission, callback, details) => {
    callback(allowsFullscreen(requester, permission, details))
  })
  contents.session.setPermissionCheckHandler((requester, permission, _origin, details) =>
    allowsFullscreen(requester, permission, details),
  )
  window.webContents.setWindowOpenHandler(() => ({ action: 'deny' }))
  window.webContents.on('will-navigate', (event) => event.preventDefault())
  window.webContents.on('will-prevent-unload', (event) => {
    const choice = dialog.showMessageBoxSync(window, {
      type: 'warning',
      title: '还有未保存的内容',
      message: '修改尚未成功保存到服务，退出将丢失本次未保存的内容。',
      buttons: ['继续编辑', '放弃修改并退出'],
      defaultId: 0,
      cancelId: 0,
    })
    if (choice === 1) event.preventDefault()
  })
  window.once('ready-to-show', () => window.show())
  // Electron 的网页焦点不总随原生窗口切换，资料刷新以原生窗口激活为准。
  window.on('focus', () => window.webContents.send('desktop:window-focus'))
  window.webContents.on('render-process-gone', (_event, details) => {
    console.error(`客户端渲染进程退出，窗口 ID=${window.id}，原因=${details.reason}`)
    dialog.showErrorBox('客户端意外退出', '请重新打开 TomBot。已保存到服务的内容会保留。')
    app.quit()
  })

  try {
    const rendererUrl = process.env.ELECTRON_RENDERER_URL
    if (!app.isPackaged && rendererUrl) {
      const url = new URL(rendererUrl)
      if (!['localhost', '127.0.0.1'].includes(url.hostname) || url.protocol !== 'http:') {
        throw new Error('开发页面必须来自本机 HTTP 服务')
      }
      await window.loadURL(rendererUrl)
    } else {
      await window.loadFile(join(import.meta.dirname, '../renderer/index.html'))
    }
  } catch {
    console.error(`客户端页面加载失败，窗口 ID=${window.id}`)
    dialog.showErrorBox('无法打开客户端', '页面加载失败，请检查构建是否完成后重新启动。')
    app.quit()
  }
}

// 安装包启动器也须遵守客户端的渲染沙盒设置。
if (process.platform === 'linux' && app.isPackaged) app.enableSandbox()
pinUserDataDirectory()
// 单实例避免两个窗口同时覆盖本地工作空间。
const hasInstanceLock = app.requestSingleInstanceLock()
if (!hasInstanceLock) {
  app.quit()
} else {
  app.on('second-instance', () => {
    const window = BrowserWindow.getAllWindows()[0]
    if (window?.isMinimized()) window.restore()
    window?.show()
    window?.focus()
  })

  app
    .whenReady()
    .then(async () => {
      if (process.platform === 'darwin') {
        // 超时关闭本机服务之后，系统更新器仍可能异步报告上一次请求的错误。
        autoUpdater.on('error', () => console.warn('系统更新器报告安装异常，应用=TomBot'))
      }
      const icon = loadApplicationIcon()
      if (icon) app.dock?.setIcon(icon)
      registerDesktopIpc(async (install) => {
        const window = BrowserWindow.getAllWindows()[0]
        if (!window || window.isDestroyed()) throw new Error('升级窗口已关闭，请重新检查更新。')
        // 冻结交互后复用页面已有的离开检查，避免在下载安装期间产生的新修改随重启丢失。
        window.setEnabled(false)
        try {
          const canLeave = await window.webContents.executeJavaScript(
            "window.dispatchEvent(new Event('beforeunload', {cancelable: true}))",
          )
          if (!canLeave) throw new Error('有未保存的修改，请保存后再点立即升级。')
          await install()
        } finally {
          if (!window.isDestroyed()) window.setEnabled(true)
        }
      })
      Menu.setApplicationMenu(
        Menu.buildFromTemplate([
          ...(process.platform === 'darwin'
            ? [
                {
                  label: 'TomBot',
                  submenu: [
                    {
                      label: '检查更新…',
                      click: () =>
                        BrowserWindow.getAllWindows()[0]?.webContents.send(
                          'desktop:update-request',
                        ),
                    },
                    { role: 'quit' as const, label: '退出 TomBot' },
                  ],
                },
              ]
            : []),
          {
            label: '编辑',
            submenu: [
              { role: 'undo', label: '撤销' },
              { role: 'redo', label: '重做' },
              { type: 'separator' },
              { role: 'cut', label: '剪切' },
              { role: 'copy', label: '复制' },
              { role: 'paste', label: '粘贴' },
              { role: 'selectAll', label: '全选' },
            ],
          },
          {
            label: '窗口',
            submenu: [
              { role: 'minimize', label: '最小化' },
              { role: 'togglefullscreen', label: '切换全屏' },
            ],
          },
        ]),
      )
      await createWindow(icon)
      app.on('activate', () => {
        if (BrowserWindow.getAllWindows().length === 0) void createWindow(icon)
      })
    })
    .catch(() => {
      console.error('客户端初始化失败，应用=TomBot')
      dialog.showErrorBox('无法启动 TomBot', '应用初始化失败，请重新启动。')
      app.quit()
    })
  app.on('window-all-closed', () => {
    if (process.platform !== 'darwin') app.quit()
  })
}
