import { macSigningMode, verifyMacReleaseSigning } from './scripts/mac-release-signing.mjs'
import { signSelfSignedApp, verifySelfSignedApp } from './scripts/mac-self-signing.mjs'
import {
  configureWindowsSigning,
  packWindowsVerifier,
  signWindowsFile,
} from './scripts/windows-signing.mjs'

const selfSigned = macSigningMode() === 'self-signed'

export default {
  appId: 'com.tombot.desktop',
  productName: 'TomBot',
  icon: 'src/main/assets/tombot-icon.png',
  toolsets: { appimage: '1.0.3', winCodeSign: '1.1.0', nsis: '1.2.1' },
  directories: { output: 'release' },
  files: ['out/**/*', 'package.json'],
  async beforePack(context) {
    if (context.electronPlatformName === 'win32') configureWindowsSigning()
    else if (!selfSigned) await verifyMacReleaseSigning(context)
  },
  afterPack: packWindowsVerifier,
  ...(selfSigned ? { afterSign: verifySelfSignedApp } : {}),
  mac: {
    target: ['zip', 'dmg'],
    type: 'distribution',
    forceCodeSigning: true,
    // 自定义签名器按证书指纹签名并验签，避开打包器内置的 Apple 证书自动选择。
    ...(selfSigned ? { identity: '-', sign: signSelfSignedApp, notarize: false } : {}),
  },
  win: {
    target: 'nsis',
    forceCodeSigning: true,
    signtoolOptions: {
      signingHashAlgorithms: ['sha256'],
      ...(!process.env.WIN_CSC_LINK || process.env.TOMBOT_WINDOWS_SIGNING === 'self-signed'
        ? { sign: signWindowsFile }
        : {}),
    },
  },
  nsis: { oneClick: true, perMachine: false },
  linux: { target: 'AppImage', category: 'Network', icon: 'src/main/assets/tombot-icon.png' },
}
