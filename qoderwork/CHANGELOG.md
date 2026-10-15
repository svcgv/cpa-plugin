# Changelog

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
