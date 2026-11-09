import { CAT_AVATAR_IDS, type CatAvatarId } from './presets'

/** 贴纸外缘。深浅主题共用，让深色毛在深色界面上仍有轮廓。 */
export const CAT_STICKER_RIM = '#f4ead8'
export const CAT_CONTOUR = '#3c342c'

export type CatPalette = {
  fur: string
  furLight: string
  shade: string
  ear: string
  earInner: string
  earRight: string
  iris: string
  irisLight: string
  irisDeep: string
  nose: string
  muzzle: string
  whisker: string
  mouth: string
  tongue: string
  lid: string
  lidRight: string
  marking: string
  blinkDelay: string
}

/**
 * 八款毛色的唯一配色。不读取主题变量，避免深色模式把毛色反相。
 * 眨眼延迟错开，多只猫并排时不会同时闭眼。
 */
export const CAT_PALETTES: Record<CatAvatarId, CatPalette> = {
  bombay: {
    fur: '#2a2a30',
    furLight: '#45454f',
    shade: '#16161a',
    ear: '#2a2a30',
    earInner: '#6a463e',
    earRight: '#2a2a30',
    iris: '#f0a01c',
    irisLight: '#ffe08a',
    irisDeep: '#c46e08',
    nose: '#5a4448',
    muzzle: '#35353d',
    whisker: '#e4dcd2',
    mouth: '#b2a8ae',
    tongue: '#e09090',
    lid: '#2a2a30',
    lidRight: '#2a2a30',
    marking: '#1a1a1e',
    blinkDelay: '-0.4s',
  },
  'orange-tabby': {
    fur: '#e8832a',
    furLight: '#f6a24e',
    shade: '#c86518',
    ear: '#e8832a',
    earInner: '#f6c2b0',
    earRight: '#e8832a',
    iris: '#f0a020',
    irisLight: '#ffe08a',
    irisDeep: '#d07010',
    nose: '#f0999a',
    muzzle: '#f8e2c8',
    whisker: '#fffaf4',
    mouth: '#8a3040',
    tongue: '#f0918e',
    lid: '#e8832a',
    lidRight: '#e8832a',
    marking: '#c45e12',
    blinkDelay: '-1.1s',
  },
  'british-shorthair': {
    fur: '#7d8794',
    furLight: '#a3acb8',
    shade: '#5e6874',
    ear: '#7d8794',
    earInner: '#5c6570',
    earRight: '#7d8794',
    iris: '#e87820',
    irisLight: '#ffb15a',
    irisDeep: '#c45510',
    nose: '#3e4650',
    muzzle: '#8b95a1',
    whisker: '#f7f4ef',
    mouth: '#3e4650',
    tongue: '#e09090',
    lid: '#7d8794',
    lidRight: '#7d8794',
    marking: '#5e6874',
    blinkDelay: '-1.8s',
  },
  ragdoll: {
    fur: '#f3e4d4',
    furLight: '#fff7ef',
    shade: '#e4d0bc',
    ear: '#8a5a42',
    earInner: '#d09a86',
    earRight: '#8a5a42',
    iris: '#2f8cff',
    irisLight: '#b9dcff',
    irisDeep: '#1460d0',
    nose: '#f0a090',
    muzzle: '#fffaf6',
    whisker: '#fffaf6',
    mouth: '#c48b84',
    tongue: '#f0a0a0',
    lid: '#8d5e48',
    lidRight: '#8d5e48',
    marking: '#8d5e48',
    blinkDelay: '-2.5s',
  },
  siamese: {
    fur: '#f0d7b8',
    furLight: '#f8e6d0',
    shade: '#e2c4a4',
    ear: '#6b3a2a',
    earInner: '#a86450',
    earRight: '#6b3a2a',
    iris: '#3aa0ef',
    irisLight: '#c5e4ff',
    irisDeep: '#1a6ec4',
    nose: '#5c342c',
    muzzle: '#6b3a2a',
    whisker: '#f8efe6',
    mouth: '#f0d0c0',
    tongue: '#e09090',
    lid: '#6b3a2a',
    lidRight: '#6b3a2a',
    marking: '#6b3a2a',
    blinkDelay: '-3.2s',
  },
  'american-shorthair': {
    fur: '#c5c8ce',
    furLight: '#e8eaee',
    shade: '#8e949e',
    ear: '#c5c8ce',
    earInner: '#f0c2ba',
    earRight: '#c5c8ce',
    iris: '#3cba5a',
    irisLight: '#c6f5d2',
    irisDeep: '#1e8a38',
    nose: '#e09a90',
    muzzle: '#f3f0ec',
    whisker: '#f7f7f8',
    mouth: '#6a625c',
    tongue: '#e09090',
    lid: '#c5c8ce',
    lidRight: '#c5c8ce',
    marking: '#3a4150',
    blinkDelay: '-3.9s',
  },
  persian: {
    fur: '#f7f4ef',
    furLight: '#ffffff',
    shade: '#e4ddd4',
    ear: '#f7f4ef',
    earInner: '#f4c6c4',
    earRight: '#f7f4ef',
    iris: '#c6c43a',
    irisLight: '#f2f49a',
    irisDeep: '#8a9220',
    nose: '#f0a0a8',
    muzzle: '#ffffff',
    whisker: '#c8c0b8',
    mouth: '#d8b0aa',
    tongue: '#f0a0a0',
    lid: '#f7f4ef',
    lidRight: '#f7f4ef',
    marking: '#e4ddd4',
    blinkDelay: '-4.6s',
  },
  calico: {
    fur: '#f7f1e8',
    furLight: '#ffffff',
    shade: '#e6d9cc',
    ear: '#2c2c2c',
    earInner: '#f3b7b0',
    earRight: '#e8832a',
    iris: '#3cba5a',
    irisLight: '#c8f5d4',
    irisDeep: '#1e8a38',
    nose: '#f099a4',
    muzzle: '#fffaf6',
    whisker: '#f8f4f0',
    mouth: '#8a3040',
    tongue: '#f0918e',
    lid: '#2c2c2c',
    lidRight: '#e8832a',
    marking: '#e8832a',
    blinkDelay: '-5.3s',
  },
}

export function getCatPalette(preset: CatAvatarId): CatPalette {
  const palette = CAT_PALETTES[preset]
  if (!palette) throw new Error('未知的猫咪预设')
  return palette
}

export function listCatPalettes(): Array<{ id: CatAvatarId; palette: CatPalette }> {
  return CAT_AVATAR_IDS.map((id) => ({ id, palette: getCatPalette(id) }))
}
