# Reasonix 插件包

Reasonix 插件包把 skills、MCP servers、prompts、主题和代码型扩展组织成一个可安装单元。

## CLI 模式

在终端里使用 `reasonix plugin` 安装和管理插件包。插件包当前按全局范围安装，
写入 Reasonix home 目录。

### 通过 CLI 安装

`install` 接收一个来源：

- GitHub 仓库，例如 `git:github.com/obra/superpowers` 或
  `https://github.com/obra/superpowers`。
- GitHub 分支或子目录 URL，例如
  `https://github.com/owner/repo/tree/main/path/to/plugin`。
- 本地目录，目录内需要包含 `reasonix-plugin.json`、
  `.codex-plugin/plugin.json` 或 `.claude-plugin/plugin.json`。

只预览安装计划，不写文件：

```bash
reasonix plugin install git:github.com/obra/superpowers --dry-run
```

确认计划后安装：

```bash
reasonix plugin install git:github.com/obra/superpowers --yes
```

指定安装名称，或覆盖已安装的同名插件：

```bash
reasonix plugin install git:github.com/obra/superpowers --name superpowers --replace --yes
```

以开发模式使用本地目录：

```bash
reasonix plugin install /path/to/plugin --link --replace --yes
```

CLI 安装参数：

- `--dry-run` 只规划和校验安装，不写文件。
- `--yes` 用于确认执行会写文件的安装。
- `--replace` 允许当前来源替换已安装的同名插件。
- `--name <name>` 或 `--name=<name>` 覆盖插件 manifest 里的名称，
  作为本次安装名称。
- `--link` 链接本地插件目录，而不是复制到 Reasonix 的插件存储目录。
  移动或删除该目录会导致这个链接插件失效。

如果运行 `reasonix plugin install <source>` 时既没有 `--dry-run`，
也没有 `--yes`，CLI 会拒绝写文件，并提示使用其中一个参数重新运行。
安装和移除命令会输出结构化 JSON，来源于桌面端同一套 install-source 后端。

插件状态和内容写入：

```text
~/.reasonix/plugin-packages.json
~/.reasonix/plugins/<name>/
```

### 通过 CLI 管理

列出已安装插件：

```bash
reasonix plugin list
```

查看某个插件的元数据、根目录、来源以及导出的能力数量：

```bash
reasonix plugin show superpowers
```

如果能读取到能力明细，`show` 也会输出具体清单：

- **skills** 会展示建议的 `/<插件名>:<技能名>` 调用方式和描述。
- **commands** 会展示 `/<插件名>:<命令名>` 调用方式、参数提示和描述。
- **mcpServers** 会展示服务器名称、传输方式和启动目标。

检查 manifest 和 skill roots 是否可读：

```bash
reasonix plugin doctor superpowers
```

工作区级能力总览（skills / MCP 合并 / 包根目录）见
[能力诊断](./CAPABILITY_DIAGNOSTICS.zh-CN.md)：

```bash
reasonix doctor capabilities --json
# 桌面端：设置 → 诊断
# Agent：  /reasonix-guide
```

在不卸载的情况下启用或禁用插件：

```bash
reasonix plugin disable superpowers
reasonix plugin enable superpowers
```

移除插件：

```bash
reasonix plugin remove superpowers --yes
```

`remove` 也可以写成 `uninstall`。它需要 `--yes`，
因为会写入状态并删除复制安装的插件内容。如果是链接模式安装的本地插件，
外部源目录会保留。

### 在 CLI 中使用已安装插件

已安装插件不会打开一个独立聊天界面。插件启用后，Reasonix 会把它的能力加载到普通交互会话里：

- 在交互会话里运行 `/plugins` 可以列出已安装插件包。
  运行 `/plugins show <name>` 可以在不离开聊天的情况下查看该插件导出的
  skills、MCP servers 和使用提示。
- **Skills** 会出现在 `/skills` 中。可以用 `/<插件名>:<技能名> [args]` 直接调用，
  也可以自然描述任务，让 agent 按 description 选择匹配的 skill。
- **MCP servers** 会进入正常 MCP/工具流程。用户只需要描述任务，
  Reasonix 会在相关时调用插件提供的工具。

如果是在另一个终端里安装、启用、禁用或更新插件，而当前已有 `reasonix` 会话正在运行，
建议开启新会话，或重新打开 `/skills` 确认当前会话能看到预期技能。

## 桌面端设置

打开 **设置 -> 插件**，可以不用 CLI 直接安装和管理插件包。

### 安装插件

安装区有两种模式：

- **本地目录**：点击 **选择插件目录**，从磁盘选择一个插件目录。
  选中路径会显示在按钮右侧。
- **Git 仓库**：填写 Git 来源，例如 `git:github.com/obra/superpowers`。
  **安装名称（可选）** 可覆盖插件 manifest 声明的名称，用于本次安装或覆盖。

选择来源和选项后，再使用操作按钮：

- **预检** 校验来源并展示计划安装动作，不写入文件。
- **安装插件** 按当前来源和选项执行安装。
- **刷新插件** 从磁盘和配置重新读取已安装插件列表。

安装选项：

- **覆盖同名插件** 允许当前来源替换已安装的同名插件。关闭时，同名安装会失败，
  而不是覆盖已有内容。
- **开发模式：链接源目录** 只在 **本地目录** 模式出现。它不会复制插件，
  而是直接链接所选目录；适合开发或调试插件。移动或删除该目录会导致这个链接插件失效。

对新的 Git 来源或本地插件目录，建议先点 **预检**。

### 管理已安装插件

已安装插件列表会展示每个插件包以及它导出的 skills 和 MCP servers。
通过应用外编辑插件文件或配置后，可点 **刷新插件** 重新读取。

展开插件行后可以：

- 启用或禁用插件。
- 查看 **使用方法**，了解该插件导出的 skills 和 MCP servers。
- 使用 **更新** 拉取或刷新具备更新来源的插件。
- 使用 **诊断** 检查插件 manifest，并查看警告或诊断信息。
- 使用 **移除插件**，确认后卸载该插件包。

### 在桌面端使用已安装插件

桌面端设置页和 CLI 使用同一套运行模型：

- 展开已安装插件，可以看到 **使用方法** 区域。
- 在任意桌面会话里输入 `/plugins` 可以列出已安装插件；
  输入 `/plugins show <name>` 可以直接从聊天界面查看同一套使用详情。
- Skills 会展示带插件名的直接命令，例如 `/superpowers:writing-plans`；
  在会话中也可以通过 `/skills` 浏览。
- 插件命令统一以带插件名的形式展示和调用，例如 `/superpowers:plan`。
- MCP servers 作为透明能力清单展示，通过普通工具调用流程可用。
- 如果当前打开的会话没有反映插件变更，刷新插件列表并开启新会话。

## 原生 Manifest

Reasonix 原生插件在根目录声明 `reasonix-plugin.json`：

```json
{
  "name": "example",
  "version": "1.0.0",
  "description": "Example plugin",
  "skills": "skills",
  "mcpServers": {
    "helper": {
      "command": "bin/helper"
    }
  }
}
```

相对路径都按插件根目录解析。Reasonix 安装插件时不会执行第三方安装脚本。

## Manifest v2（扩展）

Reasonix 原生扩展使用精确的 v2 `apiVersion`：

```json
{
  "apiVersion": "reasonix.io/plugin/v2",
  "name": "example",
  "version": "1.0.0",
  "description": "Example extension",
  "requires": [],
  "provides": [
    {
      "namespace": "plugin/example",
      "kind": "interceptors",
      "id": "default",
      "version": "1.0.0"
    }
  ],
  "contributes": {
    "skills": ["skills"],
    "agents": ["agents"],
    "commands": ["commands"],
    "prompts": ["prompts"],
    "mcpServers": {},
    "themes": ["themes/*.reasonix-theme"]
  },
  "runtime": {
    "command": "${REASONIX_PLUGIN_ROOT}/bin/example",
    "args": [],
    "env": {},
    "required": true,
    "priority": 0,
    "intercepts": ["input.receive", "tool.before"],
    "replaces": [],
    "capabilities": ["interceptors"]
  }
}
```

解析规则：

- 原生 `reasonix-plugin.json` 必须声明精确值 `reasonix.io/plugin/v2`。
  v1 与缺失版本都会被拒绝；不提供 v1 双读或自动迁移路径。
- v2 是严格的：根对象或 `contributes`/`runtime` 下的任何未知字段都会
  报错并指明字段路径，避免拼写错误静默失效。
- v2 的资源发现是显式的。Reasonix 只加载原生 manifest 中声明的 skills、
  agents、commands、prompts、MCP servers、themes 与 runtime；不会隐式
  导入根目录 `CLAUDE.md`、`.claude/settings.json` 或
  `.mcp.json` 等宿主专用 sidecar。
- minor 别名（如 `reasonix.io/plugin/v2.0`、`v2.1`）及未知 major version
  都会被拒绝。
- `requires` 与 `provides` 声明依赖约束和能力上限；Sidecar handshake
  不能超出该上限。
- v2 可以同时使用受支持的顶层资源字段（`skills`、
  `mcpServers` 等）与
  `contributes`：完全相同的路径去重；同名但定义不同的条目报 Manifest
  错误并指明键名。
- 所有相对路径与 glob 必须位于插件根目录内：拒绝路径穿越、绝对路径、
  逃逸 symlink 和非普通 theme 文件。

新资源类型：

- `prompts` 使用与 commands 相同的模板语义和参数替换，公开名为
  `/<plugin>:<name>`；`commands` 保持兼容别名。
- `themes` 是 `.reasonix-theme` 文件，在 Desktop 设置中以只读插件主题
  展示（ID 为 `plugin:<plugin>:<theme>`），不会复制进用户主题库。插件
  被禁用或卸载时，若当前使用的是它的主题，界面回退到基础样式但保留该
  ID，重新安装同一插件后自动恢复。

`runtime` 块声明的是代码型扩展——由 Reasonix 启动并通过 Extension
Protocol（基于 stdio 的 JSON-RPC 2.0，方法索引见
`docs/EXTENSION_PROTOCOL.generated.md`，Go SDK 见 `sdk/go/README.md`）
驱动的 Sidecar 进程：

- `command`/`args`/`env` 仅支持 **exec form**：command 即可执行文件，
  绝不经过 Shell 解释；`${REASONIX_PLUGIN_ROOT}` 展开为插件安装根目录。
- `intercepts` 声明要拦截的事件（如 `input.receive`、`tool.before`、
  `permission.decision`）；`replaces` 声明可以持有的替换槽
  （`system_prompt`、`context`、`provider_request`、`provider_response`、
  `compaction`、`session_policy`、`permission`、`frontend_events`、
  `tool:<name>`、`provider:<ref>`）。同一替换槽在所有已安装插件中只能有
  一个 owner，争用会导致构建失败并列出来源。
- `capabilities` 按能力族授权：`interceptors`、`strategies`、`providers`、
  `ui`。Sidecar 在握手时声明的任何超出 Manifest 的能力都会被拒绝。
- 扩展提供的模型以 `plugin/<plugin>/<provider>/<model>` 形式出现在模型
  选择器中；该 ref 同样可用作 `default_model`（包括首次启动），并可
  在 `/model`、Desktop 与 ACP 的模型切换中使用。

**完全信任（Full trust）。** 代码型扩展运行在 Sandbox 之外，继承未过滤
的完整环境：它可以读取完整会话与环境、绕过权限、直接操作本机；扩展在
`permission.decision` 上的 "allow" 可以覆盖宿主的 deny。安装、更新、
替换或 `--link` 一个带有 `runtime` 块的插件**即代表授权**——不会有二次
确认，`--link` 模式在内容变化后自动保持信任。因此安装预览、
`reasonix plugin show`、能力诊断和 Desktop 安装界面都会显著展示
`FULL TRUST` 区块，列出 Runtime 命令、Interceptors、替换槽和
Provider/UI 能力。安装前请确认该区块内容，只安装你完全信任的运行时。
只有通过插件安装流程写入插件状态的 Runtime 才能启动；项目配置无法
声明代码型 Sidecar。

## Codex 与 Claude 兼容

Reasonix 也会读取 `.codex-plugin/plugin.json` 和 `.claude-plugin/plugin.json`。
安装预检会结构化显示“完全兼容 / 部分兼容 / 不兼容”、已映射能力和每个被跳过
的条目。非原生插件如果没有任何可映射能力，会直接阻止安装，不再留下“安装成功
但不可用”的记录。“完全兼容”指清单里声明的每个能力都成功解析并映射到了
Reasonix 的对应实现。GitHub 仓库若在
`.claude-plugin/marketplace.json` 中通过 `./plugins/example` 或
`plugins/example` 这类相对字符串列出多个插件，可以直接从仓库根目录安装；
预检会在写入前逐项展示安装动作。填写可选安装名称时，可只选择 marketplace
中的同名插件。对象来源仅接受 GitHub 仓库 URL 加完整 commit SHA；未固定版本的
外部字符串、npm、`strict: false` 以及其他高级 marketplace 协议在整库安装时会
跳过，按名称选中时则直接报错。
对于 Superpowers 和 Claude 风格 skill 包，Reasonix 会映射以下兼容约定。
原生 v2 manifest 只使用显式声明，不应用这些回退规则：

- `skills` 到 Reasonix skill root。Claude 清单若未声明 `skills` 字段，会回退到
  约定目录 `skills/`（或 `.claude/skills/`），与 Claude 自身的自动发现一致。
  插件 skill 统一以 `/<插件名>:<技能名>` 展示和调用。无歧义的 `/<技能名>`
  仍作为隐藏兼容别名接受输入；项目和用户 skill 保留短名称，多个插件导出的
  同名 skill 则只能通过各自的限定名称独立调用。这一用户侧命名空间不会改变
  模型 skill 索引或 `run_skill` 工具使用的内部短标识。
- `commands/`（以及 `.claude/commands/`）映射为 Reasonix 自定义斜杠命令：每个
  `<name>.md` 提示词模板统一以 `/<插件名>:<命令名>` 展示和调用，frontmatter 的
  `description` / `argument-hint` 以及 `$ARGUMENTS` / `$1..$N` 替换均生效。
  当短名称没有歧义时，`/<命令名>` 仍作为隐藏兼容别名接受输入，但不会出现在
  补全、帮助、桌面菜单、ACP 命令发现或提供给模型的命令清单中。用户和项目命令
  始终占有自己的短名称；多个插件导出同名命令时不会生成短名称别名。显式自定义
  命令也可以占用限定名称，Desktop 插件详情会报告该冲突。原生
  `reasonix-plugin.json` 清单也可以通过 `"commands"` 路径列表显式声明。
- `agents/*.md` 映射为插件所属、需要手动调用的子代理配置。Claude 模型别名会继承
  当前 Reasonix 模型；内联 `tools` 列表会转换为 Reasonix 工具名，并支持
  `mcp__*__search` 这类 MCP 通配符。Agent 使用独立的
  `/<插件>:agent:<名称>` 命名空间，因此上游 Agent 与 Skill 同名时不会互相遮蔽。
- 插件根目录 `.mcp.json` 会映射为已安装 MCP。Claude 的 `local` 会转换为 stdio；
  中文等显示名称会生成稳定内部 ID；重复声明会去重。导入服务器默认
  `auto_start=false`，由用户按需连接，避免启动时改变提供给模型的工具 schema。

## 桌面端后端方法

Desktop 通过 Wails 方法暴露插件包操作：

- `Plugins`
- `PlanPluginInstall`
- `InstallPlugin`
- `RemovePlugin`
- `SetPluginEnabled`
- `UpdatePlugin`
- `PluginDoctor`
