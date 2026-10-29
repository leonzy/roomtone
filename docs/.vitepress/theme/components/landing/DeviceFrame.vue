<script setup lang="ts">
// 用 CSS 给移动截图套一个手机外壳（圆角，无刘海）。
// src 传亮色图路径；暗色主题下自动切到同名的 -dark 变体（可用 noDark 关闭）。
import { computed, onMounted, ref } from 'vue'
import { useData } from 'vitepress'

const props = defineProps<{ src: string; alt?: string; eager?: boolean; noDark?: boolean }>()
const { isDark } = useData()
// 仅在挂载后才应用暗色变体，避免 SSR（isDark=false）与客户端暗色首帧的水合不匹配。
const mounted = ref(false)
onMounted(() => (mounted.value = true))
const resolvedSrc = computed(() =>
  mounted.value && isDark.value && !props.noDark ? props.src.replace(/\.png$/, '-dark.png') : props.src
)
</script>

<template>
  <div class="device-frame">
    <img
      class="device-img"
      :src="resolvedSrc"
      :alt="alt ?? 'Roomtone'"
      :loading="eager ? 'eager' : 'lazy'"
      decoding="async"
    />
  </div>
</template>

<style scoped>
.device-frame {
  position: relative;
  width: 100%;
  max-width: 300px;
  margin: 0 auto;
  padding: 12px;
  border-radius: 40px;
  background: linear-gradient(160deg, #2a2a30, #17171b);
  box-shadow: 0 30px 70px -28px rgba(0, 0, 0, 0.55), 0 8px 24px -14px rgba(0, 0, 0, 0.35);
}
.device-img {
  display: block;
  width: 100%;
  height: auto;
  border-radius: 30px;
}
</style>
