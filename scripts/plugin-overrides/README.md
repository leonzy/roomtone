# Roomtone 大曲库插件修正

补丁对应 `charce526/songloft-library-plus` 的 `2fefd1afb1ac342820a17560c619c615e47a7ae2` 和 `songloft-org/songloft-plugin-tag` 的 `bf97f7db5a96e0db0aafb6f3165e95b2379281b1`。保留原权限与配置；Plus 本地版本为 `0.2.16+roomtone.1`，Tag 为 `2.3.1+roomtone.3`。宿主只在远端版本更高时自动更新，同版本的构建标识不会触发覆盖；升级上游代码时仍应重新应用并验证补丁。

宿主增加可选 `songs.list({omitFingerprint:true})`，SQL 不读取指纹列，默认列表和单曲指纹读取保持兼容。Plus 每批读取 1000 首，在后台构建目录索引并提供进度，分段让出 VM；元数据缓存 15 分钟，写操作使缓存失效，外部修改可手动刷新。

标签刮削改为每页最多 100 首，通过宿主已有的歌曲分页、目录、ID 和批量查询接口读取当前页。目录树逐层展开，歌曲、校对及整理视图不再累积整库对象或一次渲染全部歌曲。校对分类和统计在服务端计算；客户端歌曲缓存最多 500 首，显式选中的歌曲单独保留。搜索、翻页和目录切换取消旧请求并忽略过期响应。目录缓存最多 16 项、30 秒，写操作使其失效。旧宿主目录接口仍会返回当前目录的直属歌曲，插件在返回浏览器之前截取当前页。

构建前在对应上游提交上运行 `git apply <补丁绝对路径>`，安装开发依赖，再执行 `pnpm exec tsc --noEmit`。验证使用 Plus builder/SDK 2.9.4、Tag builder/SDK 2.18.0。使用全新输出目录，避免重复打包旧哈希资源：

```sh
node --input-type=module -e "import {buildPlugin} from '@songloft/plugin-builder'; await buildPlugin({cwd:process.cwd(),outDir:'dist/roomtone-build'});"
node scripts/plugin-overrides/check-ui.mjs <Plus源目录> <Tag源目录>
```

通过宿主 `/api/v1/jsplugins/upload` 更新，保留旧 ZIP 以便回退，不直接修改数据库哈希或扩大权限。

2026-10-29 验证：Plus 4 项、Tag 10 项回归通过，包含完整 HTML 的 JSDOM 测试与服务端路由测试；使用 70,518 首的总量夹具，歌曲、校对和整理 DOM 均保持在 100 行以内，缓存上限、目录展开、搜索、跨页选择及过期响应均有断言。正式 leonFN 接口验证了根目录不同页、专辑目录、搜索、校对和统计，浏览器每页只接收最多 100 首。服务端查询仍可能需要数秒，不能将分页等同于查询瞬时完成。本次没有播放、刮削或写入音频标签。

前置宿主改动验证：元数据桥接测试通过；Windows 全插件测试有两项原有路径夹具失败，二者及新增测试在 leonFN Linux 容器通过。Plus 隔离全量索引约 75 秒、5,421 个节点，无 504；原音频以只读方式挂载。

2026-10-31：Tag 增加 `POST /scrape/prepare/:id`，复用指纹与文本刮削引擎，仅返回预览，不写标签、备份或已刮标记。返回具体的源错误、未配置和低匹配度原因，错误信息去除服务地址和凭证。歌曲信息对话框在用户确认后才调用宿主标签写入接口。插件 14 项、客户端相关回归及实际 NAS 只读请求验证通过；未修改真实歌曲标签，也未主动播放音频。
