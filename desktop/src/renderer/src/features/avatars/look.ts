export type LookBounds = {
  left: number
  top: number
  width: number
  height: number
}

export type LookOffset = {
  x: number
  y: number
}

/**
 * 把指针换算成相对头像中心的视线，范围 [-1, 1]。
 * 越界会被夹住，这样眼睛不会滑出眼眶；无效尺寸必须失败，不能假装看向正中。
 */
export function resolveLookOffset(
  clientX: number,
  clientY: number,
  bounds: LookBounds,
): LookOffset {
  const values = [clientX, clientY, bounds?.left, bounds?.top, bounds?.width, bounds?.height]
  if (values.some((value) => typeof value !== 'number' || !Number.isFinite(value))) {
    throw new Error('指针位置无效')
  }
  if (bounds.width <= 0 || bounds.height <= 0) {
    throw new Error('头像尺寸无效')
  }
  const x = (clientX - (bounds.left + bounds.width / 2)) / (bounds.width / 2)
  const y = (clientY - (bounds.top + bounds.height / 2)) / (bounds.height / 2)
  return { x: clampUnit(x), y: clampUnit(y) }
}

/** 量化到百分位，避免指针微抖让同一只猫连续重绘。 */
export function quantizeLook(value: number): number {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    throw new Error('视线偏移无效')
  }
  return Math.round(clampUnit(value) * 100) / 100
}

function clampUnit(value: number): number {
  return Math.min(1, Math.max(-1, value))
}
