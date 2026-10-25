# 扫描管理 API

本文档基于以下源文件编写：

- [internal/handlers/scan.go](https://github.com/leonzy/roomtone/blob/main/internal/handlers/scan.go) -- 扫描处理器（扫描动作 + 业务设置端点）
- [internal/app/routers.go](https://github.com/leonzy/roomtone/blob/main/internal/app/routers.go) -- 路由注册
- [internal/services/scan_progress.go](https://github.com/leonzy/roomtone/blob/main/internal/services/scan_progress.go) -- 扫描进度模型
- [internal/services/fingerprint.go](https://github.com/leonzy/roomtone/blob/main/internal/services/fingerprint.go) -- 指纹计算服务

## 目录

1. [概述](#1-概述)
2. [扫描操作端点](#2-扫描操作端点)
   - [POST /scan -- 扫描并导入本地音乐](#21-post-scan----扫描并导入本地音乐)
   - [GET /scan/progress -- 获取扫描进度](#22-get-scanprogress----获取扫描进度)
   - [POST /scan/cancel -- 取消扫描](#23-post-scancancel----取消扫描)
   - [GET /scan/directories -- 获取子目录列表](#24-get-scandirectories----获取子目录列表)
   - [GET /scan/dir-names -- 获取所有目录名称](#25-get-scandir-names----获取所有目录名称)
3. [指纹计算端点](#3-指纹计算端点)
   - [GET /scan/fingerprints/status -- 获取指纹计算状态](#31-get-scanfingerprintsstatus----获取指纹计算状态)
   - [POST /scan/fingerprints -- 触发批量指纹计算](#32-post-scanfingerprints----触发批量指纹计算)
   - [GET /scan/fingerprints/progress -- 获取指纹计算进度](#33-get-scanfingerprintsprogress----获取指纹计算进度)
   - [POST /scan/fingerprints/cancel -- 中断指纹计算](#34-post-scanfingerprintscancel----中断指纹计算)
4. [扫描业务设置端点](#4-扫描业务设置端点)
   - [GET/PUT /settings/music-path -- 音乐路径配置](#41-getput-settingsmusic-path----音乐路径配置)
   - [GET/PUT /settings/scan-playlist-mode -- 歌单归并模式](#42-getput-settingsscan-playlist-mode----歌单归并模式)
   - [GET/PUT /settings/scan-auto-create-playlists -- 自动创建歌单开关](#43-getput-settingsscan-auto-create-playlists----自动创建歌单开关)
   - [GET/PUT /settings/scan-auto-fingerprint -- 自动计算音频指纹开关](#44-getput-settingsscan-auto-fingerprint----自动计算音频指纹开关)
   - [GET/PUT /settings/scan-title-source -- 扫描标题来源配置](#45-getput-settingsscan-title-source----扫描标题来源配置)
   - [GET/PUT /settings/auto-scan -- 自动扫描配置](#46-getput-settingsauto-scan----自动扫描配置)

---

## 1. 概述

扫描管理模块负责本地音乐文件的发现、导入和目录管理。所有端点均需 JWT 认证（`BearerAuth`），基础路径为 `/api/v1`。

`ScanHandler` 同时承载扫描动作端点和扫描相关业务设置端点（`/settings/*`），将 `music_path`、`scan_playlist_mode` 等配置 key 的业务化读写收敛在同一个 handler 中。

---

## 2. 扫描操作端点

### 2.1 POST /scan -- 扫描并导入本地音乐

**方法:** `POST`
**路径:** `/api/v1/scan`
**认证:** 需要 BearerAuth

**描述:** 异步扫描音乐目录并导入新发现的音乐文件到数据库。立即返回，可通过 `/scan/progress` 轮询状态。

**请求体:**

```json
{
  "reimport": false
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `reimport` | boolean | 否 | 是否重新导入已存在的文件（默认 false） |

**成功响应 (200):**

```json
{
  "message": "扫描任务已启动"
}
```

**错误响应:**

| 状态码 | 说明 |
|--------|------|
| 409 | 扫描正在进行中 |
| 500 | 启动扫描失败 |

---

### 2.2 GET /scan/progress -- 获取扫描进度

**方法:** `GET`
**路径:** `/api/v1/scan/progress`
**认证:** 需要 BearerAuth

**描述:** 获取当前扫描任务的进度信息。

**成功响应 (200):**

```json
{
  "status": "scanning",
  "total_files": 1000,
  "scanned_files": 500,
  "imported_files": 480,
  "skipped_files": 15,
  "failed_files": 5,
  "cleaned_files": 0,
  "current_file": "/music/album/track.mp3",
  "start_time": "2026-06-12T10:00:00Z",
  "end_time": null,
  "error": ""
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `status` | string | 当前状态：`idle` / `scanning` / `importing` / `creating_playlists` / `completed` / `failed` / `cancelling` / `cancelled` |
| `total_files` | int | 总文件数 |
| `scanned_files` | int | 已扫描文件数 |
| `imported_files` | int | 已导入文件数 |
| `skipped_files` | int | 跳过的文件数（已存在） |
| `failed_files` | int | 失败的文件数 |
| `cleaned_files` | int | 清理的过期文件数 |
| `current_file` | string | 当前处理的文件路径 |
| `start_time` | string/null | 开始时间（ISO 8601） |
| `end_time` | string/null | 结束时间（ISO 8601） |
| `error` | string | 错误信息 |

---

### 2.3 POST /scan/cancel -- 取消扫描

**方法:** `POST`
**路径:** `/api/v1/scan/cancel`
**认证:** 需要 BearerAuth

**描述:** 取消正在进行的扫描任务。

**成功响应 (200):**

```json
{
  "message": "扫描任务已取消"
}
```

**错误响应:**

| 状态码 | 说明 |
|--------|------|
| 400 | 没有正在进行的扫描任务 |

---

### 2.4 GET /scan/directories -- 获取子目录列表

**方法:** `GET`
**路径:** `/api/v1/scan/directories`
**认证:** 需要 BearerAuth

**描述:** 返回指定路径下的一级子目录列表，用于目录树懒加载。`path` 为空时返回音乐根目录下的子目录。

**查询参数:**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `path` | string | 否 | 目录路径（为空时使用音乐根目录） |

**成功响应 (200):**

```json
{
  "directories": ["/music/pop", "/music/rock"],
  "root": "/music"
}
```

**错误响应:**

| 状态码 | 说明 |
|--------|------|
| 400 | 路径必须在音乐目录下（防止目录遍历攻击） |
| 500 | 读取目录失败 |

---

### 2.5 GET /scan/dir-names -- 获取所有目录名称

**方法:** `GET`
**路径:** `/api/v1/scan/dir-names`
**认证:** 需要 BearerAuth

**描述:** 递归收集音乐目录下所有唯一的目录名称，按字母排序返回，用于排除目录名称的自动补全。

**成功响应 (200):**

```json
{
  "names": ["@eaDir", "Classical", "Pop", "Rock", "tmp"]
}
```

**错误响应:**

| 状态码 | 说明 |
|--------|------|
| 500 | 收集目录名称失败 |

---

## 3. 指纹计算端点

### 3.1 GET /scan/fingerprints/status -- 获取指纹计算状态

**方法:** `GET`
**路径:** `/api/v1/scan/fingerprints/status`
**认证:** 需要 BearerAuth

**描述:** 返回 ffmpeg chromaprint 可用性以及本地歌曲指纹计算统计。

**成功响应 (200):**

```json
{
  "chromaprint_available": true,
  "total": 1000,
  "computed": 795,
  "missing": 200,
  "failed": 5,
  "auto_enabled": false
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `chromaprint_available` | boolean | ffmpeg chromaprint 是否可用 |
| `total` | int | 本地歌曲总数 |
| `computed` | int | 已计算指纹的歌曲数 |
| `missing` | int | 尚未尝试过计算的歌曲数（= total - computed - failed） |
| `failed` | int | 尝试过但失败的歌曲数（无音轨 / 文件损坏 / 超时）。**不会自动重试**，只有 `recompute_all=true` 才会再试 |
| `auto_enabled` | boolean | 扫描后是否自动计算指纹（config `scan_auto_fingerprint`，默认 false） |

---

### 3.2 POST /scan/fingerprints -- 触发批量指纹计算

**方法:** `POST`
**路径:** `/api/v1/scan/fingerprints`
**认证:** 需要 BearerAuth

**描述:** 异步为本地歌曲计算音频指纹，需要 ffmpeg 支持 chromaprint。若已有任务在运行则打断重启。单文件只采样前 120 秒（AcoustID 标准），并发度按 CPU 自适应（`clamp(GOMAXPROCS/4, 1, 4)`），失败的歌曲会被标记为「已尝试」而不再自动重试。

**请求体:**

```json
{
  "recompute_all": false
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `recompute_all` | boolean | 否 | 为 true 时清空已有指纹**与「已尝试」标记**后重新计算全部（默认 false，仅计算从未尝试过的）。这是重试失败项的唯一入口 |

**成功响应 (200):**

```json
{
  "status": "started",
  "total": 200
}
```

**错误响应:**

| 状态码 | 说明 |
|--------|------|
| 400 | ffmpeg chromaprint 不可用 |

---

### 3.3 GET /scan/fingerprints/progress -- 获取指纹计算进度

**方法:** `GET`
**路径:** `/api/v1/scan/fingerprints/progress`
**认证:** 需要 BearerAuth

**描述:** 查询当前指纹计算任务的进度。

**成功响应 (200):**

```json
{
  "status": "running",
  "computed": 150,
  "total": 200,
  "failed": 3
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `status` | string | 任务状态：`idle` / `running` / `done` / `cancelled` |
| `computed` | int | 已计算数量 |
| `total` | int | 总任务数量 |
| `failed` | int | 失败数量 |

---

### 3.4 POST /scan/fingerprints/cancel -- 中断指纹计算

**方法:** `POST`
**路径:** `/api/v1/scan/fingerprints/cancel`
**认证:** 需要 BearerAuth

**描述:** 停止正在运行的批量指纹计算任务并杀掉其 ffmpeg 子进程。指纹任务不挂在扫描的取消通道上（扫描「完成」后该通道已关闭），所以需要独立的取消入口。已算出的指纹保留；被中断的歌曲**不打**「已尝试」标记，下次继续计算。

**成功响应 (200):**

```json
{
  "cancelled": true
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `cancelled` | boolean | 是否确实中断了一个在跑的任务（无任务时为 false，不报错） |

---

## 4. 扫描业务设置端点

以下端点是业务化配置接口，提供强类型 JSON、自带默认值、PUT 后内联触发副作用。与通用 `/configs/{key}` 接口写同一行 config，但前端业务功能应一律走这些端点。

### 4.1 GET/PUT /settings/music-path -- 音乐路径配置

**路径:** `/api/v1/settings/music-path`
**认证:** 需要 BearerAuth

#### GET -- 获取音乐路径与扫描排除配置

**成功响应 (200):**

```json
{
  "path": "music",
  "exclude_dirs": ["@eaDir", "tmp"],
  "exclude_paths": []
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `path` | string | 音乐目录路径（默认 `"music"`） |
| `exclude_dirs` | string[] | 排除的目录名称列表 |
| `exclude_paths` | string[] | 排除的具体路径列表 |

#### PUT -- 更新音乐路径与扫描排除配置

写入配置后异步触发 Scanner 重建 + 清理排除目录中的歌曲（副作用与 admin `/configs/music_path` PUT 一致）。

**请求体:** 同 GET 响应格式。`path` 不能为空。

**错误响应:**

| 状态码 | 说明 |
|--------|------|
| 400 | 请求格式错误或 path 为空 |
| 500 | 保存配置失败 |

---

### 4.2 GET/PUT /settings/scan-playlist-mode -- 歌单归并模式

**路径:** `/api/v1/settings/scan-playlist-mode`
**认证:** 需要 BearerAuth

控制扫描后自动创建歌单时的目录归并模式。默认 `directory`。

#### GET 响应 / PUT 请求体:

```json
{
  "mode": "directory"
}
```

| 字段 | 类型 | 可选值 | 说明 |
|------|------|--------|------|
| `mode` | string | `directory` / `top_level` / `bubble_up` | `directory`：每个文件夹生成独立歌单（默认）；`top_level`：按一级子目录合并歌单；`bubble_up`：歌曲同时出现在所有上级文件夹歌单 |

**PUT 错误响应:**

| 状态码 | 说明 |
|--------|------|
| 400 | 请求格式错误或 `mode` 值非法（仅接受上述三个枚举值） |
| 500 | 保存配置失败 |

---

### 4.3 GET/PUT /settings/scan-auto-create-playlists -- 自动创建歌单开关

**路径:** `/api/v1/settings/scan-auto-create-playlists`
**认证:** 需要 BearerAuth

控制扫描完成后是否根据音乐目录结构自动创建歌单。默认启用（`true`）。关闭后扫描仅入库歌曲，不再自动建歌单。

#### GET 响应 / PUT 请求体:

```json
{
  "enabled": true
}
```

---

### 4.4 GET/PUT /settings/scan-auto-fingerprint -- 自动计算音频指纹开关

**路径:** `/api/v1/settings/scan-auto-fingerprint`
**认证:** 需要 BearerAuth

控制扫描完成后是否自动为缺失指纹的本地歌曲计算 chromaprint 音频指纹。**默认关闭（`false`）**：指纹只服务于「重复歌曲检测」和插件歌词/封面搜索，属按需功能，全库自动计算会在扫描进度已显示「完成」之后继续长时间占用 CPU（songloft-org/songloft#323）。关闭时用户可在重复检测页手动 `POST /scan/fingerprints`。

开启后单文件开销恒定（只采样前 120 秒），并发按 CPU 自适应，失败只尝试一次，可随时通过 `POST /scan/fingerprints/cancel` 停止。

#### GET 响应 / PUT 请求体:

```json
{
  "enabled": false
}
```

---

### 4.5 GET/PUT /settings/scan-title-source -- 扫描标题来源配置

**路径:** `/api/v1/settings/scan-title-source`
**认证:** 需要 BearerAuth

配置扫描时歌曲标题的来源方式。切换后需以「重新导入」模式扫描才能生效。PUT 完成后触发 Scanner 重建。

#### GET 响应 / PUT 请求体:

```json
{
  "title_source": "tag"
}
```

| 字段 | 类型 | 可选值 | 说明 |
|------|------|--------|------|
| `title_source` | string | `tag` / `filename` | `tag`：优先使用音频标签中的标题（默认）；`filename`：始终使用文件名（不含扩展名） |

**PUT 错误响应:**

| 状态码 | 说明 |
|--------|------|
| 400 | 请求格式错误或 `title_source` 值无效 |
| 500 | 保存配置失败 |

---

### 4.6 GET/PUT /settings/auto-scan -- 自动扫描配置

**路径:** `/api/v1/settings/auto-scan`
**认证:** 需要 BearerAuth

配置自动扫描的启用状态和扫描间隔。更新后立即生效（无需重启），异步触发自动扫描调度器重新配置。

#### GET 响应 / PUT 请求体:

```json
{
  "enabled": false,
  "interval_seconds": 3600
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `enabled` | boolean | 是否启用自动扫描（默认 `false`） |
| `interval_seconds` | int | 扫描间隔秒数（默认 3600，有效范围 60-86400） |

**PUT 错误响应:**

| 状态码 | 说明 |
|--------|------|
| 400 | 请求格式错误或 `interval_seconds` 不在 60-86400 范围内 |
| 500 | 保存配置失败 |

---

**章节来源：** `internal/handlers/scan.go` / `internal/app/routers.go` / `internal/services/scan_progress.go` / `internal/services/fingerprint.go`

## 当前源码补充接口

以下接口补齐旧版列表遗漏。请求字段、响应模型与副作用以对应 handler 和 Swagger 为准。

### GET /api/v1/scan/fingerprints/failed

获取指纹计算失败的歌曲列表。

| 状态 | 响应类型 |
| --- | --- |
| 200 | `object` |
| 500 | `object` |
