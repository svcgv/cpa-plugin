# QoderWork CPA 插件

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 QoderWork Provider 插件：**国内版（qoder.com.cn）+ 国际版（qoder.com）双区域**、多账号 OAuth/PAT 双登录、动态模型、COSY / Bearer 双协议推理、每日奖励、积分面板、token 自动保活。

## 功能

| 能力 | 说明 |
|---|---|
| **双登录方式** | ① OAuth 设备授权（PKCE，浏览器授权，dt- 30 天 + drt- 1 年自动旋转）② PAT 导入（pt-，长期有效兜底）——两家族可共存于同一 auth 文件 |
| **面板 OAuth 登录** | 面板「OAuth 登录」弹窗可选 **国内版 qoder.com.cn** 或 **国际版 qoder.com**：显示授权链接、可复制/打开、可取消、可重试；登录成功后自动刷新该账号状态。两区域可共存于同一实例 |
| **Pro 领取** | 面板手动「领取 Pro」：先查 eligibility，可领才 claim 一次性 Pro 升级包（+1800），不自动领取 |
| **双区域推理** | CN：COSY 签名（RSA 包 AES 会话密钥 + MD5 请求签名）对接 gateway.qoder.com.cn SSE；国际版：`Authorization: Bearer {jt-}` 直连 api2-v2.qoder.sh 的 OpenAI `/model/v1/chat/completions` |
| **动态模型** | CN：COSY 拉取 `/algo/api/v2/model/list?Encode=1`（bodyless GET，空 body 签名），按 `assistant` → `chat` → `inline` scene 解析（对齐 Qoder CLI 1.1.69）；国际版：同一 COSY scene 目录，网关为 `api2.qoder.sh/algo/api/v2/model/list`（Bearer `/model/v1/models` 仅作兜底）。缓存按账号隔离；各自有静态兜底，失败原因可在 `/plugins/qoderwork/models/status` 查看 |
| **每日奖励** | 仅国内版：读取活动列表并领取当前窗口内的 `CLAIMABLE` Credits，面板支持单账号、批量与自动领取；北京时间 10:05、11:05、15:05、19:05、23:05 查询。仅活动接口返回 404/405 时回退旧签到接口 |
| **积分面板** | 账号卡片：昵称/区域徽标/积分/计划/领取状态/操作（CN 显示每日奖励+领取Pro；国际版仅刷新/选用） |
| **token 保活** | 22:00 定时刷新；按 token 前缀路由（drt- → deviceToken/refresh，jrt- → jobToken/refresh），刷新端点按区域选择（qoder.com.cn / qoder.sh）；PAT 永不劫持 OAuth 刷新 |
| **auth 隔离** | 文件名前缀 `qoderwork-` 过滤，与 workbuddy 等其他插件互不干扰 |

## 安装

### 从 Release（推荐）

```bash
# 按你的平台下载（示例 linux/arm64）
unzip qoderwork_0.2.6_linux_arm64.zip
cp qoderwork.so /path/to/cliproxyapi/plugins/qoderwork.so
```

### 从源码

```bash
cd qoderwork
CGO_ENABLED=1 go build -buildmode=c-shared -ldflags "-X main.version=0.2.6" -o qoderwork.so .
cp qoderwork.so /path/to/cliproxyapi/plugins/
```

### config.yaml

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    qoderwork:
      enabled: true

      # 仅 CN；国际版永不执行。account_key 可从 /rewards/status 获取。
      rewards:
        auto: true
        actions:
          checkin: true

# 模型别名（可选）
oauth-model-alias:
  qoderwork:
    - name: qmodel_preview
      alias: qoder/qwen3.8-max
    - name: qmodel_latest
      alias: qoder/qwen3.7-max
```

## 使用

### 方式一：OAuth 登录（推荐）

在插件面板点 **OAuth 登录**，弹窗会给出授权链接：

1. 点「打开」或在浏览器访问该链接 → 登录 qoder.com.cn（阿里云 SSO）→ 点 Continue 授权
2. 插件自动轮询拿 token 落盘（dt-/drt-），成功后立即刷新该账号卡片
3. 关闭弹窗或重新发起会取消上一个会话；旧会话的迟到结果不会覆盖新登录

PAT 与新 token 家族可共存：重新用 OAuth 登录同一账号时，已存在的 `pt-` PAT 会保留，
继续作为刷新兜底。

也可以继续用 CPA 自带的 QoderWork OAuth 登录卡片，两者等价。

### 方式二：PAT 导入（仅国内版）

1. qoder.com.cn → 设置 → Personal Access Token → 创建（pt- 开头）
2. 插件面板（`/v0/resource/plugins/qoderwork/panel`）→ 右上角「导入 QoderWork 凭证」→ 粘贴 PAT → 导入
3. 插件自动换 jobToken（jt-/jrt-）落盘

### 面板

`/v0/resource/plugins/qoderwork/panel`（需 management key）

- 账号卡片：套餐内 Credits 与资源包分行显示（不再把资源包余额当成账户可用积分）/ 计划 / 每日奖励按钮（领取后返回最新积分）
- 「一键领取全部」批量领取；「奖励设置」可保存自动开关与账号覆盖
- 领取记录显示逐项结果；国际版不显示奖励操作，也不会发起奖励请求

## 凭证家族与刷新

```
auth 文件字段（可共存）：
  accessToken:   dt- (OAuth, ~30d) 或 jt- (PAT 交换, 24h)
  refreshToken:  drt- (~1y, 旋转)   或 jrt- (48h)
  personalToken: pt- (长期兜底，导入即永久)
```

- 刷新路由按 `refreshToken` 前缀：`drt-` → `deviceToken/refresh`；`jrt-` → `jobToken/refresh` → 失败 fallback PAT re-exchange
- `personalToken` 永不主动覆盖活跃 token，只做最终兜底
- host 15 分钟 auto-refresh + 插件 22:00 keepalive 均按此规则

## 模型

10 个静态模型（`qmodel_preview` 等）+ COSY 动态拉取。CPA 侧别名示例：`qoder/qwen3.8-max` → `qmodel_preview`。

动态拉取细节（对齐官方客户端 / cockpit-tools）：

- 请求 `GET https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1`，**无请求体**，
  COSY 签名覆盖空 body（`Cosy-Bodyhash`/`Cosy-Bodylength` 随之置空值）。
  旧实现签的是 `qoderEncode("{}")` 的 body 但发送时又不带 body，签名与线上 body 不一致，
  网关直接返回 `403 {"code":"101","message":"Signature invalid"}`，于是模型列表静默退回静态兜底 —— 这正是「登录后拉取模型异常」的根因。
- 兼容兜底：依次尝试 `?Encode=1`（空 body 签名）、`?Encode=1` + 编码 body、`POST` + 编码 body，
  并把响应按「明文 JSON / QoderEncoding / `{code,data}` 信封」三种形态解析。
- 客户端身份：`Cosy-Machinetype=5`、`Cosy-Machinetoken=Cosy-Machineid`、`Cosy-Machineos=<arch>_<os>`；
  若本机存在官方客户端的 `SharedClientCache/cache/machine_token.json` 与 `cache/id`，优先使用其中的
  机器身份与 `Cosy-Version`（cockpit-tools 同款做法）。OAuth 设备授权会持久化 `machineId`，
  让授权时用的机器标识与后续 COSY 请求保持一致。
- 诊断接口：`GET /plugins/qoderwork/models/status` 返回上次拉取来源（dynamic/static）、数量、
  以及每个账号的失败原因；`POST /plugins/qoderwork/models/refresh` 清缓存并立即重拉。
- 失败退避 2 分钟，避免上游故障时每次 `model.static`/`model.for_auth` 都打满请求。

## 区域（国内版 / 国际版）

两个区域是**完全独立**的服务，账号不互通：

| | 国内版 CN | 国际版 INTL |
|---|---|---|
| 站点 | qoder.com.cn | qoder.com |
| OpenAPI | openapi.qoder.com.cn | openapi.qoder.sh |
| 推理网关 | gateway.qoder.com.cn | api2-v2.qoder.sh |
| 推理协议 | COSY 签名 + agent_chat_generation | `Bearer {jt-}` + OpenAI `/model/v1/chat/completions` |
| 设备授权回调 | `qoder-work-cn://` | `qoder://aicoding.aicoding-agent/login-success` |
| 模型清单 | `/algo/api/v2/model/list`（COSY） | `api2.qoder.sh/algo/api/v2/model/list`（COSY；`/model/v1/models` 兜底） |
| 签到、每日奖励 / 积分 / Pro 包 | 有 | 无（奖励流程本地跳过） |

区域在登录时选择并持久化到 auth 文件（`region` 字段，兼容旧的 `domain` 推断）。
同一实例可同时挂 CN 与国际版账号：面板分别打 `CN` / `INTL` 徽标并按区域筛选，
`model.for_auth` 永远只返回该账号所属区域的模型，`model.static` 在混合场景返回并集。

## 参考文档

- [KNOWLEDGE.md](../KNOWLEDGE.md) — QoderWork API 逆向全记录（COSY 签名/编码/端点）
- [analysis/api-endpoints-scan.md](../analysis/api-endpoints-scan.md) — 客户端全端点扫描
- [analysis/qoderwork-real-oauth.md](../analysis/qoderwork-real-oauth.md) — OAuth 设备授权流程 + 本地测试证据

## License

MIT（based on workbuddy by lovingfish，见 [LICENSE](LICENSE)）
