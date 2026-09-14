export function manualChunks(id: string): string | undefined {
  if (!id.includes('node_modules')) return undefined

  if (
    id.includes('/vue/') ||
    id.includes('/vue-router/') ||
    id.includes('/pinia/') ||
    id.includes('/@vue/')
  ) {
    return 'vendor-vue'
  }
  if (id.includes('/@vueuse/') || id.includes('/xlsx/')) return 'vendor-ui'
  if (id.includes('/chart.js/') || id.includes('/vue-chartjs/')) return 'vendor-chart'
  if (id.includes('/vue-i18n/') || id.includes('/@intlify/')) return 'vendor-i18n'
  if (id.includes('/@stripe/stripe-js/')) return 'vendor-stripe'

  // Airwallex packages initialize remote scripts at module evaluation time.
  if (id.includes('/@airwallex/')) return 'vendor-airwallex'

  return 'vendor-misc'
}
