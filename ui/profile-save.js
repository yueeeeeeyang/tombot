// @ts-check

/** 按字段提交资料，设置请求时限且不自动重试；未知结果与业务拒绝明确告知用户。 */
export async function saveProfile(
  /** @type {string} */ url,
  /** @type {string} */ csrf,
  /** @type {{ displayName?: string, avatar?: Blob }} */ update,
) {
  const body = new FormData()
  body.set('csrf', csrf)
  if (update.displayName !== undefined) body.set('display_name', update.displayName)
  if (update.avatar !== undefined) body.set('avatar', update.avatar, 'avatar.png')
  /** @type {Response} */
  let response
  /** @type {unknown} */
  let result
  try {
    response = await fetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { Accept: 'application/json' },
      body,
      redirect: 'error',
      signal: AbortSignal.timeout(15000),
    })
    result = await response.json()
  } catch {
    throw new Error('保存结果未确认，请检查网络后重试或刷新页面。')
  }
  if (response.status === 401) throw new Error('登录已失效，请重新登录后再保存。')
  if (response.status === 403) throw new Error('页面校验已失效，请刷新后重试。')
  if (!response.ok) {
    const message =
      result &&
      typeof result === 'object' &&
      'message' in result &&
      typeof result.message === 'string'
        ? result.message
        : '保存失败，请稍后重试。'
    throw new Error(message)
  }
  if (
    !result ||
    typeof result !== 'object' ||
    !('display_name' in result) ||
    typeof result.display_name !== 'string' ||
    !result.display_name
  ) {
    throw new Error('保存结果未确认，请刷新页面查看。')
  }
  return { displayName: result.display_name }
}
