<template>
  <main class="flex min-h-screen items-center justify-center bg-gray-50 px-5 py-8 dark:bg-dark-900">
    <div class="w-full max-w-sm text-center">
      <h1 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('auth.desktopCaptcha.title') }}</h1>

      <p v-if="state === 'loading'" data-testid="desktop-captcha-loading" class="mt-3 text-sm text-gray-500 dark:text-dark-400">
        {{ t('auth.desktopCaptcha.loading') }}
      </p>
      <p v-else-if="state === 'unavailable'" data-testid="desktop-captcha-unavailable" class="mt-3 text-sm text-red-600 dark:text-red-400">
        {{ t('auth.desktopCaptcha.unavailable') }}
      </p>
      <p v-else-if="state === 'submitting'" class="mt-3 text-sm text-gray-500 dark:text-dark-400">
        {{ t('auth.desktopCaptcha.submitting') }}
      </p>
      <p v-else-if="state === 'complete'" data-testid="desktop-captcha-complete" class="mt-3 text-sm text-green-600 dark:text-green-400">
        {{ t('auth.desktopCaptcha.complete') }}
      </p>
      <p v-else-if="state === 'failed'" data-testid="desktop-captcha-failed" class="mt-3 text-sm text-red-600 dark:text-red-400">
        {{ t('auth.desktopCaptcha.failed') }}
      </p>

      <template v-if="state === 'ready'">
        <CaptchaChallenge
          ref="captchaRef"
          class="mt-5"
          :turnstile-enabled="provider === 'turnstile'"
          :turnstile-site-key="settings?.turnstile_site_key || ''"
          :tencent-enabled="provider === 'tencent'"
          :tencent-app-id="settings?.tencent_captcha_app_id || ''"
          :tencent-region="settings?.tencent_captcha_region || 'cn'"
          :aliyun-enabled="provider === 'aliyun'"
          :aliyun-scene-id="settings?.aliyun_captcha_scene_id || ''"
          :aliyun-prefix="settings?.aliyun_captcha_prefix || ''"
          :aliyun-region="settings?.aliyun_captcha_region || 'cn'"
          @verify="completeWidgetProof"
          @error="state = 'failed'"
        />
        <button
          v-if="provider === 'tencent' || provider === 'aliyun'"
          data-testid="desktop-captcha-start"
          type="button"
          class="btn btn-primary mt-5 w-full"
          @click="startActionChallenge"
        >
          {{ t('auth.desktopCaptcha.start') }}
        </button>
      </template>
    </div>
  </main>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { getIsolatedPublicSettings } from '@/api/publicSettings'
import CaptchaChallenge, { type ActionCaptchaResult } from '@/components/CaptchaChallenge.vue'
import type { ActionCaptchaRequestProof, PublicSettings } from '@/types'

type CaptchaProvider = 'turnstile' | 'tencent' | 'aliyun' | null
type ViewState = 'loading' | 'ready' | 'submitting' | 'complete' | 'failed' | 'unavailable'

const { t } = useI18n()
const captchaRef = ref<InstanceType<typeof CaptchaChallenge> | null>(null)
const settings = ref<PublicSettings | null>(null)
const provider = ref<CaptchaProvider>(null)
const state = ref<ViewState>('loading')
let bridge: Window['ainoCaptcha']
let nonce = ''
let submitted = false

function resolveProvider(config: PublicSettings): CaptchaProvider | 'disabled' | 'invalid' {
  const enabledProviders = [
    config.turnstile_enabled
      ? { provider: 'turnstile' as const, configured: Boolean(config.turnstile_site_key) }
      : null,
    config.tencent_captcha_enabled
      ? { provider: 'tencent' as const, configured: Boolean(config.tencent_captcha_app_id) }
      : null,
    config.aliyun_captcha_enabled
      ? { provider: 'aliyun' as const, configured: Boolean(config.aliyun_captcha_scene_id && config.aliyun_captcha_prefix) }
      : null
  ].filter((entry): entry is NonNullable<typeof entry> => entry !== null)

  if (enabledProviders.length === 0) return 'disabled'
  if (enabledProviders.length !== 1 || !enabledProviders[0].configured) return 'invalid'
  return enabledProviders[0].provider
}

async function submitProof(proof: ActionCaptchaRequestProof): Promise<void> {
  if (submitted || !bridge || !nonce) return
  submitted = true
  state.value = 'submitting'
  try {
    await bridge.submit({ nonce, proof })
    state.value = 'complete'
  } catch {
    state.value = 'failed'
  }
}

function completeWidgetProof(token: string, randstr = ''): void {
  if (!token) return
  if (provider.value === 'tencent') {
    void submitProof({ tencent_captcha_ticket: token, tencent_captcha_randstr: randstr })
  } else {
    void submitProof({ turnstile_token: token })
  }
}

async function startActionChallenge(): Promise<void> {
  if (submitted || state.value !== 'ready') return
  const proof: ActionCaptchaResult | null = await captchaRef.value?.verifyAction() ?? null
  if (!proof) return
  completeWidgetProof(proof.token, proof.randstr)
}

onMounted(async () => {
  const candidate = window.ainoCaptcha
  if (!candidate || typeof candidate.getChallenge !== 'function' || typeof candidate.submit !== 'function') {
    state.value = 'unavailable'
    return
  }
  bridge = candidate

  try {
    const challenge = await candidate.getChallenge()
    if (!challenge || typeof challenge.nonce !== 'string' || !challenge.nonce || challenge.nonce !== challenge.nonce.trim()) {
      state.value = 'unavailable'
      return
    }
    nonce = challenge.nonce
    settings.value = await getIsolatedPublicSettings()
    const resolved = resolveProvider(settings.value)
    if (resolved === 'invalid') {
      state.value = 'unavailable'
      return
    }
    if (resolved === 'disabled') {
      await submitProof({})
      return
    }
    provider.value = resolved
    state.value = 'ready'
  } catch {
    state.value = 'unavailable'
  }
})
</script>
