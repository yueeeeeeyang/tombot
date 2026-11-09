import bombay from '../../assets/cats/bombay.png'
import orangeTabby from '../../assets/cats/orange-tabby.png'
import britishShorthair from '../../assets/cats/british-shorthair.png'
import ragdoll from '../../assets/cats/ragdoll.png'
import siamese from '../../assets/cats/siamese.png'
import americanShorthair from '../../assets/cats/american-shorthair.png'
import persian from '../../assets/cats/persian.png'
import calico from '../../assets/cats/calico.png'
import type { CatAvatarId } from './presets'

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
