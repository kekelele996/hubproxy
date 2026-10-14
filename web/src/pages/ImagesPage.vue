<script setup lang="ts">
import { ref } from 'vue'
import { Loader2 } from 'lucide-vue-next'
import {
  fetchImageInfo,
  prepareBatchDownload,
  prepareSingleDownload,
  triggerDownload,
} from '@/api'
import { errorMessage } from '@/lib/utils'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'
import PageHero from '@/components/PageHero.vue'
import Switch from '@/components/ui/Switch.vue'
import Textarea from '@/components/ui/Textarea.vue'

const singleImage = ref('')
const singlePlatform = ref('linux/amd64')
const singleCompressed = ref(true)
const singleStatus = ref('')
const singleError = ref('')
const singleLoading = ref(false)

const batchText = ref('')
const batchPlatform = ref('linux/amd64')
const batchCompressed = ref(true)
const batchStatus = ref('')
const batchError = ref('')
const batchLoading = ref(false)

interface BatchCheckItem {
  image: string
  ok: boolean
  error?: string
}

const batchChecks = ref<BatchCheckItem[]>([])
const failedChecks = () => batchChecks.value.filter((item) => !item.ok)

async function preflight(images: string[]) {
  for (const image of [...new Set(images)]) {
    await fetchImageInfo(image)
  }
}

// 逐项校验镜像，单项失败不影响其他项
async function checkBatchImages(images: string[]): Promise<BatchCheckItem[]> {
  return Promise.all(
    images.map(async (image) => {
      try {
        await fetchImageInfo(image)
        return { image, ok: true }
      } catch (e) {
        return { image, ok: false, error: errorMessage(e, '镜像不可用') }
      }
    }),
  )
}

async function onSingleSubmit() {
  singleError.value = ''
  singleStatus.value = ''
  const image = singleImage.value.trim()
  if (!image) {
    singleError.value = '请输入镜像名称'
    return
  }

  singleLoading.value = true
  singleStatus.value = '正在准备下载...'
  try {
    await preflight([image])
    const data = await prepareSingleDownload({
      image,
      platform: singlePlatform.value,
      compressed: singleCompressed.value,
    })
    if (!data.download_url) throw new Error('下载地址生成失败')
    triggerDownload(data.download_url)
    const platformText = singlePlatform.value.trim()
      ? ` (${singlePlatform.value.trim()})`
      : ''
    singleStatus.value = `开始下载 ${image}${platformText}`
  } catch (e) {
    singleStatus.value = ''
    singleError.value = errorMessage(e, '下载失败')
  } finally {
    singleLoading.value = false
  }
}

async function onBatchSubmit() {
  batchError.value = ''
  batchStatus.value = ''
  batchChecks.value = []
  const images = [
    ...new Set(
      batchText.value
        .split('\n')
        .map((line) => line.trim())
        .filter((line) => line && !line.startsWith('#')),
    ),
  ]

  if (images.length === 0) {
    batchError.value = '请输入镜像列表'
    return
  }

  batchLoading.value = true
  batchStatus.value = '正在逐个校验镜像...'
  try {
    const checks = await checkBatchImages(images)
    batchChecks.value = checks
    const validImages = checks.filter((item) => item.ok).map((item) => item.image)
    const failedCount = checks.length - validImages.length

    if (validImages.length === 0) {
      batchStatus.value = ''
      batchError.value = '镜像全部校验失败，未开始下载'
      return
    }

    batchStatus.value = `正在准备下载 ${validImages.length} 个镜像...`
    const data = await prepareBatchDownload({
      images: validImages,
      platform: batchPlatform.value,
      useCompressedLayers: batchCompressed.value,
    })
    if (!data.download_url) throw new Error('下载地址生成失败')
    triggerDownload(data.download_url)
    const skipped = failedCount > 0 ? `，已跳过 ${failedCount} 个校验失败的镜像` : ''
    batchStatus.value =
      `开始下载 ${validImages.length} 个镜像${skipped}；` +
      '打包结果见 tar 包内 download-report.txt'
  } catch (e) {
    batchStatus.value = ''
    batchError.value = errorMessage(e, '下载失败')
  } finally {
    batchLoading.value = false
  }
}
</script>

<template>
  <div class="mx-auto max-w-3xl">
    <PageHero
      eyebrow="Offline Image"
      title="离线镜像"
      subtitle="流式下载，兼容 docker load，支持多架构。"
    />

    <section class="field-block">
      <h2 class="text-center text-sm font-semibold tracking-[0.16em] text-muted-foreground uppercase">
        单镜像
      </h2>

      <Transition name="fade" mode="out-in">
        <p v-if="singleError" key="error" class="text-center text-destructive">{{ singleError }}</p>
        <p v-else-if="singleStatus" key="status" class="flex items-center justify-center gap-2 text-muted-foreground">
          <Loader2 v-if="singleLoading" class="size-4 animate-spin" />
          {{ singleStatus }}
        </p>
      </Transition>

      <label class="block space-y-1.5">
        <span>镜像名称</span>
        <Input v-model="singleImage" placeholder="nginx 或 user/app:tag" />
      </label>
      <label class="block space-y-1.5">
        <span>目标架构（可选）</span>
        <Input v-model="singlePlatform" placeholder="linux/amd64" />
      </label>
      <div class="flex items-center justify-between py-1">
        <span>压缩层</span>
        <Switch v-model:checked="singleCompressed" />
      </div>
      <Button class="w-full" :disabled="singleLoading" @click="onSingleSubmit">
        <Loader2 v-if="singleLoading" class="size-4 animate-spin" />
        {{ singleLoading ? '准备中...' : '立即下载' }}
      </Button>
    </section>

    <section class="section-gap field-block">
      <h2 class="text-center text-sm font-semibold tracking-[0.16em] text-muted-foreground uppercase">
        批量下载
      </h2>

      <Transition name="fade" mode="out-in">
        <p v-if="batchError" key="error" class="text-center text-destructive">{{ batchError }}</p>
        <p v-else-if="batchStatus" key="status" class="flex items-center justify-center gap-2 text-muted-foreground">
          <Loader2 v-if="batchLoading" class="size-4 animate-spin" />
          {{ batchStatus }}
        </p>
      </Transition>

      <label class="block space-y-1.5">
        <span>镜像列表</span>
        <Textarea
          v-model="batchText"
          placeholder="alpine&#10;redis:alpine&#10;user/app:1.0"
        />
      </label>
      <label class="block space-y-1.5">
        <span>目标架构（可选）</span>
        <Input v-model="batchPlatform" placeholder="linux/amd64" />
      </label>
      <div class="flex items-center justify-between py-1">
        <span>压缩层</span>
        <Switch v-model:checked="batchCompressed" />
      </div>
      <div
        v-if="failedChecks().length"
        class="space-y-1 rounded-md border border-destructive/40 p-3 text-sm"
      >
        <p class="font-medium text-destructive">以下镜像校验失败，将自动跳过：</p>
        <ul class="space-y-0.5">
          <li v-for="item in failedChecks()" :key="item.image" class="text-destructive">
            {{ item.image }}<span class="opacity-75">：{{ item.error }}</span>
          </li>
        </ul>
      </div>
      <p class="text-xs text-muted-foreground">
        单个镜像打包失败会自动跳过，不影响其他镜像；下载完成后可在 tar 包内查看
        download-report.txt / download-report.json 了解成功与失败明细。
      </p>
      <Button class="w-full" :disabled="batchLoading" @click="onBatchSubmit">
        <Loader2 v-if="batchLoading" class="size-4 animate-spin" />
        {{ batchLoading ? '准备中...' : '批量下载' }}
      </Button>
    </section>
  </div>
</template>
