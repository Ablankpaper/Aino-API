import { describe, expect, it } from 'vitest'
import { phoneDeliveryMessageKey } from '@/utils/phone'

describe('phone delivery labels', () => {
  it.each([
    ['submitted', 'auth.phoneDeliverySubmitted'],
    ['sending', 'auth.phoneDeliverySending'],
    ['failed', 'auth.phoneDeliveryFailed'],
    ['unknown', 'auth.phoneDeliveryUnknown'],
    ['future-provider-state', 'auth.phoneDeliveryUnknown']
  ])('maps %s without implying confirmed delivery', (status, key) => {
    expect(phoneDeliveryMessageKey(status)).toBe(key)
  })
})
