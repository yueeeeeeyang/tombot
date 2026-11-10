import { CAT_AVATAR_IDS, type CatAvatarId } from './presets'
import type { EyeFrame } from './blink'
import type { EarFrame } from './ears'

const frameModules = import.meta.glob('../../assets/cats/frames/*.png', {
  eager: true,
  import: 'default',
}) as Record<string, string>

/** 运行时只用 256 像素帧。22/34/56 即使按 3 倍屏也落在 256 以内，不再打包 1254 原图。 */
export const CAT_FRAME_SIZE = 256

const FRAME_SUFFIXES = [
  'eyes-half-overlay',
  'eyes-closed-overlay',
  'ears-perk-half',
  'ears-perk',
  'ear-left-twitch',
  'ear-right-twitch',
] as const

function frameFile(preset: CatAvatarId, suffix?: (typeof FRAME_SUFFIXES)[number]): string {
  return suffix ? `${preset}-${suffix}.png` : `${preset}.png`
}

function requireFrame(fileName: string): string {
  const match = Object.entries(frameModules).find(([path]) => path.endsWith(`/${fileName}`))
  if (!match || typeof match[1] !== 'string' || match[1].length === 0) {
    throw new Error(`缺少猫咪帧 ${fileName}`)
  }
  return match[1]
}

const sources = new Map<string, string>()
for (const preset of CAT_AVATAR_IDS) {
  sources.set(frameFile(preset), requireFrame(frameFile(preset)))
  for (const suffix of FRAME_SUFFIXES) {
    sources.set(frameFile(preset, suffix), requireFrame(frameFile(preset, suffix)))
  }
}

export function listCatFrameSources(): string[] {
  return [...sources.values()]
}

export function catPortraitSource(preset: CatAvatarId, ear: EarFrame): string {
  if (!sources.has(frameFile(preset))) throw new Error('未知的猫咪预设')
  const suffix =
    ear === 'perk-half'
      ? 'ears-perk-half'
      : ear === 'perk'
        ? 'ears-perk'
        : ear === 'twitch-left'
          ? 'ear-left-twitch'
          : ear === 'twitch-right'
            ? 'ear-right-twitch'
            : undefined
  const source = sources.get(frameFile(preset, suffix))
  if (!source) throw new Error(`缺少猫咪帧 ${frameFile(preset, suffix)}`)
  return source
}

/** 睁眼不叠图层。半闭和闭合只盖眼部，底下可以是静息或耳朵帧。 */
export function catEyeOverlaySource(preset: CatAvatarId, eye: EyeFrame): string | null {
  if (eye === 'open') return null
  const suffix = eye === 'half' ? 'eyes-half-overlay' : 'eyes-closed-overlay'
  const source = sources.get(frameFile(preset, suffix))
  if (!source) throw new Error(`缺少猫咪帧 ${frameFile(preset, suffix)}`)
  return source
}

export function getCatAvatarImage(preset: CatAvatarId): string {
  return catPortraitSource(preset, 'rest')
}
