<script setup lang="ts">
import { inject, type Ref } from 'vue'
import type { Lang } from '../../../data/downloads'
import { createT } from '../../../data/landing-i18n'
import BrowserFrame from './BrowserFrame.vue'

const lang = inject<Ref<Lang>>('landingLang')!
const t = (k: string) => createT(lang.value)(k)
</script>

<template>
  <section class="gallery" data-reveal>
    <div class="landing-container">
      <p class="section-eyebrow">{{ t('gallery.eyebrow') }}</p>
      <h2 class="section-title">{{ t('gallery.title') }}</h2>
      <p class="section-subtitle">{{ t('gallery.subtitle') }}</p>

      <div class="gallery-grid">
        <div class="g-wide">
          <BrowserFrame src="/screenshots/playlists-desktop.png" url="Roomtone · 歌单" />
        </div>
        <BrowserFrame src="/screenshots/settings-desktop.png" url="Roomtone · 设置" />
        <BrowserFrame src="/screenshots/library-desktop.png" url="Roomtone · 歌曲库" />
      </div>
    </div>
  </section>
</template>

<style scoped>
.gallery { padding: 40px 0 8px; }
.gallery-grid {
  margin-top: 34px;
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 24px;
}
.g-wide { grid-column: 1 / -1; }
@media (max-width: 880px) {
  .gallery-grid { grid-template-columns: 1fr; }
  .g-wide { grid-column: auto; }
}
</style>
