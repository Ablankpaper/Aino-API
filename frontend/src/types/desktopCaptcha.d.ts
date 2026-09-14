import type { ActionCaptchaRequestProof } from './index'

interface DesktopCaptchaBridge {
  getChallenge(): Promise<{ nonce: string }> | { nonce: string }
  submit(input: { nonce: string; proof: ActionCaptchaRequestProof }): Promise<void>
}

declare global {
  interface Window {
    ainoCaptcha?: DesktopCaptchaBridge
  }
}

export {}
