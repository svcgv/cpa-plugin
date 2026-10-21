# Qoder 0.4.8 模型目录逆向证据

目的：解释并修复 qoderwork 插件“登录后模型写死、只有 auto 可用”。

## 目标物

| 层 | 路径 | 说明 |
| --- | --- | --- |
| 外层安装器 | `~/code/qorder/Qoder-Installer-mac-arm64.zip` | SHA-256 `74740eef…de50`，300 MB |
| 内层安装包 | `Qoder Installer.app/Contents/Resources/payload/Qoder-0.4.8-mac-arm64.zip` | 302 MB |
| 应用 | `Qoder.app` 0.4.8，Electron 43.1.1，commit `f8c98a8` | `build-manifest.json` |
| 主逻辑 | `Contents/Resources/app.asar` | 153 MB，字符串表混淆 |
| CLI 运行时 | `qodercli-worker-runtime-darwin-arm64.tgz` 1.1.69 | 安装包内 `@qoder-ai/qoder-agent-sdk` 的 runtime-manifest 指定 |

REA 归档清单 evidence：`ev_4c66ea792f64c57bbc7d719142e3905a6b3872a770d65ff71628297990df9800`
（`inspect_artifact`，包含 `inventory_artifact` `ev_a12d85ecaed3787b13eca4a76676aea9f4ce438634d8801433675acbf5287d03`）。

JS 应用图：`analyze_javascript_application`
- 第一次因 unpacked native 模块签名后哈希不一致，按 `record-and-continue` 重跑；
- evidence `ev_8ae1e50aaf2aa21d824035e0fcebb4bf02e5f4e3b2eb1c72ac92f8b06938177f`（asar SHA-256 `93bd40fe…10fe2`）；
- 完整结果超过 MCP 传输预算，按设计视为不完整预览，不作为结论依据；结论改用解包后的源码与官方 CLI 运行时核对。

## 关键结论（均来自静态证据）

1. 桌面端不直接请求模型目录，模型列表来自 **Qoder CLI/Worker 运行时**（`listModels` / `QoderApi.listModelsFromRemote`），桌面主进程只做 `listChatModels` / `listComposerModels` 转发。
2. CLI 1.1.69 的目录请求：
   - path 常量 `HzA = "/api/v2/model/list?Encode=1"`；
   - URL 经 `iJ(endpoint, path)` 拼接，endpoint 不以 `/algo` 结尾时自动追加 → 线上实际为
     `https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1`；
   - 方法 GET、无请求体，COSY 签名由 Wasm `qodercontext_prepareRequest` 生成。
3. 目录响应按 **scene** 组织，客户端默认 scene 为 `assistant`
   （`yu().scene`，默认值 `sNt="assistant"`）；
   旧版插件只解析 `chat`、`inline`，因此真实模型被全部丢弃。
4. 每条模型字段：`key`/`display_name`/`format`/`enable`/`context_config`
   （`token_count` 可能是字符串）/`max_input_tokens`/`max_output_tokens`/
   `default_context_window`/`available_context_windows`/`efforts` 等；
   `enable` 可能是 `true/false` 或 `1/0`。
5. 国际版目录与 CN 同协议：catalog endpoint 是 `GQ`（prod `gateway.qoder.com.cn` / global `api2.qoder.sh`），
   chat endpoint 才是 `Bbe`（`api2-v2.qoder.sh`）。因此国际版也必须用 COSY + `/algo/api/v2/model/list`。
6. 线上无凭证探测：
   - `/algo/api/v2/model/list?Encode=1` → `403 {"code":"101","message":"Signature invalid"}`（路由存在，进入签名校验）
   - `/api/v2/model/list?Encode=1` → `404`
   说明 host+`/algo` 前缀正确，问题不是路径写错。

## 已实施修复（qoderwork）

- `models.go`：`modelCatalogScenes = ["assistant","chat","inline"]`，多 scene 合并、按 key 去重；
  逐条容错解码；兼容混合布尔与字符串数字；`context_config`/`default_context_window`/
  `available_context_windows` 取上下文长度。
- `models.go`：动态模型缓存改为 `map[owner][]ModelInfo`，owner = `region:uid`
  （无 uid 时退化为 PAT/token），避免多账号共用一份目录。
- `models.go`：首选 CLI 1.1.69 线形（`/algo/api/v2/model/list?Encode=1`），保留 bare `/api`
  与编码 body POST 兜底。
- `sign.go`：COSY 签名路径同时支持 `/algo/api/...` → `/api/...` 与 bare `/api/v2/...` → `/v2/...`。
- `intl.go`：国际版目录改为与 CN 相同的 COSY scene 目录，host 为 `api2.qoder.sh`
  （CLI 1.1.69 的 catalog endpoint 是 `GQ`，不是 chat 的 `api2-v2.qoder.sh`）。

## 验证

- `gofmt`、`go vet ./...`、`go test -race -count=1 ./...` 全部通过。
- 新增测试：assistant scene、编码响应、字符串 `token_count`、坏记录容错、bare `/api` 兜底、
  按账号隔离缓存、`Cosy-Sigpath` 取值。

## 未知

- 没有真实 Qoder 账号凭证，未能完成带凭证的线上目录拉取；需在 CPA 安装后通过
  `GET /plugins/qoderwork/models/status` 与 `POST /plugins/qoderwork/models/refresh`
  确认动态来源与模型数量。
- Wasm 签名内部实现未逐指令分析（REA 当前不支持 wasm 直接打开；已导出内嵌模块
  `qoder_auth_wasm_bg.wasm` 与 `wasm_real.wasm` 供后续 Ghidra 分析）。

## 图片附件为什么“发不出去”（2026-10-21 复核）

现象：插件文本会话正常，带图片时模型回答“没有收到图片”。

REA 复核 `qodercli-worker-runtime 1.1.69`（`qoder-worker-runtime.obf.mjs`）：

- 消息构造 `$dA → Ynl/Jnl`：附件被渲染进 `messages[].content` 的内容数组，
  `{"type":"image_url","image_url":{"url":"data:<mime>;base64,..."}}`；
- `chat_context` 由 `Eal()` 构造，`imageUrls` 恒为 `null`；
- `/api/v2/image/upload`（`qDd` / `tFt.upload`，`withSignature("auth")`）属于
  inference/feedback 的图片上传链路，聊天请求并不经过它。

也就是说：插件此前把图片下载/上传后写进 `chat_context.imageUrls`，
网关不会把该字段作为模型输入，图片因此被丢弃（上传失败还会静默 continue）。

真实凭证直打 `agent_chat_generation` 的实测：

| 请求形态 | 模型回答 |
| --- | --- |
| 纯文本、无 image part | “您的消息中没有附带图片” |
| `messages` 内联 data URL 图片 | “红/蓝”（正确识别左红右蓝） |
| `chat_context.imageUrls` | 仍看不到图片 |

修复：`buildQoderBody` 通过 `messageContent()` 把图片作为 content part 内联；
删除 `image_upload.go` 的上传兜底；新增
`TestBuildQoderBodyInlinesImagesAsContentParts` 与
`TestBuildQoderBodyAllowsImageOnlyRequest` 两条回归测试。
