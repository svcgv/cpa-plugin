# WorkBuddy 插件（CLIProxyAPI）

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 **腾讯 CodeBuddy**
（国内版 `copilot.tencent.com` + 国际版 `workbuddy.ai`）原生 OAuth 提供商插件：
按账号动态发现模型、流式执行、积分感知调度、CN 每日签到与成长奖励、内置管理面板。

[English → README.md](README.md)

## 功能

- **OAuth 登录** — 通过宿主 auth store 管理多账号 `workbuddy-<uid>.json`，
  CN 和 Global 共用一个插件、一份配置。
- **多区域添加账号** — 面板「添加账号」可选国内版、国际版（`workbuddy.ai`）
  或 CodeBuddy 国际版（`codebuddy.ai`）。国内版走现有 CodeBuddy CN 网关；国际版
  即本插件一直使用的 WorkBuddy 国际服务。多区可同时使用。
  区域由凭证 domain 决定（`cn` / `codebuddy-global` / `workbuddy-global`），
  路由、刷新与模型目录按区域隔离，不会跨区回退。
- **模型目录**：默认按已认证账号发现并缓存可用模型，也可以用 YAML 中的完整
  列表替代 WorkBuddy discovery。两种模式都会用 models.dev 补充缺失的 metadata。
  宿主侧 `oauth-model-alias` / `oauth-excluded-models` 配置仍然生效。
- **执行器** — OpenAI 兼容 chat completions，流式（真 SSE，走 `host.stream.emit`）
  和非流式（SSE 折叠成单个 completion）都支持。内置 `tool_choice` 归一、
  Claude Code 模板清洗、按区域注入 system message。
- **积分生命周期** — 账号耗尽时自动 `disabled` 并保留凭证，积分恢复后自动启用；
  只有确认所在服务**没有**签到活动（即额度为一次性 trial）的账号才会删除 auth 文件。
  能力或积分未知时不会删除。Executor 遇到硬积分错误立即触发 reconcile。
- **CN 每日签到与成长奖励** — 仅对 CN 账号执行每日签到、旅行礼物、派 Buddy、
  新任务与任务奖、断登补签、连登兑换、抽奖盲盒和能量开 Buddy。支持一键全领与分项操作；
  自动开关可分别关闭。北京时间 00:05 签到并领取，01/05/09/13/17/21:05 继续补领。
- **国际版隔离** — 两个国际服务的账号在本地筛选阶段直接跳过奖励、签到、补签、
  额度刷新及奖励调度附带的维护操作。手动 `/checkin` 同样只返回本地跳过。
- **Trial 领取** — 国际版（`workbuddy.ai`）账号可在面板领取一次性 250 积分专家加油包。
  CodeBuddy 国际版（`codebuddy.ai`）不显示该按钮。
- **积分面板** — 内嵌面板 `/v0/resource/plugins/workbuddy/panel`，含积分
  进度条、套餐徽章、耗尽/禁用标记、CN/Global 筛选、凭证导入，以及按服务标注的
  `CN` / `Global(CB)` / `Global(WB)` 徽章、CN 成长奖励按钮和领取记录。国际版不显示签到或奖励入口。
- **调度器**（可选） — `scheduler_mode: credits` 让插件选中面板选中的账号；
  `off`（默认）完全交给 CPA 内置调度。
- **Usage 上报** — 实现 `UsagePlugin` 能力，每条请求的 usage record 转发到
  可配置的 CPAMP 端点。未配置 URL+key 时不上报。

## 快速开始

### 1. 安装插件

把编译好的 `workbuddy.so` 放到 CPA 插件目录：

```bash
cp workbuddy.so /path/to/cliproxyapi/plugins/
```

多架构部署可用平台子目录约定：

```
plugins/
  linux/amd64/workbuddy.so
  linux/arm64/workbuddy.so
  darwin/arm64/workbuddy.so
```

### 2. 启用配置

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    workbuddy:
      enabled: true
```

### 3. 登录

从 CPA 侧边栏打开 WorkBuddy 面板（或直接访问
`/v0/resource/plugins/workbuddy/panel`），点 **添加账号**，选择区域后完成授权：

- **国内版（codebuddy.cn）** 走现有 CodeBuddy CN 网关。
- **国际版（workbuddy.ai）** 登录 WorkBuddy 国际服务（本插件历史上的「国际版」）。
- **CodeBuddy 国际版（codebuddy.ai）** 登录 CodeBuddy 国际服务，与上面是两个独立服务。

面板只接收会话 ID 和授权链接，令牌在后端交换并写入 auth store，不会回显到页面。
关闭弹窗或重新发起会取消上一个会话；旧会话的迟到响应不会覆盖当前登录。
每个账号登录一次，CN 与国际文件分别为 `workbuddy-<uid>.json` 与
`workbuddy-codebuddy-global-<uid>.json`，因此同一 UID 在两个服务上可以并存。

CPA 自带 Auth 卡片仍可用，它只发起 CN 登录。

### 4. 调用

用任何映射到账号动态目录中模型的 alias 调 OpenAI 兼容端点：

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "client-alias",
    "messages": [{"role": "user", "content": "hi"}],
    "stream": true
  }'
```

## 配置项

全部字段可选，位于 `plugins.configs.workbuddy` 下。

```yaml
plugins:
  configs:
    workbuddy:
      enabled: true

      # 可选的完整 model ID 列表，每项必须是单行 YAML string。
      # 非空列表就是全部模型：跳过 WorkBuddy catalog HTTP 和 catalog cache
      # 读写，但 models.dev metadata 的 fetch、ETag 和 last-good cache 规则不变。
      # 未设置、null 或 [] 时继续动态发现 WorkBuddy catalog。
      models: []

      # WorkBuddy 插件发起的全部 HTTP 请求可使用独立代理。
      # 支持 http、https、socks5、socks5h。
      # 空值或未设置时继承现有 CPA 路由；配置无效或代理运行失败时
      # fail closed，不会回退到 CPA 全局代理或直连。
      proxy-url: ""

      # 旧版签到开关；只在没有 rewards.actions.checkin 时控制签到。
      checkin_auto: true

      # 默认全部开启；国际版永不执行。account_key 从 /rewards/status 的
      # accounts 映射获取，可在面板「奖励设置」中保存。
      rewards:
        auto: true
        actions:
          checkin: true
          travel: true
          tasks: true
          makeup: true
          redeem: true
          lottery: true
          energy: true
        # accounts:
        #   cn-<hash>:
        #     auto: false
        #     actions:
        #       lottery: false

      # 积分生命周期：CN 耗尽禁用 / Global 耗尽删除 / CN 回血恢复（默认 true）。
      lifecycle_auto: true

      # 调度行为（默认 "off"）：
      #   off     → 完全交给 CPA 内置调度
      #   credits → 插件选中面板选中的账号（耗尽/禁用时回退）
      scheduler_mode: "off"

      # CPAMP usage 上报。URL+key 都设置才会上报。
      # 未配置时 fallback 到 USAGE_REPORT_URL / USAGE_REPORT_KEY /
      # CPAMP_ADMIN_KEY 环境变量或 docker secret 文件。
      usage_report_url: "http://cpa-manager-plus:18317/v0/management/usage/import"
      usage_report_key: ""

      # 插件层 management 鉴权。设置后所有 /v0/management/plugins/workbuddy/*
      # 写端点要求该 Bearer token。空（默认）则只靠宿主 management middleware。
      # 也可从 WB_MANAGEMENT_KEY 环境变量读。
      management_key: ""
```

模型 alias 和排除走 CPA 原生 `oauth-model-alias` 和 `oauth-excluded-models`
配置，无需插件侧重复。

设置 `proxy-url` 后，chat、billing/签到/成长奖励/trial、token refresh、
`executor.http_request`、usage 上报、OAuth state/token/account 请求和 usage
endpoint 探测都会使用该代理。CLIProxyAPI v7.2.30 的 native plugin host HTTP
API 不支持单次请求覆盖代理，因此显式插件代理由插件自身发送，不会出现在
CPA request-log 中。浏览器打开的 OAuth URL 不由插件请求，浏览器需要自行具备
相应网络路径。

## 模型目录

`model.static` 只是离线 fallback contract，只返回带通用 default metadata 的
`auto`，不读账号 cache，也不发网络请求。

`models` 是非空单行 YAML string sequence 时，它按 YAML 顺序定义完整模型列表。
`model.for_auth` 仍会校验账号，但不会请求 WorkBuddy catalog HTTP，不会读写账号的
WorkBuddy catalog cache，也不会删除已有 cache。models.dev metadata 仍执行原有的
在线 fetch、ETag、持久化和 last-good fallback。metadata fresh 时账号进入 `ready`；
刷新失败但有有效 metadata cache 时进入 `stale`；没有有效 metadata 时进入 `failed`
并返回空模型列表。未设置 `models`、`models: null` 和 `models: []` 都会恢复动态发现，
并可继续使用已有 WorkBuddy catalog cache。

动态模式下，每个账号第一次调用 `model.for_auth` 时执行 authenticated bootstrap：

1. WorkBuddy `GET /v3/config` 提供该账号有权使用的 model ID 和 serving 字段。
   只有该端点明确返回 HTTP 404 或 405 时，才 fallback 到旧端点
   `GET /console/enterprises/personal/models`；其他错误不会触发 fallback。
2. [models.dev `/models.json`](https://models.dev/models.json) 只补充 WorkBuddy
   缺失的 canonical metadata，不决定账号 entitlement，也不覆盖 WorkBuddy
   提供的 serving 字段。
3. 每项来源都先校验，再替换 persistent cache，最后发布 immutable 的账号模型目录。

Cache 根目录由 `os.UserConfigDir()` 计算，不硬编码平台路径：

```plaintext
<user-config-dir>/CLIProxyAPI/workbuddy/model-catalog/
  metadata.json
  metadata.json.bak
  models/
    <identity-sha256>.json
    <identity-sha256>.json.bak
```

Linux root 的默认路径是
`/root/.config/CLIProxyAPI/workbuddy/model-catalog/`。

首次 bootstrap 没有有效 cache 时 fail closed：两个来源必须都成功完成获取、
校验和持久化，账号才能进入 `ready`。Bootstrap 失败时，`model.for_auth`
返回成功 envelope 和空模型列表。之后每次启动进程时，第一次调用仍会尝试刷新
两个来源。如果某项刷新失败，但该来源有有效 last-good cache，账号以 `stale`
启动并使用该 cache。某项来源既没有 fresh 结果，也没有有效 last-good cache 时，
账号进入 `failed`。

只有 `ready` 和 `stale` 可以执行。`not_started`、`loading`、`failed` 会在所有
executor 入口返回固定、脱敏的 `not_ready` 和 HTTP 503，scheduler 也会排除这些
账号。Panel 保持可访问，并通过 `model_status` 返回账号级来源和时间戳。固定错误
分类是 `auth_invalid`、`workbuddy_transport`、`workbuddy_http`、
`workbuddy_schema`、`models_dev_transport`、`models_dev_http`、
`models_dev_schema`、`cache_read`、`cache_write`；不会暴露上游原始错误、response
body、凭证或 cache 路径。

当前没有 background 或 request-time refresh、panel retry endpoint、Enterprise
custom model source。新进程启动或 auth、token、plugin config generation 变化时，
才有下一次 bootstrap 机会。

## 生命周期

| 状态 | CN 账号 | 国际版账号 |
|---|---|---|
| 积分 > 0 | active | active |
| 积分 = 0 | `disabled: true`（凭证保留） | auth 文件**删除** |
| 积分回血 | 自动恢复启用 | n/a（已删） |
| Trial 可领 | n/a | 仅旧 WorkBuddy 国际版，每账号一次 |
| 能力或积分未知 | 不动（永不误杀） | 不动 |

判断依据是该账号所在服务的签到活动：CN 账号始终保留；国际版只有确认没有
签到活动时才删除。

Executor 遇到硬积分错误（402、"insufficient credits"、"积分不足" 等）
会立即触发该账号的 reconcile。

## 开发

需要 Go 1.26+（与 CPA 一致）。

```bash
# 编译插件
go build -buildmode=c-shared -o workbuddy.so .

# 跑测试
go test -race ./...

# Lint
gofmt -l .
go vet ./...
```

`proxy-url` 为空时，共享请求 helper 保持现有路由：使用宿主桥的请求继续进入
CPA request-log 并应用宿主 transport 策略；既有 OAuth、usage probe、Windows
和旧宿主直连路径不变。设置 `proxy-url` 后，插件发起的全部 HTTP 请求改用插件
代理，失败时不回退。

完整开发流程见 [docs/development.md](docs/development.md)，模块结构见
[docs/architecture.md](docs/architecture.md)。

## License

MIT — 见 [LICENSE](LICENSE)。
