# QoderWork CPA 插件

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 QoderWork（qoder.com.cn，CN 区）Provider 插件：多账号 OAuth/PAT 双登录、动态模型、COSY 签名推理、每日签到、积分面板、token 自动保活。

## 功能

| 能力 | 说明 |
|---|---|
| **双登录方式** | ① OAuth 设备授权（PKCE，浏览器授权，dt- 30 天 + drt- 1 年自动旋转）② PAT 导入（pt-，长期有效兜底）——两家族可共存于同一 auth 文件 |
| **面板 OAuth 登录** | 面板「OAuth 登录」弹窗直接发起设备授权：显示授权链接、可复制/打开、可取消、可重试；登录成功后自动刷新该账号额度与签到状态。仅国内版 qoder.com.cn，不提供国际版选项 |
| **Pro 领取** | 面板手动「领取 Pro」：先查 eligibility，可领才 claim 一次性 Pro 升级包（+1800），不自动领取 |
| **COSY 推理** | RSA 包 AES 会话密钥 + MD5 请求签名，对接 gateway.qoder.com.cn SSE 流式 |
| **动态模型** | COSY 拉取 `/algo/api/v2/model/list`（bodyless GET，空 body 签名；兼容旧 `?Encode=1`/编码 body 形式），chat + inline scene，10 静态模型兜底；失败原因可在 `/plugins/qoderwork/models/status` 查看 |
| **每日签到** | 面板手动签到（单账号/批量）+ 09:00/21:00 定时自动签到，签到后返回最新积分快照 |
| **积分面板** | 账号卡片：昵称/积分/计划/签到状态/操作（签到/刷新/选用） |
| **token 保活** | 22:00 定时刷新；按 token 前缀路由（drt- → deviceToken/refresh，jrt- → jobToken/refresh），PAT 永不劫持 OAuth 刷新 |
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

### 方式二：PAT 导入

1. qoder.com.cn → 设置 → Personal Access Token → 创建（pt- 开头）
2. 插件面板（`/v0/resource/plugins/qoderwork/panel`）→ 右上角「导入 QoderWork 凭证」→ 粘贴 PAT → 导入
3. 插件自动换 jobToken（jt-/jrt-）落盘

### 面板

`/v0/resource/plugins/qoderwork/panel`（需 management key）

- 账号卡片：积分余额 / 计划 / 签到按钮（签到后返回最新积分）
- 「全部签到」批量签到；自动签到开关（09:00/21:00）

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

- 请求 `GET https://gateway.qoder.com.cn/algo/api/v2/model/list`，**无查询串**、**无请求体**，
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

## 参考文档

- [KNOWLEDGE.md](../KNOWLEDGE.md) — QoderWork API 逆向全记录（COSY 签名/编码/端点）
- [analysis/api-endpoints-scan.md](../analysis/api-endpoints-scan.md) — 客户端全端点扫描
- [analysis/qoderwork-real-oauth.md](../analysis/qoderwork-real-oauth.md) — OAuth 设备授权流程 + 本地测试证据

## License

MIT（based on workbuddy by lovingfish，见 [LICENSE](LICENSE)）
