<template>
  <div class="space-y-3">
  <div class="grid gap-2 sm:grid-cols-[minmax(0,1.4fr)_auto]">
    <input
      v-model="phone"
      data-testid="phone-binding-phone"
      type="tel"
      inputmode="tel"
      autocomplete="tel"
      class="input"
      :placeholder="t('profile.authBindings.phonePlaceholder')"
      :disabled="disabled || binding"
    />
    <button
      data-testid="phone-binding-send"
      type="button"
      class="btn btn-secondary btn-sm"
      :disabled="sendDisabled"
      @click="sendCode"
    >
      {{ retryRemaining > 0 ? t('auth.resendCountdown', { countdown: retryRemaining }) : t('profile.authBindings.sendCodeAction') }}
    </button>
    <input
      v-model.trim="code"
      data-testid="phone-binding-code"
      type="text"
      inputmode="numeric"
      autocomplete="one-time-code"
      :maxlength="codeLength"
      class="input"
      :placeholder="t('profile.authBindings.codePlaceholder')"
      :disabled="disabled || binding"
    />
    <button
      data-testid="phone-binding-submit"
      type="button"
      class="btn btn-primary btn-sm"
      :disabled="submitDisabled"
      @click="bindPhone"
    >
      {{ binding ? t('common.loading') : t('profile.authBindings.confirmPhoneBindAction') }}
    </button>
    <p v-if="delivery" data-testid="phone-binding-delivery" class="text-xs text-gray-500 dark:text-gray-400 sm:col-span-2">
      {{ t(phoneDeliveryMessageKey(delivery)) }}
    </p>
    <p v-if="expired" class="text-sm text-amber-600 dark:text-amber-400 sm:col-span-2">
      {{ t('auth.phoneCodeExpired') }}
    </p>
    <a
      v-if="requiresRelogin"
      data-testid="phone-binding-relogin"
      href="/login?redirect=/profile"
      class="text-sm font-medium text-primary-600 hover:text-primary-500 dark:text-primary-400 sm:col-span-2"
    >
      {{ t('profile.authBindings.reloginAction') }}
    </a>
  </div>
  <CaptchaChallenge
    v-if="captchaEnabled"
    ref="captchaRef"
    :turnstile-enabled="settings?.turnstile_enabled === true"
    :turnstile-site-key="settings?.turnstile_site_key || ''"
    :tencent-enabled="settings?.tencent_captcha_enabled === true"
    :tencent-app-id="settings?.tencent_captcha_app_id || ''"
    :tencent-region="settings?.tencent_captcha_region || 'cn'"
    :aliyun-enabled="settings?.aliyun_captcha_enabled === true"
    :aliyun-scene-id="settings?.aliyun_captcha_scene_id || ''"
    :aliyun-prefix="settings?.aliyun_captcha_prefix || ''"
    :aliyun-region="settings?.aliyun_captcha_region || 'cn'"
    @verify="onCaptchaVerify"
    @expire="resetCaptcha"
    @error="resetCaptcha"
  />
  <TotpStepUpDialog :controller="stepUp" />
  </div>
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { bindPhoneIdentity, sendPhoneBindingCode } from '@/api/user'
import CaptchaChallenge from '@/components/CaptchaChallenge.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import { usePhoneChallenge } from '@/composables/usePhoneChallenge'
import { isStepUpCancelled, useStepUp } from '@/composables/useStepUp'
import { useAppStore } from '@/stores'
import type { ActionCaptchaRequestProof, User } from '@/types'
import { extractApiErrorMessage, extractApiErrorCode } from '@/utils/apiError'
import { normalizeCNPhone, phoneDeliveryMessageKey, phoneRetryAfterSeconds } from '@/utils/phone'

const props = withDefaults(defineProps<{
  codeLength: number
  disabled?: boolean
  getCaptchaProof?: () => Promise<ActionCaptchaRequestProof | null>
}>(), { disabled: false, getCaptchaProof: undefined })
const emit = defineEmits<{ updated: [user: User]; busy: [value: boolean] }>()
const { t } = useI18n()
const appStore = useAppStore()
const stepUp = useStepUp()
const captchaRef = ref<InstanceType<typeof CaptchaChallenge> | null>(null)
const captchaToken = ref('')
const captchaRandstr = ref('')
const phone = ref('')
const code = ref('')
const requiresRelogin = ref(false)
const binding = ref(false)

const normalizedPhone = computed(() => normalizeCNPhone(phone.value))
const challenge = usePhoneChallenge(normalizedPhone)
const { challengeId, delivery, expired, retryRemaining, currentChallenge, sendingCurrentPhone, pendingRequests } = challenge
const settings = computed(() => appStore.cachedPublicSettings)
const aliyunCaptchaReady = computed(() =>
  settings.value?.aliyun_captcha_enabled === true &&
  Boolean(settings.value.aliyun_captcha_scene_id) &&
  Boolean(settings.value.aliyun_captcha_prefix)
)
const actionCaptchaEnabled = computed(() =>
  (settings.value?.tencent_captcha_enabled === true && Boolean(settings.value.tencent_captcha_app_id)) || aliyunCaptchaReady.value
)
const captchaEnabled = computed(() =>
  (settings.value?.turnstile_enabled === true && Boolean(settings.value.turnstile_site_key)) || actionCaptchaEnabled.value
)
const sendDisabled = computed(() =>
  props.disabled || binding.value || !normalizedPhone.value || retryRemaining.value > 0 || sendingCurrentPhone.value
)
const submitDisabled = computed(() =>
  props.disabled || binding.value || !currentChallenge.value || !new RegExp(`^[0-9]{${props.codeLength}}$`).test(code.value)
)

watch(phone, () => invalidateChallenge())
watch(() => pendingRequests.value > 0 || binding.value, (busy) => emit('busy', busy), { immediate: true })

function invalidateChallenge(): void {
  challenge.invalidate()
  requiresRelogin.value = false
}

function handleError(error: unknown, fallback: string): void {
  if (isStepUpCancelled(error)) return
  if (extractApiErrorCode(error) === 'RECENT_AUTH_REQUIRED') requiresRelogin.value = true
  appStore.showError(extractApiErrorMessage(error, fallback))
}

function onCaptchaVerify(token: string, randstr = ''): void {
  captchaToken.value = token
  captchaRandstr.value = randstr
}

function resetCaptcha(): void {
  captchaToken.value = ''
  captchaRandstr.value = ''
  captchaRef.value?.reset()
}

async function acquireCaptchaProof(): Promise<ActionCaptchaRequestProof | null> {
  if (props.getCaptchaProof) return props.getCaptchaProof()
  if (actionCaptchaEnabled.value) {
    const proof = await captchaRef.value?.verifyAction()
    if (!proof) return null
    return settings.value?.tencent_captcha_enabled === true
      ? { tencent_captcha_ticket: proof.token, tencent_captcha_randstr: proof.randstr }
      : { turnstile_token: proof.token }
  }
  if (settings.value?.turnstile_enabled === true) {
    if (!captchaToken.value) {
      appStore.showError(t('auth.completeVerification'))
      return null
    }
    return { turnstile_token: captchaToken.value }
  }
  return {}
}

async function sendCode(): Promise<void> {
  const canonical = normalizedPhone.value
  if (!canonical) {
    appStore.showError(t('auth.invalidPhone'))
    return
  }
  const request = challenge.beginRequest(canonical)
  if (!request) return
  requiresRelogin.value = false
  try {
    const proof = await acquireCaptchaProof()
    if (proof === null || !challenge.isCurrent(request)) return
    const result = await stepUp.run(() => sendPhoneBindingCode({ phone: canonical, ...proof }))
    challenge.adopt(request, result)
  } catch (error) {
    if (!challenge.isCurrent(request)) return
    const retryAfter = phoneRetryAfterSeconds(error)
    challenge.applyRetryAfter(request, retryAfter)
    handleError(error, t('auth.sendCodeFailed'))
  } finally {
    challenge.finishRequest(request)
    resetCaptcha()
  }
}

async function bindPhone(): Promise<void> {
  const canonical = normalizedPhone.value
  if (!canonical || !currentChallenge.value || !new RegExp(`^[0-9]{${props.codeLength}}$`).test(code.value)) {
    appStore.showError(expired.value ? t('auth.phoneCodeExpired') : t('auth.phoneCodeRequired'))
    return
  }
  binding.value = true
  requiresRelogin.value = false
  try {
    const user = await stepUp.run(() => bindPhoneIdentity({
      phone: canonical,
      challenge_id: challengeId.value,
      code: code.value
    }))
    emit('updated', user)
    code.value = ''
    invalidateChallenge()
    appStore.showSuccess(t('profile.authBindings.bindSuccess'))
  } catch (error) {
    handleError(error, t('common.tryAgain'))
  } finally {
    binding.value = false
  }
}

onUnmounted(() => {
  challenge.dispose()
  stepUp.onCancel()
})
</script>
