// @vitest-environment node

import { describe, expect, it } from 'vitest'
import { manualChunks } from '../../../../tooling/manualChunks'

describe('Airwallex lazy-loading contract', () => {
  it('keeps the side-effectful payment SDK out of the shared vendor chunk', () => {
    const airwallexChunk = manualChunks('/workspace/node_modules/@airwallex/components-sdk/lib/index.js')
    const airtrackerChunk = manualChunks('/workspace/node_modules/@airwallex/airtracker/lib/index.js')
    const sharedVendorChunk = manualChunks('/workspace/node_modules/axios/index.js')

    expect(airwallexChunk).toBeTruthy()
    expect(airtrackerChunk).toBe(airwallexChunk)
    expect(airwallexChunk).not.toBe(sharedVendorChunk)
  })
})
