import { computed, ref, type ComputedRef } from 'vue'

interface PhoneChallengeResult {
  challenge_id: string
  expires_in: number
  retry_after?: number
  delivery?: string
}

interface PhoneChallengeRequest {
  generation: number
  phone: string
}

export function usePhoneChallenge(normalizedPhone: ComputedRef<string | null>) {
  const challengeId = ref('')
  const challengePhone = ref('')
  const delivery = ref('')
  const expired = ref(false)
  const expiresAt = ref(0)
  const retryAt = ref(0)
  const now = ref(Date.now())
  const sendingPhones = ref(new Set<string>())
  const pendingRequests = ref(0)
  let generation = 0
  let timer: ReturnType<typeof setInterval> | null = null

  const retryRemaining = computed(() => Math.max(0, Math.ceil((retryAt.value - now.value) / 1000)))
  const currentChallenge = computed(() =>
    Boolean(challengeId.value && challengePhone.value === normalizedPhone.value && !expired.value)
  )
  const sendingCurrentPhone = computed(() => {
    const phone = normalizedPhone.value
    return phone !== null && sendingPhones.value.has(phone)
  })

  function clearTimer(): void {
    if (timer) clearInterval(timer)
    timer = null
  }

  function ensureTimer(): void {
    if (timer) return
    timer = setInterval(() => {
      now.value = Date.now()
      if (expiresAt.value > 0 && now.value >= expiresAt.value) {
        challengeId.value = ''
        expired.value = true
        expiresAt.value = 0
      }
      if (retryAt.value <= now.value && expiresAt.value === 0) clearTimer()
    }, 250)
  }

  function clearChallenge(): void {
    challengeId.value = ''
    challengePhone.value = ''
    delivery.value = ''
    expiresAt.value = 0
    retryAt.value = 0
    expired.value = false
    now.value = Date.now()
    clearTimer()
  }

  function invalidate(): void {
    generation += 1
    clearChallenge()
  }

  function beginRequest(phone: string): PhoneChallengeRequest | null {
    if (sendingPhones.value.has(phone)) return null
    generation += 1
    clearChallenge()
    pendingRequests.value += 1
    sendingPhones.value = new Set(sendingPhones.value).add(phone)
    return { generation, phone }
  }

  function isCurrent(request: PhoneChallengeRequest): boolean {
    return request.generation === generation && normalizedPhone.value === request.phone
  }

  function adopt(request: PhoneChallengeRequest, result: PhoneChallengeResult): boolean {
    if (!isCurrent(request)) return false
    const requestNow = Date.now()
    challengeId.value = result.challenge_id
    challengePhone.value = request.phone
    delivery.value = result.delivery || ''
    expiresAt.value = requestNow + Math.max(0, result.expires_in) * 1000
    retryAt.value = requestNow + Math.max(0, result.retry_after || 0) * 1000
    now.value = requestNow
    ensureTimer()
    return true
  }

  function applyRetryAfter(request: PhoneChallengeRequest, seconds: number): void {
    if (!isCurrent(request) || seconds <= 0) return
    now.value = Date.now()
    retryAt.value = now.value + seconds * 1000
    ensureTimer()
  }

  function finishRequest(request: PhoneChallengeRequest): void {
    pendingRequests.value = Math.max(0, pendingRequests.value - 1)
    if (sendingPhones.value.has(request.phone)) {
      const next = new Set(sendingPhones.value)
      next.delete(request.phone)
      sendingPhones.value = next
    }
  }

  function dispose(): void {
    generation += 1
    clearTimer()
  }

  return {
    challengeId,
    delivery,
    expired,
    retryRemaining,
    currentChallenge,
    sendingCurrentPhone,
    pendingRequests,
    beginRequest,
    isCurrent,
    adopt,
    applyRetryAfter,
    finishRequest,
    invalidate,
    dispose
  }
}
