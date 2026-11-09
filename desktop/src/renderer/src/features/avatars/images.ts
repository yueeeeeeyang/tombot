import americanShorthair from '../../assets/cats/american-shorthair.png'
import bombay from '../../assets/cats/bombay.png'
import britishShorthair from '../../assets/cats/british-shorthair.png'
import calico from '../../assets/cats/calico.png'
import orangeTabby from '../../assets/cats/orange-tabby.png'
import persian from '../../assets/cats/persian.png'
import ragdoll from '../../assets/cats/ragdoll.png'
import siamese from '../../assets/cats/siamese.png'
import type { CatAvatarId } from './presets'

/** 原图随应用打包。八款地址必须各不相同，不能在运行时再画一只替代。 */
export const CAT_AVATAR_IMAGES: Record<CatAvatarId, string> = {
  bombay,
  'orange-tabby': orangeTabby,
  'british-shorthair': britishShorthair,
  ragdoll,
  siamese,
  'american-shorthair': americanShorthair,
  persian,
  calico,
}

export function getCatAvatarImage(preset: CatAvatarId): string {
  const source = CAT_AVATAR_IMAGES[preset]
  if (!source) throw new Error('未知的猫咪预设')
  return source
}
