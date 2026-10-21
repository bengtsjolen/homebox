<template>
  <ScannerXRayView />
</template>

<script setup lang="ts">
  import ScannerXRayView from "@/components/Scanner/XRayView.vue";

  definePageMeta({
    middleware: ["auth"],
    layout: "empty",
  });

  useHead({
    title: "HomeBox | X-ray",
  });
</script>
