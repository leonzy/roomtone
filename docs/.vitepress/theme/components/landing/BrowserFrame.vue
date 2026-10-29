<script setup lang="ts">
// 用 CSS 给桌面截图套一个浏览器窗口外框（红黄绿点 + 地址栏）。
// src 传亮色图路径；暗色主题下自动切到同名的 -dark 变体（可用 noDark 关闭）。
import { computed, onMounted, ref } from 'vue'
import { useData } from 'vitepress'

const props = defineProps<{ src: string; alt?: string; url?: string; eager?: boolean; noDark?: boolean }>()
const { isDark } = useData()
// 仅在挂载后才应用暗色变体，避免 SSR（isDark=false）与客户端暗色首帧的水合不匹配。
const mounted = ref(false)
onMounted(() => (mounted.value = true))
const resolvedSrc = computed(() =>
  mounted.value && isDark.value && !props.noDark ? props.src.replace(/\.png$/, '-dark.png') : props.src
)
</script>

<template>
  <div class="browser-frame">
    <div class="browser-bar">
      <span class="dot dot-r" />
      <span class="dot dot-y" />
      <span class="dot dot-g" />
      <div class="browser-url">{{ url ?? 'roomtone.local' }}</div>
    </div>
    <img
      class="browser-img"
      :src="resolvedSrc"
      :alt="alt ?? 'Roomtone'"
      :loading="eager ? 'eager' : 'lazy'"
      decoding="async"
    />
  </div>
</template>

<style scoped>
.browser-frame {
  border-radius: 14px;
  overflow: hidden;
  background: var(--vp-c-bg);
  border: 1px solid var(--vp-c-divider);
  box-shadow: 0 24px 60px -24px rgba(0, 0, 0, 0.35), 0 6px 20px -12px rgba(0, 0, 0, 0.2);
}
.browser-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  background: var(--vp-c-bg-soft);
  border-bottom: 1px solid var(--vp-c-divider);
}
.dot {
  width: 11px;
  height: 11px;
  border-radius: 50%;
  display: inline-block;
}
.dot-r { background: #ff5f57; }
.dot-y { background: #febc2e; }
.dot-g { background: #28c840; }
.browser-url {
  flex: 1;
  margin-left: 10px;
  height: 22px;
  line-height: 22px;
  padding: 0 12px;
  font-size: 12px;
  color: var(--vp-c-text-3);
  background: var(--vp-c-bg);
  border: 1px solid var(--vp-c-divider);
  border-radius: 999px;
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
}
.browser-img {
  display: block;
  width: 100%;
  height: auto;
}
</style>
