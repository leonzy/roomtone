# API 接口参考

本文档基于以下源文件编写：

- [internal/app/routers.go](https://github.com/leonzy/roomtone/blob/main/internal/app/routers.go) -- 完整路由注册（API v1 路由组、中间件挂载、JS 插件路由）
- [internal/handlers/response.go](https://github.com/leonzy/roomtone/blob/main/internal/handlers/response.go) -- respondJSON / respondError 响应辅助函数
- [internal/middleware/auth.go](https://github.com/leonzy/roomtone/blob/main/internal/middleware/auth.go) -- JWT 认证中间件（Bearer Token + access_token 查询参数回退）
- [docs/api_response.md](https://github.com/leonzy/roomtone/blob/main/docs/api_response.md) -- API 响应格式规范

## 目录

1. [基础信息](#1-基础信息)
   - [API 版本与基础 URL](#11-api-版本与基础-url)
   - [认证方式](#12-认证方式)
   - [响应格式](#13-响应格式)
   - [Content-Type 约定](#14-content-type-约定)
   - [分页](#15-分页)
2. [公开端点与认证端点](#2-公开端点与认证端点)
3. [API 分组索引](#3-api-分组索引)

---

## 1. 基础信息

### 1.1 API 版本与基础 URL

**章节来源**: `internal/app/routers.go`

所有业务接口统一注册在 `/api/v1` 路由组下。完整 URL 格式：

```
{scheme}://{host}:{port}{base_path}/api/v1/{endpoint}
```

| 组成部分 | 默认值 | 说明 |
|----------|--------|------|
| `scheme` | `http` | 反代后可为 `https` |
| `host:port` | `localhost:58091` | 启动时通过 `-port` 参数配置 |
| `base_path` | 空 | 通过 `-base-path /xxx` 或环境变量 `BASE_PATH=/xxx` 配置子路径部署；后端用 `http.StripPrefix` 剥离前缀 |

示例：默认启动时，歌曲列表接口为 `http://localhost:58091/api/v1/songs`；若配置 `BASE_PATH=/music`，则为 `http://localhost:58091/music/api/v1/songs`。

当前 API 版本为 **v1**，所有端点均以 `/api/v1/` 为前缀。开发模式下可通过 `/swagger/index.html` 查看交互式 API 文档。

### 1.2 认证方式

**章节来源**: `internal/middleware/auth.go`

Songloft 使用 **JWT 双令牌机制**（Access Token + Refresh Token）。认证中间件按以下优先级获取令牌：

1. **Authorization 请求头**（首选）：`Authorization: Bearer <access_token>`
2. **URL 查询参数**（回退）：`?access_token=<token>`，用于 `<img>` 标签、音频播放器等无法自定义 Header 的场景

认证失败时返回 JSON 格式错误（而非纯文本），保持与业务端点一致：

```json
{"error": "缺少认证信息"}
{"error": "无效的 token", "detail": "token has expired"}
```

> **特殊处理**: 小爱音箱固件会将 URL 中的 `&` 替换为空格，中间件会自动拆分并还原被吞掉的参数。

### 1.3 响应格式

**章节来源**: `internal/handlers/response.go`、`docs/api_response.md`

项目采用 **RESTful 直返风格**，不使用 `{code, data, message}` 统一信封。

#### 成功响应

| 场景 | 格式 | 示例 |
|------|------|------|
| 单个实体 | 直接返回模型对象 | `{"id":1, "title":"Sample Track", ...}` |
| 列表（含分页） | 集合名 + 分页元数据 | `{"songs":[...], "total":100, "limit":20, "offset":0}` |
| 操作结果 | `{"message": "..."}` | `{"message": "歌曲已删除"}` |

#### 错误响应

所有 API 端点的错误统一通过 `respondError` 返回，格式固定：

```json
{"error": "人类可读的错误信息", "detail": "可选的技术细节"}
```

- `error`（必有）：面向用户的简短描述
- `detail`（可选）：底层错误信息，仅当内部 `err != nil` 时输出

中间件层错误（认证失败等）同样返回 JSON 格式，使用相同的 `{error, detail}` 字段结构。

#### 例外：二进制流端点

播放（`/songs/{id}/play`）、资源代理（`/proxy`）、封面图片（`/songs/{id}/cover`）等二进制流端点，错误可能返回纯文本（`http.Error`），因为客户端不期望 JSON body。

### 1.4 Content-Type 约定

**章节来源**: `internal/handlers/response.go`、`internal/app/routers.go`

默认 `application/json`。例外情况：音频播放（`audio/*`，支持 Range）、封面图片（`image/*`）、歌单导出与插件文件（`application/octet-stream`）、插件页面（`text/html`）、HLS 播放列表（`application/vnd.apple.mpegurl`）。上传接口（插件安装、歌单封面）使用 `multipart/form-data`。

### 1.5 分页

**章节来源**: `internal/handlers/music.go`

支持分页的列表接口使用 `limit`（默认 20，超出上限自动截断）和 `offset`（默认 0）查询参数。响应体包含集合名、`total`、`limit`、`offset` 四个字段。

---

## 2. 公开端点与认证端点

**章节来源**: `internal/app/routers.go`

路由注册分为两层：公开端点（无需认证）和授权端点（需 JWT）。

**公开端点**（无需 Bearer Token）：

| 端点 | 说明 |
|------|------|
| `POST /api/v1/auth/login` | 用户登录，获取令牌对 |
| `POST /api/v1/auth/refresh` | 用刷新令牌换取新的访问令牌 |
| `GET /api/v1/version` | 获取服务版本信息 |
| `GET /api/v1/health` | 健康检查 |
| `GET /api/v1/jsplugin/{entryPath}` | 插件静态页面（HTML） |
| `GET /api/v1/jsplugin/{entryPath}/static/*` | 插件静态资源（CSS/JS/图片） |
| `GET /api/v1/jsplugin-assets/*` | 插件公共资源（theme.css/components.css/common.js/字体） |
| 插件 `publicPaths` 声明的路径 | 插件 manifest 中声明的无需认证的 API 路径 |

**认证端点**（需 Bearer Token）：

除上述公开端点外的所有 `/api/v1/*` 端点均需认证。认证失败返回 `401 Unauthorized`。

---

## 3. API 分组索引

本索引核对 `internal/app/routers.go`、插件路由和 `docs/swagger.json`。当前 OpenAPI 包含 140 个路径、191 个方法；HEAD 不单列，动态插件路由使用占位路径。各路由表为当前源码快照，实际请求类型与字段以 Swagger 和对应 handler 为准。

| 模块 | 前缀 | 方法数 | 文档 |
| --- | --- | --- | --- |
| 歌曲 | `/api/v1/songs` | 38 | [歌曲](歌曲%20API.md) |
| 歌单 | `/api/v1/playlists` | 22 | [歌单](歌单%20API.md) |
| 播放历史 | `/api/v1/play-history` | 3 | [播放历史](播放历史%20API.md) |
| 认证 | `/api/v1/auth` | 5 | [认证](认证%20API.md) |
| 通用配置 | `/api/v1/configs` | 5 | [通用配置](配置与设置%20API.md) |
| 业务设置 | `/api/v1/settings` | 48 | [业务设置](配置与设置%20API.md) |
| 扫描与指纹 | `/api/v1/scan` | 10 | [扫描与指纹](扫描管理%20API.md) |
| 缓存 | `/api/v1/cache-manage` | 5 | [缓存](缓存管理%20API.md) |
| 插件管理 | `/api/v1/jsplugins` | 13 | [插件管理](插件管理%20API.md) |
| 插件运行时 | `/api/v1/jsplugin` | 9 | [插件运行时](插件管理%20API.md) |
| 插件公共资源 | `/api/v1/jsplugin-assets` | 2 | [插件公共资源](插件管理%20API.md) |
| 插件健康度 | `/api/v1/plugins` | 1 | [插件健康度](插件管理%20API.md) |
| 自定义标签 | `/api/v1/song-tags` | 9 | [自定义标签](标签与主题%20API.md) |
| 主题包 | `/api/v1/theme-packs` | 9 | [主题包](标签与主题%20API.md) |
| 后端升级 | `/api/v1/upgrade` | 7 | [后端升级](系统管理%20API.md) |
| 资源与转码代理 | `/api/v1/proxy` | 2 | [资源与转码代理](系统管理%20API.md) |
| 日志导出 | `/api/v1/logs` | 1 | [日志导出](系统管理%20API.md) |
| 健康检查 | `/api/v1/health` | 1 | [健康检查](系统管理%20API.md) |
| 版本 | `/api/v1/version` | 1 | [版本](系统管理%20API.md) |

开发模式的交互式文档：`/swagger/index.html`；静态分组参考见[Swagger API](../../swagger-api/index.md)。
