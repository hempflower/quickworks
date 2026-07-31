# Quickworks 管理面 UI 计划

状态：待 Phase 1 的仓库创建、代理与生命周期主流程稳定后实施。

## 目标

- 使用 Tailwind CSS 建立一致、紧凑且可访问的管理面视觉系统。
- 保持服务端渲染与少量原生 JavaScript；不引入 SPA、Node.js 运行时或客户端状态管理框架。
- 不改变现有 JWT、CSRF、工作区归属校验和 API 权限边界。

## 实施项

1. 构建与静态资源
   - 在 `web/package.json` 增加仅开发期的 npm 依赖 `tailwindcss` 与 `@tailwindcss/cli`，并提交 `web/package-lock.json` 固定版本；不将 `node_modules/`、npm cache 或 Tailwind 二进制提交到仓库。
   - Tailwind 输入为 `web/src/styles.css`，输出为 `web/static/app.css`；产物提交到仓库并由 Go embed 提供，因此运行 Quickworks 不需要 Node.js。
   - 增加构建命令、开发 watch 命令与 CI 中的产物一致性检查。
   - 定义颜色、间距、排版、状态色和深浅主题 token，优先满足 WCAG AA 对比度。

2. 应用外壳与认证
   - 实现响应式导航、用户菜单、登录中/错误页和全局通知区域。
   - 未登录 HTML 导航继续自动跳转 GitHub；API 的 401 JSON/文本语义保持不变。
   - 所有表单继续包含 CSRF token，错误消息以可访问的 alert 区域展示。

3. 工作区面板
   - 简洁 workspace 列表：显示名称、模板、CPU/内存分配、观测状态和打开、启动、停止、删除、失败重试操作。
   - 创建工作区抽屉或页面：空工作区、GitHub 仓库、模板选择与当前配额提示。
   - 对 pending、starting、running、stopping、stopped、failed、degraded、deleting、deleted 使用统一状态 badge 和明确文案。

4. 工作区详情与构建可见性
   - 增加详情页，包含生命周期操作、工作台入口、模板/资源摘要和 build 历史。
   - 使用现有 SSE 日志 API 实现增量日志视图，并为失败、等待 provisioner、lease 过期提供易理解的状态说明。
   - 不把错误详情、OAuth token、enrollment token 或其他 secret 渲染到 HTML/日志。

5. P3 调度与配额视图
   - 展示模板可选性、required labels、已匹配 provisioner、等待时间与无匹配 worker 的原因。
   - 展示按模板的 workspace/running 数量配额；CPU、内存只作为模板资源分配信息，不作为配额维度。
   - 管理员视图与普通用户视图分离前，不暴露其他用户或 provisioner 的敏感信息。

6. 验证
   - 为关键 HTML 页面、CSRF 表单、模板选择、状态显示和未登录重定向增加 handler 测试。
   - 使用浏览器级测试验证窄屏布局、键盘操作、色彩对比和 SSE 日志更新。
   - 运行 `CGO_ENABLED=0 go test ./...`、`go vet`、二进制构建及 Tailwind 产物检查。

## 非目标

- 不在本阶段引入 React/Vue、用户自定义主题、可视化模板编辑器或完整设计系统。
- 不用 UI 轮询替代 build、agent 和 provisioner 的真实状态机。

## 目标目录结构

```text
web/
├── package.json                 # 仅 CSS 构建工具与 npm scripts
├── package-lock.json            # 固定 npm 工具链版本
├── src/
│   └── styles.css                # Tailwind 输入、design token 与少量组件层样式
├── static/
│   └── app.css                  # 编译产物；由 Go embed，在运行时直接提供
└── templates/
    ├── layout.html               # 页面外壳、导航、通知与 CSS 引用
    ├── dashboard.html            # 工作区列表与创建入口
    ├── workspace.html            # 工作区详情、生命周期操作与 build 历史
    ├── repository.html           # GitHub 仓库确认与创建
    └── partials/
        ├── status_badge.html
        ├── quota.html
        └── build_logs.html
```

Go 侧由 `web/embed.go` 嵌入并提供 `/static/app.css`；路由、鉴权、workspace 状态机与 agent 隧道仍留在既有包中。模板不得直接访问数据库、OAuth token、provisioner token 或 agent enrollment token。

根 `.gitignore` 将补充 `web/node_modules/` 和 npm 调试日志规则。新增 Makefile 目标为 `ui-install`、`ui-build`、`ui-watch` 和 `ui-check`；后者重新构建 CSS 并检查工作树无差异，作为 CI 的验证步骤。
