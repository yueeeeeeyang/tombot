import { initializeSelfSigningIdentity } from './mac-self-signing.mjs'
import { macSigningMode } from './mac-release-signing.mjs'
import { initializeWindowsIdentity } from './windows-signing.mjs'

try {
  if (process.platform === 'darwin' && macSigningMode() === 'self-signed') {
    const identity = await initializeSelfSigningIdentity()
    console.info('macOS 自签名身份已就绪，证书指纹 ' + identity)
  }
  await initializeWindowsIdentity()
} catch (error) {
  console.error('初始化发行身份失败：' + error.message)
  process.exitCode = 1
}
