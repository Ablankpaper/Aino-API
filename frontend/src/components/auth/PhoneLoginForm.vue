<template>
  <div class="space-y-5">
    <div>
      <label for="phone-login-phone" class="input-label">{{ t('auth.phoneLabel') }}</label>
      <div class="relative">
        <div class="pointer-events-none absolute inset-y-0 left-0 flex items-center pl-3.5 text-gray-400">+86</div>
        <input
          id="phone-login-phone"
          v-model="phone"
          data-testid="phone-login-phone"
          type="tel"
          inputmode="tel"
          autocomplete="tel"
          class="input pl-14"
          :disabled="disabled || verifying"
          :placeholder="t('auth.phonePlaceholder')"
          @blur="normalizePhoneDisplay"
        />
      </div>
    </div>

    <div>
      <label for="phone-login-code" class="input-label">{{ t('auth.verificationCode') }}</label>
      <div class="flex gap-2">
        <input
          id="phone-login-code"
          v-model.trim="code"
          data-testid="phone-login-code"
          type="text"
          inputmode="numeric"
          autocomplete="one-time-code"
          :maxlength="codeLength"
          class="input min-w-0 flex-1"
          :disabled="disabled || verifying"
          :placeholder="t('auth.phoneCodePlaceholder', { length: codeLength })"
          @keydown.enter.prevent="submit"
        />
        <button
          data-testid="phone-login-send"
          type="button"
          class="btn btn-secondary shrink-0"
          :disabled="sendDisabled"
          @click="requestCode"
        >
          {{ retryRemaining > 0 ? t('auth.resendCountdown', { countdown: retryRemaining }) : t('auth.sendCode') }}
        </button>
      </div>
      <p v-if="delivery" data-testid="phone-login-delivery" class="mt-2 text-xs text-gray-500 dark:text-dark-400">
        {{ t(phoneDeliveryMessageKey(delivery)) }}
      </p>
      <p v-if="expired" data-testid="phone-login-expired" class="mt-2 text-sm text-amber-600 dark:text-amber-400">
        {{ t('auth.phoneCodeExpired') }}
      </p>
    </div>

    <p v-if="!registrationEnabled" data-testid="phone-registration-disabled" class="text-sm text-gray-500 dark:text-dark-400">
      {{ t('auth.phoneRegistrationDisabled') }}
    </p>
    <div v-else class="space-y-3 border-t border-gray-100 pt-4 dark:border-dark-700">
      <p class="text-sm text-gray-500 dark:text-dark-400">{{ t('auth.phoneRegistrationHint') }}</p>
      <input
        v-if="invitationCodeEnabled"
        v-model.trim="invitationCode"
        data-testid="phone-login-invitation"
        class="input"
        type="text"
        :placeholder="t('auth.invitationCodePlaceholder')"
        :disabled="disabled || verifying"
      />
      <input
        v-if="promoCodeEnabled"
        v-model.trim="promoCode"
        data-testid="phone-login-promo"
        class="input"
        type="text"
        :placeholder="t('auth.promoCodePlaceholder')"
        :disabled="disabled || verifying"
      />
    </div>

    <button
      data-testid="phone-login-submit"
      type="button"
      class="btn btn-primary w-full"
      :disabled="submitDisabled"
      @click="submit"
    >
      <Icon name="login" size="md" class="mr-2" />
      {{ verifying ? t('auth.signingIn') : t('auth.phoneSignIn') }}
    </button>

    <button
      data-testid="phone-login-existing-account"
      type="button"
      class="w-full text-sm font-medium text-primary-600 transition-colors hover:text-primary-500 dark:text-primary-400 dark:hover:text-primary-300"
      @click="emit('use-existing-account')"
    >
      {{ t('auth.phoneExistingAccount') }}
    </button>
  </div>
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { isTotp2FARequired, sendPhoneCode } from '@/api/auth'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore, useAuthStore } from '@/stores'
import type { ActionCaptchaRequestProof, AuthResponse, TotpLoginResponse } from '@/types'
import { extractI18nErrorMessage } from '@/utils/apiError'
import { normalizeCNPhone, phoneDeliveryMessageKey, phoneRetryAfterSeconds } from '@/utils/phone'

const props = withDefaults(defineProps<{
  disabled?: boolean
  codeLength: number
  registrationEnabled: boolean
  invitationCodeEnabled: boolean
  promoCodeEnabled: boolean
  agreementRevision?: string
  agreementAccepted?: boolean
  getCaptchaProof?: () => Promise<ActionCaptchaRequestProof | null>
}>(), {
  disabled: false,
  agreementRevision: '',
  agreementAccepted: true,
  getCaptchaProof: undefined
})

const emit = defineEmits<{
  authenticated: [response: AuthResponse]
  'requires-2fa': [response: TotpLoginResponse]
  busy: [value: boolean]
  'reset-captcha': []
  'use-existing-account': []
}>()

const { t } = useI18n()
const authStore = useAuthStore()
const appStore = useAppStore()
const phone = ref('')
const code = ref('')
const invitationCode = ref('')
const promoCode = ref('')
const challengeId = ref('')
const challengePhone = ref('')
const delivery = ref('')
const expired = ref(false)
const expiresAt = ref(0)
const retryAt = ref(0)
const now = ref(Date.now())
const sendingPhone = ref('')
const pendingRequests = ref(0)
const verifying = ref(false)
let generation = 0
let timer: ReturnType<typeof setInterval> | null = null

const normalizedPhone = computed(() => normalizeCNPhone(phone.value))
const retryRemaining = computed(() => Math.max(0, Math.ceil((retryAt.value - now.value) / 1000)))
const currentChallenge = computed(() =>
  Boolean(challengeId.value && challengePhone.value === normalizedPhone.value && !expired.value)
)
const sendDisabled = computed(() =>
  props.disabled || verifying.value || !normalizedPhone.value || retryRemaining.value > 0 || sendingPhone.value === normalizedPhone.value
)
const submitDisabled = computed(() =>
  props.disabled || verifying.value || !currentChallenge.value || !new RegExp(`^[0-9]{${props.codeLength}}$`).test(code.value)
)

watch(() => phone.value, () => invalidateChallenge())
watch(() => pendingRequests.value > 0 || verifying.value, (busy) => emit('busy', busy), { immediate: true })

function ensureTimer(): void {
  if (timer) return
  timer = setInterval(() => {
    now.value = Date.now()
    if (expiresAt.value > 0 && now.value >= expiresAt.value) {
      challengeId.value = ''
      expired.value = true
      expiresAt.value = 0
    }
    if (retryAt.value <= now.value && expiresAt.value === 0) {
      clearTimer()
    }
  }, 250)
}

function clearTimer(): void {
  if (timer) clearInterval(timer)
  timer = null
}

function invalidateChallenge(): void {
  generation += 1
  challengeId.value = ''
  challengePhone.value = ''
  delivery.value = ''
  expiresAt.value = 0
  retryAt.value = 0
  expired.value = false
  now.value = Date.now()
  clearTimer()
}

function normalizePhoneDisplay(): void {
  const canonical = normalizedPhone.value
  if (canonical) phone.value = canonical.slice(3)
}

async function requestCode(): Promise<void> {
  const canonical = normalizedPhone.value
  if (!canonical) {
    appStore.showError(t('auth.invalidPhone'))
    return
  }
  const requestGeneration = ++generation
  challengeId.value = ''
  challengePhone.value = ''
  delivery.value = ''
  expired.value = false
  const proof = props.getCaptchaProof ? await props.getCaptchaProof() : {}
  if (proof === null || requestGeneration !== generation || normalizedPhone.value !== canonical) return

  pendingRequests.value += 1
  sendingPhone.value = canonical
  try {
    const result = await sendPhoneCode({ phone: canonical, ...proof })
    if (requestGeneration !== generation || normalizedPhone.value !== canonical) return
    const requestNow = Date.now()
    challengeId.value = result.challenge_id
    challengePhone.value = canonical
    delivery.value = result.delivery
    expiresAt.value = requestNow + Math.max(0, result.expires_in) * 1000
    retryAt.value = requestNow + Math.max(0, result.retry_after || 0) * 1000
    now.value = requestNow
    ensureTimer()
  } catch (error) {
    if (requestGeneration !== generation) return
    const retryAfter = phoneRetryAfterSeconds(error)
    if (retryAfter > 0) {
      now.value = Date.now()
      retryAt.value = now.value + retryAfter * 1000
      ensureTimer()
    }
    appStore.showError(extractI18nErrorMessage(error, t, 'auth.errors', t('auth.sendCodeFailed')))
  } finally {
    pendingRequests.value = Math.max(0, pendingRequests.value - 1)
    if (sendingPhone.value === canonical) sendingPhone.value = ''
    emit('reset-captcha')
  }
}

async function submit(): Promise<void> {
  const canonical = normalizedPhone.value
  if (!canonical) {
    appStore.showError(t('auth.invalidPhone'))
    return
  }
  if (!currentChallenge.value || !new RegExp(`^[0-9]{${props.codeLength}}$`).test(code.value)) {
    appStore.showError(expired.value ? t('auth.phoneCodeExpired') : t('auth.phoneCodeRequired'))
    return
  }

  verifying.value = true
  try {
    const response = await authStore.loginWithPhone({
      phone: canonical,
      challenge_id: challengeId.value,
      code: code.value,
      register_if_new: props.registrationEnabled && props.agreementAccepted,
      agreement_revision: props.registrationEnabled && props.agreementAccepted && props.agreementRevision ? props.agreementRevision : undefined,
      invitation_code: props.registrationEnabled && props.invitationCodeEnabled ? invitationCode.value || undefined : undefined,
      promo_code: props.registrationEnabled && props.promoCodeEnabled ? promoCode.value || undefined : undefined
    })
    if (isTotp2FARequired(response)) emit('requires-2fa', response)
    else emit('authenticated', response)
  } catch (error) {
    appStore.showError(extractI18nErrorMessage(error, t, 'auth.errors', t('auth.loginFailed')))
  } finally {
    verifying.value = false
  }
}

onUnmounted(() => {
  generation += 1
  clearTimer()
})
</script>
