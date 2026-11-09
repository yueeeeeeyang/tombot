import { app, BrowserWindow, dialog, Menu, session } from 'electron'
import { join } from 'node:path'

/** 创建隔离的本地客户端窗口；加载失败时报告错误并退出，避免留下空白窗口。 */
async function createWindow(): Promise<void> {
  const window = new BrowserWindow({
    width: 1073,
    height: 780,
    minWidth: 860,
    minHeight: 640,
    show: false,
    title: 'TomBot',
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

  window.webContents.setWindowOpenHandler(() => ({ action: 'deny' }))
  window.webContents.on('will-navigate', (event) => event.preventDefault())
  window.webContents.on('will-prevent-unload', (event) => {
    const choice = dialog.showMessageBoxSync(window, {
      type: 'warning',
      title: '还有未保存的内容',
      message: '本地草稿尚未成功保存，退出将丢失本次未保存的修改。',
      buttons: ['继续编辑', '放弃修改并退出'],
      defaultId: 0,
      cancelId: 0,
    })
    if (choice === 1) event.preventDefault()
  })
  window.once('ready-to-show', () => window.show())
  window.webContents.on('render-process-gone', (_event, details) => {
    console.error(`客户端渲染进程退出，窗口 ID=${window.id}，原因=${details.reason}`)
    dialog.showErrorBox('客户端意外退出', '请重新打开 TomBot。已保存的本地草稿会保留。')
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
      app.setName('TomBot')
      session.defaultSession.setPermissionRequestHandler((_contents, _permission, callback) => {
        callback(false)
      })
      session.defaultSession.setPermissionCheckHandler(() => false)
      Menu.setApplicationMenu(
        Menu.buildFromTemplate([
          ...(process.platform === 'darwin'
            ? [
                {
                  label: 'TomBot',
                  submenu: [
                    { role: 'about' as const, label: '关于 TomBot' },
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
      await createWindow()
      app.on('activate', () => {
        if (BrowserWindow.getAllWindows().length === 0) void createWindow()
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
