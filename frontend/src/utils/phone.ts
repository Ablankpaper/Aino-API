export function normalizeCNPhone(raw: string): string | null {
  const trimmed = raw.trim()
  if (!trimmed) return null

  let cleaned = ''
  for (const char of trimmed) {
    if (char >= '0' && char <= '9') {
      cleaned += char
      continue
    }
    if (/\s/.test(char) || char === '-' || char === '(' || char === ')') continue
    if (char === '+' && cleaned === '') {
      cleaned = '+'
      continue
    }
    return null
  }

  let local = cleaned
  if (local.startsWith('+86')) local = local.slice(3)
  else if (local.startsWith('0086')) local = local.slice(4)
  else if (local.startsWith('+')) return null
  else if (local.startsWith('86') && local.length === 13) local = local.slice(2)

  return /^1[3-9][0-9]{9}$/.test(local) ? `+86${local}` : null
}

export function phoneRetryAfterSeconds(error: unknown): number {
  if (!error || typeof error !== 'object') return 0
  const value = (error as { metadata?: Record<string, unknown> }).metadata?.retry_after
  const parsed = Number(value)
  return Number.isInteger(parsed) && parsed > 0 ? parsed : 0
}

export function phoneDeliveryMessageKey(status: unknown): string {
  switch (String(status || '').trim().toLowerCase()) {
    case 'submitted':
      return 'auth.phoneDeliverySubmitted'
    case 'sending':
      return 'auth.phoneDeliverySending'
    case 'failed':
      return 'auth.phoneDeliveryFailed'
    default:
      return 'auth.phoneDeliveryUnknown'
  }
}
