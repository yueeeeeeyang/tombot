/** 与上一条间隔超过此时长才再显示时间；恰好等于不拆开，避免刚发出的两句被隔断。 */
export const MESSAGE_TIME_GAP_MS = 5 * 60 * 1000

const WEEKDAY_WINDOW_DAYS = 7
const WEEKDAY_LABELS = ['星期日', '星期一', '星期二', '星期三', '星期四', '星期五', '星期六']

function startOfLocalDay(date: Date): number {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate()).getTime()
}

/** 本地日历上的天数差；用四舍五入避开夏令时当天 23 或 25 小时。 */
function calendarDaysBefore(timestamp: Date, now: Date): number {
  return Math.round((startOfLocalDay(now) - startOfLocalDay(timestamp)) / 86_400_000)
}

function formatClock(date: Date): string {
  return `${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`
}

function formatFullDate(date: Date, clock: string): string {
  return `${date.getFullYear()}年${date.getMonth() + 1}月${date.getDate()}日 ${clock}`
}

/**
 * 按微信的日历口径格式化消息时间。
 * 今天只显示时刻，昨天加“昨天”，一周内用星期，更早或未来日期写出完整年月日。
 */
export function formatMessageTime(timestamp: Date, now: Date): string {
  const clock = formatClock(timestamp)
  const days = calendarDaysBefore(timestamp, now)
  if (days === 0) return clock
  if (days === 1) return `昨天 ${clock}`
  if (days > 1 && days < WEEKDAY_WINDOW_DAYS)
    return `${WEEKDAY_LABELS[timestamp.getDay()]} ${clock}`
  return formatFullDate(timestamp, clock)
}

/**
 * 决定这条消息前要不要插入时间。
 * 会话第一条一定显示；之后只有比上一条晚超过 5 分钟才显示，跨天但间隔更短也不插入。
 */
export function messageTimeSeparator(
  createdAt: string,
  previousCreatedAt: string | undefined,
  now: Date,
): string | undefined {
  if (previousCreatedAt) {
    const gap = new Date(createdAt).getTime() - new Date(previousCreatedAt).getTime()
    if (gap <= MESSAGE_TIME_GAP_MS) return undefined
  }
  return formatMessageTime(new Date(createdAt), now)
}
