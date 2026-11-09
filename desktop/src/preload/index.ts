import { contextBridge } from 'electron'

// 仅暴露布局所需的系统标识，页面没有 Node.js 或通用 IPC 权限。
contextBridge.exposeInMainWorld('desktop', Object.freeze({ platform: process.platform }))
