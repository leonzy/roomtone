import { relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'
import AutoSidebar from 'vite-plugin-vitepress-auto-sidebar';
import taskLists from 'markdown-it-task-lists'
import llmstxt from 'vitepress-plugin-llms'

// GitHub Pages 项目站点部署在 /roomtone/ 子路径下。VitePress 会自动给 markdown 链接
// 和主题资源加上 base，但 head 里的裸路径不会，所以下面统一拼 BASE。
const BASE = '/roomtone/'

export default async () => {
  return withMermaid(defineConfig({
    base: BASE,
    title: "Roomtone",
    description: "Roomtone - 自托管个人音乐服务器，支持 JS 插件扩展、Flutter 与 Lynx 客户端",
    lang: 'zh-Hans',

    // 中英双语：中文为根（/），英文在 /en/。目前仅落地页（index.md / en/index.md）
    // 做了双语；文档正文暂保持中文，英文导航指向中文文档（后续再补译）。
    locales: {
      root: {
        label: '简体中文',
        lang: 'zh-Hans',
      },
      en: {
        label: 'English',
        lang: 'en',
        link: '/en/',
        themeConfig: {
          nav: [
            { text: 'Get Started', link: '/en/quick-start' },
            { text: 'Clients', link: '/en/clients' },
            {
              text: 'Plugins',
              items: [
                { text: 'Plugin List', link: 'https://github.com/songloft-org/songloft/issues/4' },
                { text: 'Plugin Dev Guide', link: '/en/js-plugin-development-guide' },
                { text: 'Plugin Registry Guide', link: '/en/plugin_registry' },
                { text: 'Theme Pack Guide', link: '/en/theme-pack-guide' },
              ],
            },
            { text: 'RepoWiki', link: '/en/repowiki/项目概述' },
            { text: 'FAQ', link: '/en/faq' },
                { text: 'Changelog', link: 'https://github.com/leonzy/roomtone/releases' },
            {
              text: 'More',
              items: [
                { text: 'API Docs', link: '/swagger-api/' },
                { text: 'Lynx Client', link: '/en/player-lynx/' },
                { text: '容器镜像', link: 'https://github.com/leonzy/roomtone/pkgs/container/roomtone' },
                { text: 'Privacy', link: '/en/PRIVACY' },
                { text: 'NOTICE', link: '/en/NOTICE' },
              ],
            },
          ],
        },
      },
    },

    head: [
      ['link', { rel: 'icon', type: 'image/svg+xml', href: `${BASE}favicon.svg` }],
      ['meta', { property: 'og:type', content: 'website' }],
      ['meta', { property: 'og:title', content: 'Roomtone - 自托管个人音乐服务器' }],
      ['meta', { property: 'og:description', content: '简单、自由、插件化的个人音乐服务器，支持 JS 插件扩展' }],
      ['meta', { property: 'og:image', content: 'https://leonzy.github.io/roomtone/logo.png' }],
    ],

    themeConfig: {
      logo: '/logo.png',

      nav: [
        { text: '快速开始', link: '/quick-start' },
        { text: '客户端', link: '/clients' },
        {
          text: '插件',
          items: [
            { text: '插件列表', link: '/issues/4' },
            { text: '插件开发指南', link: '/js-plugin-development-guide' },
            { text: '插件源制作指南', link: '/plugin_registry' },
            { text: '插件工具链', link: '/plugin-toolchain/' },
            { text: '主题包制作指南', link: '/theme-pack-guide' },
          ],
        },
        { text: '源码解析', link: '/repowiki/项目概述' },
        { text: 'FAQ', link: '/faq' },
        { text: '更新日志', link: '/changelog' },
        {
          text: '更多',
          items: [
            { text: 'API 文档', link: '/swagger-api/' },
            { text: 'Flutter 客户端', link: '/player/architecture' },
            { text: 'Lynx 客户端', link: '/player-lynx/' },
            { text: 'HA 加载项', link: '/addon/' },
                { text: 'Container Images', link: 'https://github.com/leonzy/roomtone/pkgs/container/roomtone' },
            { text: '隐私说明', link: '/PRIVACY' },
            { text: 'NOTICE', link: '/NOTICE' },
          ],
        },
      ],

      socialLinks: [
        { icon: 'github', link: 'https://github.com/leonzy/roomtone' },
      ],

      footer: {
        message: '源码基于 <a href="https://github.com/songloft-org/songloft/blob/main/LICENSE">Apache 2.0</a> 协议开源',
        copyright: `Copyright © 2025-${new Date().getFullYear()} <a href="https://github.com/hanxi">涵曦</a>`,
      },

      search: {
        provider: 'local',
      },

      editLink: {
        pattern: 'https://github.com/leonzy/roomtone/issues',
        text: '在 GitHub 上提问',
      },
    },

    sitemap: {
      // VitePress 用 sitemap 包把页面路径（以 / 开头）解析到 hostname 上，
      // 前导斜杠会把 base 吃掉，所以 hostname 只写域名，子路径由 transformItems 补。
      hostname: 'https://leonzy.github.io',
      transformItems: (items) =>
        items.map((item) => ({
          ...item,
          url: `${BASE}${item.url}`,
          links: item.links?.map((link) => ({ ...link, url: `${BASE}${link.url}` })),
        })),
    },

    lastUpdated: true,

    markdown: {
      lineNumbers: false,
      // 关闭 attrs 语法：repowiki 的 REST 标题（如 `### GET /playlists/{id}`）
      // 结尾的 `{id}` 会被 attrs 当成空的自定义锚点导致 id 冲突。全站未使用 {#锚点}/{.class} 语法。
      attrs: { disable: true },
      config: (md) => {
        md.use(taskLists)
      },
    },

    vite: {
      plugins: [
        AutoSidebar({
          // 插件用 join(cwd, path) 拼接，需传相对路径而非绝对路径。
          path: relative(process.cwd(), fileURLToPath(new URL('..', import.meta.url))),
          ignoreList: [
            'node_modules', '.vitepress', 'dist', 'build',
            'public', 'scripts', 'components', 'assets',
          ],
          collapsed: true,
          titleFromFile: true,
          ignoreIndexItem: true, // 首页 index.md（落地页）不进侧边栏
        }),
        // 每页生成同名 .md 源文件 + 站点级 /llms.txt、/llms-full.txt，供「复制给 LLM」按钮
        // （CopyForLLM 组件）和外部 AI 抓取使用。
        llmstxt({}),
      ],
    },
  }))
}
