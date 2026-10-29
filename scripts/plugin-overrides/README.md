# Roomtone 大曲库插件修正

这两份补丁对应 `charce526/songloft-library-plus` 的 `2fefd1afb1ac342820a17560c619c615e47a7ae2` 和 `songloft-org/songloft-plugin-tag` 的 `bf97f7db5a96e0db0aafb6f3165e95b2379281b1`。保留原插件权限和配置，只调整元数据批量读取与页面加载；版本增加 `+roomtone.1`。插件更新会覆盖本地版本，升级新上游代码时应重新应用并验证补丁。

宿主增加可选 `songs.list({omitFingerprint:true})`，SQL 不读取指纹列。原默认行为和单曲指纹读取保持兼容。Plus 每批 1000 首，后台索引可轮询进度，构建分段让出 VM；元数据缓存 15 分钟，写操作继续触发失效，外部修改可手动刷新。标签刮削页面每批 1000 首，完成后一次替换列表，目录树在浏览器中生成。

构建前在相应上游提交上运行 `git apply <补丁绝对路径>`，安装仓库开发依赖，然后运行 `pnpm exec tsc --noEmit`。验证使用 Plus builder/SDK 2.9.4 和 Tag builder/SDK 2.18.0。用全新的输出目录避免旧哈希资源被重复打包：

```sh
node --input-type=module -e "import {buildPlugin} from '@songloft/plugin-builder'; await buildPlugin({cwd:process.cwd(),outDir:'dist/roomtone-build'});"
node scripts/plugin-overrides/check-ui.mjs <Plus源目录> <Tag源目录>
```

通过宿主 `/api/v1/jsplugins/upload` 更新，保留旧 ZIP 以便回退；不要直接更改数据库哈希或扩大权限。没有执行刮削、写标签或播放。

2026-10-29 验证：数据库测试、元数据桥接测试通过；Windows 全 JS 插件测试存在两项原有路径夹具失败，二者及新增测试在 leonFN 的 Linux 容器通过。Plus 原界面测试通过。隔离全量曲库实测：70,518 首，标签分页约 44 秒，Plus 目录索引约 75 秒、5,421 个节点，目录歌曲页能正确返回；期间无 504。首次建立索引仍需要时间，页面显示进度。隔离测试以只读方式挂载原音频。
