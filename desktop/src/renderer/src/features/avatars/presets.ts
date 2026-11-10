export const CAT_AVATARS = [
  { id: 'bombay', name: '孟买猫' },
  { id: 'orange-tabby', name: '橘猫' },
  { id: 'british-shorthair', name: '英短蓝猫' },
  { id: 'ragdoll', name: '布偶猫' },
  { id: 'siamese', name: '暹罗猫' },
  { id: 'american-shorthair', name: '美短银虎斑' },
  { id: 'persian', name: '波斯猫' },
  { id: 'calico', name: '三花猫' },
] as const

export type CatAvatarId = (typeof CAT_AVATARS)[number]['id']
export const CAT_AVATAR_IDS = CAT_AVATARS.map((preset) => preset.id)

export const DEFAULT_AGENT_AVATARS = {
  assistant: 'bombay',
  developer: 'orange-tabby',
  producer: 'british-shorthair',
  artist: 'calico',
  audio: 'siamese',
  coding: 'american-shorthair',
  news: 'ragdoll',
  organizer: 'persian',
} as const satisfies Record<string, CatAvatarId>
