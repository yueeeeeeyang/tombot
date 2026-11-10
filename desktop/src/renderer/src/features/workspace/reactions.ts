/** 气泡上允许的表情。快捷行在前，其余只出现在“更多”里，存储不接受名单以外的符号。 */
export const REACTION_EMOJIS = [
  '👍',
  '👎',
  '❤️',
  '😂',
  '🎉',
  '😊',
  '😍',
  '🥰',
  '😘',
  '😅',
  '🤣',
  '😭',
  '😡',
  '🤔',
  '😴',
  '😎',
  '🥳',
  '👏',
  '🙏',
  '🔥',
  '✨',
  '💯',
  '👀',
  '✅',
  '❌',
  '🤝',
  '💪',
  '🌹',
  '☕',
] as const

export const QUICK_REACTION_COUNT = 5
export const QUICK_REACTION_EMOJIS = REACTION_EMOJIS.slice(0, QUICK_REACTION_COUNT)
export const MORE_REACTION_EMOJIS = REACTION_EMOJIS.slice(QUICK_REACTION_COUNT)

export type ReactionEmoji = (typeof REACTION_EMOJIS)[number]

/** 判断符号是否在允许名单内；界面只提供名单里的按钮，存储再拦一次。 */
export function isReactionEmoji(emoji: string): emoji is ReactionEmoji {
  return (REACTION_EMOJIS as readonly string[]).includes(emoji)
}
