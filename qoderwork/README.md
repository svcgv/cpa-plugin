# QoderWork CPA 插件

CLIProxyAPI 的 QoderWork provider，支持国内版和国际版 PAT 账号、动态模型、COSY 签名对话、每日活动和上下文窗口设置。

## 登录与账号

1. 在 [国内版](https://qoder.cn/account/integrations) 或 [国际版](https://qoder.com/account/integrations) 创建个人访问令牌（PAT）。
2. 打开插件面板 `/v0/resource/plugins/qoderwork/panel`，选择「PAT 登录」和对应区域，粘贴 PAT 并导入。
3. 插件校验 PAT、账号身份后保存到 CPA 凭证库。两区域凭证互不通用；国际版凭证文件名带 `intl` 前缀。

旧设备授权账号的记录会保留；缺少 PAT 的账号需要导入对应区域的 PAT。插件通过该区域的 jobToken 接口刷新凭证。请勿将 PAT 或凭证文件提交到仓库。

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
      rewards:
        auto: true
        actions:
          checkin: true
```

客户端可以继续通过 CPA 的 `oauth-model-alias` 配置别名。插件传递完整消息、工具调用及结果，不负责压缩历史；自动压缩由客户端管理。

Codex 等客户端的推理档位会映射到模型声明的最近档位；例如该模型仅提供 `low / medium / xhigh` 时，`high` 会映射到 `xhigh`。上游未声明档位时沿用其默认设置。

## 签到和额度

国内版与国际版按各自账号查询可领取活动。面板支持手动和自动领取；自动查询时间为北京时间 10:05、11:05、15:05、19:05、23:05，并在启动后补查。领取请求不盲目重试。面板显示活动结果和套餐、资源包的额度信息。

## 诊断与测试

- `GET /v0/management/plugins/qoderwork/models/status` 查看最近模型目录状态。
- `GET /v0/management/plugins/qoderwork/models/context` 查看当前区域窗口配置。
- `POST /v0/management/plugins/qoderwork/models/refresh` 刷新模型目录。
- 在 `qoderwork` 目录运行 `go test ./...` 和 `node --test panel.test.js`。真实 PAT 集成测试使用 `QODER_TEST_PAT_PATH=/path/to/pat.txt go test -run TestLivePATIntegration -v`；设置 `QODER_TEST_CHECKIN=1` 会检查并领取当前可领取活动。

## License

MIT；参考项目及许可见 [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md)。
