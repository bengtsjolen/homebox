<template>
  <div class="relative h-dvh w-dvw overflow-hidden bg-black">
    <img
      v-if="currentPhoto"
      :src="currentPhoto.src"
      :alt="title"
      class="absolute inset-0 size-full object-contain"
    />

    <div
      v-if="!error"
      class="pointer-events-none absolute inset-x-0 top-0 z-10 h-40 bg-gradient-to-b from-black/70 to-transparent"
    />

    <div v-if="error" class="absolute inset-0 z-20 flex items-center justify-center bg-background/90 p-6">
      <div class="flex max-w-sm flex-col items-center gap-4 text-center">
        <MdiAlertCircleOutline class="size-12 text-destructive" />
        <p class="text-sm text-foreground">{{ t(error) }}</p>
        <Button variant="outline" @click="startCamera">
          {{ t("scanner_ar.retry") }}
        </Button>
      </div>
    </div>

    <div v-else class="pointer-events-none absolute inset-0 z-20 flex flex-col">
      <div class="pointer-events-auto flex items-center gap-3 p-3">
        <Button
          variant="ghost"
          size="icon"
          class="text-white hover:bg-white/10"
          :aria-label="t('scanner_ar.back')"
          @click="handleBack"
        >
          <MdiArrowLeft class="size-5" />
        </Button>
        <span class="text-sm font-semibold text-white">{{ t("scanner_xray.title") }}</span>
        <div class="grow" />
        <div v-if="isScanning" class="flex items-center gap-1.5">
          <span class="size-2 animate-pulse rounded-full bg-green-500" />
          <span class="text-xs text-white/80">{{ t("scanner_ar.scanning") }}</span>
        </div>
        <Button
          variant="outline"
          size="sm"
          class="border-white/30 bg-black/40 text-white hover:bg-black/60"
          @click="navigateTo('/scanner-ar')"
        >
          {{ t("scanner_ar.ar_mode") }}
        </Button>
      </div>

      <div class="pointer-events-none px-4">
        <p v-if="title" class="text-balance text-2xl font-semibold text-white drop-shadow-md">
          {{ title }}
        </p>
        <p v-else-if="lookingUp" class="text-sm text-white/80">{{ t("scanner_xray.looking_up") }}</p>
        <p v-else class="text-sm text-white/80">{{ t("scanner_xray.point_at_code") }}</p>
        <p v-if="title && !photos.length" class="mt-2 text-sm text-white/80">{{ t("scanner_xray.no_photo") }}</p>
        <p v-else-if="photos.length > 1" class="mt-1 text-xs text-white/70">
          {{ shownPhotoNumber }} / {{ photos.length }}
        </p>
      </div>
    </div>

    <video
      v-show="!error"
      ref="videoEl"
      class="absolute bottom-3 right-3 z-30 aspect-video w-32 rounded-lg border border-white/40 object-cover shadow-lg sm:w-44"
      autoplay
      muted
      playsinline
      poster="data:image/gif,AAAA"
    />

    <div
      v-if="!error"
      class="absolute inset-x-0 bottom-0 z-30 flex items-center gap-2 bg-black/50 p-3 pr-36 backdrop-blur-sm sm:pr-52"
    >
      <Button
        variant="outline"
        size="icon"
        class="border-white/30 bg-black/40 text-white hover:bg-black/60"
        :disabled="photos.length < 2"
        :aria-label="t('scanner_xray.previous_image')"
        @click="stepPhoto(-1)"
      >
        <MdiChevronLeft class="size-5" />
      </Button>
      <Button
        variant="outline"
        size="icon"
        class="border-white/30 bg-black/40 text-white hover:bg-black/60"
        :disabled="photos.length < 2"
        :aria-label="t('scanner_xray.next_image')"
        @click="stepPhoto(1)"
      >
        <MdiChevronRight class="size-5" />
      </Button>
      <Button class="ml-auto" :disabled="!openTo" @click="openShown">
        {{ t("scanner_xray.open") }}
      </Button>
      <Button
        variant="outline"
        size="icon"
        class="border-white/30 bg-black/40 text-white hover:bg-black/60"
        :aria-label="t('scanner_ar.switch_camera')"
        @click="switchCamera"
      >
        <MdiCameraFlip class="size-5" />
      </Button>
    </div>
  </div>
</template>

<script setup lang="ts">
  import { computed, ref, watch, onMounted, onBeforeUnmount } from "vue";
  import { useI18n } from "vue-i18n";
  import { useBarcodeDetector, type DetectedEntity, type EntityData } from "@/composables/use-barcode-detector";
  import { Button } from "@/components/ui/button";
  import MdiAlertCircleOutline from "~icons/mdi/alert-circle-outline";
  import MdiArrowLeft from "~icons/mdi/arrow-left";
  import MdiCameraFlip from "~icons/mdi/camera-flip";
  import MdiChevronLeft from "~icons/mdi/chevron-left";
  import MdiChevronRight from "~icons/mdi/chevron-right";

  type Pictured = {
    id: string;
    name: string;
    attachments?: { id: string; type: string; primary: boolean }[];
  };

  const { t } = useI18n();
  const api = useUserApi();
  const videoEl = ref<HTMLVideoElement>();
  const { isScanning, error, detections, startCamera, stopCamera, switchCamera } = useBarcodeDetector(videoEl);

  const shown = ref<DetectedEntity | null>(null);
  const shownKey = ref("");
  const photoIndex = ref(0);

  function area(detection: DetectedEntity) {
    return detection.boundingBox.width * detection.boundingBox.height;
  }

  const focus = computed(() => {
    const ready = detections.value.filter(detection => detection.data && !detection.error);
    if (!ready.length) {
      return null;
    }
    return ready.reduce((best, detection) => (area(detection) > area(best) ? detection : best));
  });

  const lookingUp = computed(() => detections.value.some(detection => detection.loading));

  watch(focus, next => {
    if (!next) {
      return;
    }
    const key = `${next.entityType}:${next.id}`;
    if (key !== shownKey.value) {
      shownKey.value = key;
      photoIndex.value = 0;
    }
    shown.value = next;
  });

  function picturedOf(data: EntityData | null | undefined): Pictured | undefined {
    const entity = (data?.item || data?.location) as Pictured | undefined;
    if (!entity?.id || !entity.name) {
      return undefined;
    }
    return entity;
  }

  const pictured = computed(() => picturedOf(shown.value?.data));

  const title = computed(() => {
    if (pictured.value?.name) {
      return pictured.value.name;
    }
    const count = shown.value?.data?.childItems?.length ?? 0;
    if (count > 0) {
      return t("scanner_xray.several_items", { count });
    }
    return "";
  });

  const photos = computed(() => {
    const entity = pictured.value;
    if (!entity?.attachments?.length) {
      return [];
    }
    return entity.attachments
      .filter(attachment => attachment.type === "photo")
      .slice()
      .sort((left, right) => Number(right.primary) - Number(left.primary))
      .map(attachment => ({
        id: attachment.id,
        src: api.authURL(`/entities/${entity.id}/attachments/${attachment.id}`),
      }));
  });

  const shownPhotoNumber = computed(() => {
    if (!photos.value.length) {
      return 0;
    }
    return Math.min(photoIndex.value, photos.value.length - 1) + 1;
  });

  const currentPhoto = computed(() => {
    const list = photos.value;
    if (!list.length) {
      return null;
    }
    return list[shownPhotoNumber.value - 1] ?? null;
  });

  const openTo = computed(() => {
    const data = shown.value?.data;
    if (!data) {
      return "";
    }
    if (data.item?.id) {
      return `/item/${data.item.id}`;
    }
    if (data.location?.id) {
      return `/location/${data.location.id}`;
    }
    const assetId = data.childItems?.[0]?.assetId;
    return assetId ? `/assets/${assetId}` : "";
  });

  function stepPhoto(delta: number) {
    const count = photos.value.length;
    if (count < 2) {
      return;
    }
    photoIndex.value = (photoIndex.value + delta + count) % count;
  }

  function openShown() {
    if (openTo.value) {
      navigateTo(openTo.value);
    }
  }

  function handleBack() {
    useRouter().back();
  }

  onMounted(() => {
    startCamera();
  });

  onBeforeUnmount(() => {
    stopCamera();
  });
</script>
