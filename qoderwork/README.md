# QoderWork CPA 插件

CLIProxyAPI 的 QoderWork provider，支持国内版和国际版 PAT 账号、动态模型、COSY 签名对话、每日活动、CLI 1.1.89 请求兼容和自动上下文压缩。

## 登录与账号

1. 在 [国内版](https://qoder.cn/account/integrations) 或 [国际版](https://qoder.com/account/integrations) 创建个人访问令牌（PAT）。
2. 打开插件面板 `/v0/resource/plugins/qoderwork/panel`，选择「PAT 登录」和对应区域，粘贴 PAT 并导入。
3. 插件校验 PAT、账号身份后保存到 CPA 凭证库。两区域凭证互不通用；国际版凭证文件名带 `intl` 前缀。

已保存的设备授权账号（`dt-`/`drt-`）继续使用 Qoder 的 deviceToken 刷新接口和 COSY 签名目录/对话；PAT 账号则通过该区域的 jobToken 接口刷新。两种凭证都可用于模型发现与聊天。请勿将 PAT 或凭证文件提交到仓库。

## 客户端兼容与请求线缆

插件按 Qoder CLI `1.1.89` 的 remote-chat 线缆构造请求：

- 固定 `chat_task=FREE_INPUT`、`version=3`、`task_id=common`，并发送完整的 `chat_context`、`parameters` 和 `model_config`。
- `messages` 保留工具调用、工具结果、图片、assistant 推理文本和 system/developer 指令；Developer 会按上游角色映射为 system。
- 使用 CLI 的 `X-Qoder-Model`、`X-Session-ID`、`X-RequestSet-ID`、`Cosy-Version` 和 `User-Agent` 路由头。
- 同时解析 Chat Completions、OpenAI Responses、Anthropic content blocks 以及 DeepSeek Harness 常见消息形状，因此 Codex、Claude Code 和 DeepSeek Harness 都可通过 CPA 使用。
- Claude Code、Codex CLI 和 DeepSeek Harness 的已屏蔽身份模板仍会经过共享 `contextrules` 清洗。

REA 分析记录、二进制与 npm bundle 哈希、完整字段和压缩阈值见
[`docs/qoder-cli-1.1.89.md`](docs/qoder-cli-1.1.89.md)。

## 模型与上下文

模型目录使用 `qoder/` 前缀和可读名称（例如 `qoder/qwen3.8-flash`），调用上游时转换为该账号的模型 key。国内版和国际版的目录按账号及区域隔离。获取失败时先使用该账号保存的目录，再使用区域内置目录。目录文件存放于系统缓存目录的 `qoderwork` 子目录，可通过 `QODERWORK_CACHE_DIR` 更改；缓存不包含 PAT。

从旧版本升级后，请在 CC Switch 刷新模型列表并重新选择 `qoder/` 模型。旧的 `qfmodel` 等短 key 不再直接注册到 CPA 模型列表；若配置了针对旧 key 的 CPA 模型别名，需要一并更新为对应的新名称。

模型元数据声明了多个上下文档位时，默认使用最大档位。面板「模型与上下文」可分别控制国内版和国际版；也可在插件配置中设置：

```yaml
plugins:
  configs:
    qoderwork:
      cn_max_context: true
      intl_max_context: true
      context_compaction: true
      context_compaction_pct: 0
      context_compaction_max_tokens: 8192
      context_compaction_model: ""
      rewards:
        auto: true
        actions:
          checkin: true
```

### 自动上下文压缩

默认开启 CLI 兼容的上下文压缩。阈值按 CLI 1.1.89 的公式计算：先用模型上下文减去最多 20k 输出预留，再预留 `max(13k, 消息预算的 5%)`。因此 200k 模型约在 167k 输入 token 触发，1M 模型约在 931k 触发。

达到阈值后，插件把当前最后一条 user 输入之前的历史交给同一模型做一次只读摘要调用，随后用 `<compacted-summary>` 检查点替换旧历史，并保留 system/developer 指令和当前回合。摘要请求带 `tool_choice:"none"` 和 `task_id:"agent_summary"`。同一客户端 session 的已覆盖前缀会在内存中缓存 45 分钟，后续请求不会重复摘要同一段历史；服务重启后缓存清空。

配置项：

- `context_compaction: true|false`：总开关，默认 `true`。
- `context_compaction_pct: 0-100`：可选百分比覆盖；`0` 使用 CLI 公式。
- `context_compaction_max_tokens`：摘要调用的最大输出 token，默认 `8192`。
- `context_compaction_model`：可选专用上游模型 key；为空时复用当前请求模型。

摘要失败时不会让主请求失败，插件会记录脱敏警告并继续发送原始历史。也可通过环境变量兼容 CLI：`QODER_AUTOCOMPACT_WINDOW` 和 `QODER_AUTOCOMPACT_PCT_OVERRIDE`。

客户端可以继续通过 CPA 的 `oauth-model-alias` 配置别名。

Codex 等客户端的推理档位会映射到模型声明的最近档位；例如该模型仅提供 `low / medium / xhigh` 时，`high` 会映射到 `xhigh`。上游未声明档位时沿用其默认设置。

## 签到和额度

国内版与国际版按各自账号查询可领取活动。面板支持手动和自动领取；自动查询时间为北京时间 10:05、11:05、15:05、19:05、23:05，并在启动后补查。领取请求不盲目重试。面板显示活动结果和套餐、资源包的额度信息。

## 诊断与测试

- `GET /v0/management/plugins/qoderwork/models/status` 查看最近模型目录状态。
- `GET /v0/management/plugins/qoderwork/models/context` 查看当前区域窗口配置、压缩开关、触发统计和缓存命中。
- `POST /v0/management/plugins/qoderwork/models/refresh` 刷新模型目录。
- 在 `qoderwork` 目录运行 `go test ./...` 和 `node --test panel.test.js`。真实 PAT 集成测试使用 `QODER_TEST_PAT_PATH=/path/to/pat.txt go test -run TestLivePATIntegration -v`；设置 `QODER_TEST_CHECKIN=1` 会检查并领取当前可领取活动。

## License

MIT；参考项目及许可见 [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md)。
