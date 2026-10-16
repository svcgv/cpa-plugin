# Changelog

## 0.7.0

### Add the international realm (qoder.com)

QoderWork CN and international are separate services with separate accounts and
a different inference protocol. The plugin now carries a realm on every
credential and routes accordingly; existing CN credentials are untouched
(region defaults to CN, inferred from the legacy `domain` field).

- Panel OAuth login gained a region selector (国内版 qoder.com.cn / 国际版
  qoder.com). The international flow uses `openapi.qoder.sh` for the device
  grant and the `qoder://aicoding.aicoding-agent/login-success` redirect.
- International inference is a plain `Authorization: Bearer {jt-}` call to
  `api2-v2.qoder.sh/model/v1/chat/completions` (OpenAI Chat Completions, no COSY
  signing, no agent envelope). Streaming and non-streaming are both supported.
- Model discovery is realm-aware: CN keeps the COSY catalog; international
  fetches `/model/v1/models` (OpenAI shape) with a static fallback. `model.static`
  returns the union when both realms are present; `model.for_auth` is always
  realm-exact.
- Token refresh, keepalive and quota/plan lookups pick the endpoint from the
  credential's realm.
- Check-in, the Pro upgrade pack and credits-based lifecycle are CN-only;
  international accounts skip them cleanly instead of erroring.
- Auth cards show a `[CN]`/`[INTL]` tag, the panel shows region badges plus a
  CN/INTL filter, and region is persisted in the auth file (`region` field).

Known limitation: the international model-catalog path is not yet confirmed
against a live international account (unauthenticated probes return 404), so it
is tried as a candidate list and falls back to a static international catalog.

## 0.6.0

### Fix dynamic model discovery (CN) — the "登录后拉取模型异常" root cause

The catalog fetch signed `qoderEncode("{}")` as the request body but then sent
the GET with **no** body. The gateway hashes the actual wire body, so the COSY
signature never matched and every catalog call came back

```
403 {"code":"101","message":"Signature invalid"}
```

which the plugin swallowed, silently degrading the model list to the 10-item
static fallback. Verified live: signing an empty body turns the same request
into `{"code":"105","message":"Login expired"}` (the real, auth-level answer),
i.e. the signature check now passes.

- Sign a bodyless GET: `GET /algo/api/v2/model/list` with **no** query string
  and no request body, and `Cosy-Bodyhash`/`Cosy-Bodylength` describing the
  empty body. `?Encode=1` + encoded-body forms remain as fallbacks.
- Parse plain JSON, QoderEncoding payloads and `{code,data:{...}}` envelopes;
  read `chat` then `inline`; honour `context_config`/`max_output_tokens`.
- Match the official client identity for discovery: `Cosy-Machinetype=5`,
  `Cosy-Machinetoken=Cosy-Machineid`, `Cosy-Machineos=<arch>_<os>`, and — when
  the official client's `SharedClientCache/cache/machine_token.json` exists —
  its cached machine identity and `Cosy-Version` (same source cockpit-tools
  uses).
- Persist the OAuth `machineId` in the auth file so the device grant and the
  later COSY requests present the same machine identity.
- Add `GET /plugins/qoderwork/models/status` and
  `POST /plugins/qoderwork/models/refresh` for diagnosis, plus a 2-minute
  failure backoff so a broken upstream cannot fan out on every `model.static`.
