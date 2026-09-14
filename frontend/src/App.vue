<script setup lang="ts">
import { computed } from 'vue'
import { RouterView, useRoute } from 'vue-router'
import AppStandardShell from '@/components/AppStandardShell.vue'

const route = useRoute()
const isIsolatedPublicRoute = computed(() => route.meta.isolatedPublic === true)
</script>

<template>
  <RouterView v-if="isIsolatedPublicRoute" />
  <AppStandardShell v-else />
</template>
