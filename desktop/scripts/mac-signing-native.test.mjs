import { execFileSync, spawnSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { copyFile, mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { expect, it } from 'vitest'
import { initializeSelfSigningIdentity, signSelfSignedApp } from './mac-self-signing.mjs'

/** 在独立钥匙串和应用中运行系统签名器，直接按已安装版本的指定要求验新版。 */
it.skipIf(process.platform !== 'darwin')(
  '原发布者的新版通过系统校验，同名的其他发布者及篡改的包被拒绝',
  async () => {
    const directory = await mkdtemp(join(tmpdir(), 'tombot-signed-update-test-'))
    const keychains = []
    const settings = { encoding: 'utf8', stdio: 'pipe', timeout: 30000 }
    try {
      for (const name of ['原发布者', '另一发布者']) {
        const path = join(directory, name + '.keychain-db')
        const password = randomBytes(24).toString('hex')
        // 只向失败信息写系统状态码，不输出包含临时钥匙串口令的命令。
        const result = spawnSync(
          '/usr/bin/security',
          ['create-keychain', '-p', password, path],
          settings,
        )
        if (result.status !== 0) throw new Error('创建独立测试钥匙串失败，状态 ' + result.status)
        keychains.push(path)
        const first = await initializeSelfSigningIdentity(path)
        expect(await initializeSelfSigningIdentity(path)).toBe(first)
      }
      async function signedBundle(version, keychain, folder) {
        const app = join(directory, folder, 'TomBot.app')
        const contents = join(app, 'Contents')
        await mkdir(join(contents, 'MacOS'), { recursive: true })
        await copyFile('/usr/bin/true', join(contents, 'MacOS', 'TomBot'))
        await writeFile(
          join(contents, 'Info.plist'),
          `<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.tombot.desktop</string><key>CFBundleName</key><string>TomBot</string><key>CFBundleExecutable</key><string>TomBot</string><key>CFBundlePackageType</key><string>APPL</string><key>CFBundleShortVersionString</key><string>${version}</string></dict></plist>`,
        )
        await signSelfSignedApp(
          { app, platform: 'darwin', keychain, optionsForFile: () => ({}) },
          { codeSigningInfo: { value: Promise.resolve({ keychainFile: keychain }) } },
        )
        return app
      }
      const installed = await signedBundle('1.0.0', keychains[0], 'installed')
      const update = await signedBundle('1.0.1', keychains[0], 'update')
      const foreign = await signedBundle('1.0.1', keychains[1], 'foreign')
      const shown = spawnSync('/usr/bin/codesign', ['-d', '-r-', installed], settings)
      expect(shown.status).toBe(0)
      const requirement = /^designated => (.*)$/m.exec(shown.stdout + shown.stderr)?.[1]
      expect(requirement).toContain('com.tombot.desktop')
      const verify = (path) =>
        spawnSync(
          '/usr/bin/codesign',
          ['--verify', '--strict', '--deep', '--all-architectures', '-R=' + requirement, path],
          settings,
        ).status
      expect(verify(update)).toBe(0)
      // codesign 用 3 区分“签名完好但发布者不匹配”和损坏的签名。
      expect(verify(foreign)).toBe(3)
      await writeFile(join(update, 'Contents', 'Info.plist'), '已被修改')
      expect(verify(update)).not.toBe(0)
    } finally {
      try {
        for (const path of keychains)
          execFileSync('/usr/bin/security', ['delete-keychain', path], settings)
      } finally {
        await rm(directory, { recursive: true, force: true })
      }
    }
  },
  60_000,
)
