# 隐私说明

> 最后更新：2026-06-10

Roomtone 是一款**自托管**软件，所有数据保存在你自己的服务器上。本文档说明软件本身的数据处理行为，便于你判断合规边界。

## 1. 遥测与监控上报

Roomtone 服务端集成了 [Tracely](https://github.com/hanxi/tracely) 监控 SDK。该功能为**编译时可选**——仅当构建时通过 `-ldflags` 注入了 `AppID`、`AppSecret` 和 `Host` 三个参数才启用。

- **GitHub Releases 提供的预编译二进制与 Docker 镜像**：已注入 Tracely 参数，**监控默认启用**。上报内容包括：崩溃时的 panic 堆栈信息、首次安装事件、版本升级事件、定时活跃心跳。**不含**用户数据、音乐文件或账号信息。
- **自行编译**（`make build` 不传 `TRACELY_APP_ID` / `TRACELY_APP_SECRET` / `TRACELY_HOST`）：监控**不启用**，服务端不会发出任何上报请求。

启动日志会明确显示 Tracely 的启用状态：
```
Tracely 监控未启用（编译时未注入 AppSecret/Host）   # 未启用
Tracely 监控初始化成功                              # 已启用
```

你可以通过抓包（tcpdump / Wireshark）或防火墙规则验证这一点。

除此之外，Roomtone 不内置任何匿名统计、用户行为分析或广告 SDK。

## 2. 主动出站请求清单

Roomtone 在以下场景会发起**主动**出站请求：

| 触发场景 | 请求目标 | 数据内容 |
|---------|---------|---------|
| 服务端发生 panic（仅编译时启用 Tracely 时） | 维护者自建的 Tracely 服务 | 错误堆栈、服务端版本号、平台信息。**不含**用户数据、音乐文件或账号信息 |
| 服务端首次启动或版本升级后启动（仅编译时启用 Tracely 时） | 维护者自建的 Tracely 服务 | 安装/升级事件、当前版本号、升级前版本号、平台信息。**不含**用户数据 |
| 用户在「设置」中点击「检查更新」 | `github.com/songloft-org/songloft` | 仅 HTTP GET version.json，不带任何用户标识 |
| 用户安装 / 启用 JS 插件并触发其网络能力 | 由该插件的代码决定（普通 HTTP 请求为默认宿主能力；UDP socket 受 `permissions: ["net"]` 沙箱权限约束） | 由插件实现决定 |
| 用户在 Web UI 中加载本仓库 README 中的徽章（如 visitorbadge.io） | `api.visitorbadge.io` | 仅 GitHub README 渲染时由 GitHub 服务端代理，不在 Roomtone 软件内 |

## 3. 数据存储位置

| 数据 | 位置 | 说明 |
|------|------|------|
| 用户账号 / 密码哈希 | `data/songloft.db`（SQLite） | bcrypt 哈希，不存明文 |
| JWT Token | 客户端本地（浏览器 LocalStorage / Flutter 端 secure_storage） | 服务端只保存 refresh token 的哈希 |
| 音乐元数据 / 封面 / 歌词 | `data/songloft.db` + `data/cache/` | 仅本地，不上传 |
| 播放记录 / 收藏 | `data/songloft.db` | 仅本地 |
| 插件配置 / 状态 | `data/songloft.db` 的 `plugin_storage` 表 | 插件通过沙箱 `storage` API 写入 |

**所有数据保存在你自己的部署环境内。** 项目方无法访问你的数据，因为根本没有任何"上报"链路。

## 4. JS 插件的数据收集

JS 插件**可能**通过宿主网络请求能力或其声明的 `permissions` 读取歌曲库、写入存储、使用 UDP socket 等能力。**第三方插件的数据收集行为完全由插件本身决定，与 Roomtone 主项目无关。**

- 插件的普通 HTTP 网络请求（`fetch`）为默认宿主能力；原始 UDP socket 需在 manifest 中显式声明 `permissions: ["net"]`，运行时由宿主在 QuickJS 沙箱中校验。
- 安装第三方插件前，请阅读其源码或权限清单，确认其网络请求范围是否符合你的预期。
- 如需禁止某个插件的网络访问，可在 Roomtone Web UI 中禁用该插件，或部署时在防火墙层屏蔽对应域名。

## 5. 若你将 Roomtone 部署给他人使用

如果你将 Roomtone 部署在公网或多用户环境，**你本人即成为《个人信息保护法》《GDPR》等法规下的"个人信息处理者"**，需自行：

- 向你的用户披露数据处理范围（账号、播放记录、IP 等）；
- 提供数据导出 / 删除途径；
- 保障传输与存储安全（HTTPS、磁盘加密等）。

项目方对此**不承担任何责任**，详见 [README 版权与免责声明](./README.md#️-版权与免责声明)。

## 6. 联系方式

如发现本文档与实际行为不符，请通过 [GitHub Issues](https://github.com/leonzy/roomtone/issues) 反馈。
