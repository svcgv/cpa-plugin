# Changelog

## 0.7.5 (qoderwork 0.5.7)

### Wire `oauth-model-alias` back to the upstream model key

`resolveUpstreamModel` existed but was never called, so a CPA alias such as
`deepseek-4.1-flash -> dfmodel` was forwarded to the gateway verbatim. The
gateway does not recognise the alias and the request fails.

- Both the streaming and non-streaming CN executors now resolve the alias
  before `cpaToUpstreamKey`.
- Alias lookup is case-insensitive and also accepts a `qoderwork/` or `qoder/`
  prefix, in case the client sends the provider-qualified name.
- Added regression tests covering host aliases, the per-auth attribute forms
  (`[{"name","alias"}]`, `{alias: name}`, `alias=name`) and passthrough of
  unknown models.

Note: once CPA applies an alias it replaces the original name in `/v1/models`,
so calling the upstream name directly returns 502 `unknown provider for model`.
That is host behaviour, not a plugin bug — use the alias.


## 0.7.4 (qoderwork 0.5.6)

### Never invent a working directory in the template prompt

The Qoder template's `<env>` block originally carried someone else's machine
(`D:/Projects/Qoder`, Windows). Version 0.5.5 rewrote it from `os.Getwd()`, but
the plugin runs inside the CPA host process, whose cwd is generally unrelated to
the user's project (inside the container it is `/CLIProxyAPI`). The model then
answered "I'm in /CLIProxyAPI on Linux" for a request made from a completely
different project.

CPA does not forward the client's working directory through `ExecutorRequest`,
so the plugin has no reliable source for it.

- If the request discloses a working directory (`cwd` at the top level or under
  `metadata`, in camelCase or snake_case), use it. Git status is only asserted
  when `.git` is actually observable from this process.
- Otherwise say so explicitly:
  `Working directory: unknown (not provided by the client; use paths from the
  conversation)` and `Is directory a git repo: unknown`.
- Never fall back to the host process cwd. A regression test asserts the host
  cwd never appears in the prompt.


## 0.7.3 (qoderwork 0.5.5)

### Stop the Qoder template prompt from overriding Codex

`baseprompt.json` carries a 12 KB "You are Qoder" system prompt that is sent on
every CN chat. When Codex connected through CPA's Responses bridge, both system
prompts were sent with Qoder's first, so the model answered "I am Qoder" and
used Qoder's tool names (`Bash`/`TodoWrite`) instead of Codex's
(`shell`/`apply_patch`).

Two additional defects were found in that template:

- Its `<env>` block was captured from someone else's machine:
  `Working directory: D:/Projects/Qoder`, `Platform: windows`,
  `OS Version: MINGW64_NT-10.0-22631`, `Today's date: <no value>`.
- It hard-codes 14 Qoder CLI tool names and orders the model to use
  `TodoWrite`, which Codex does not have.

Tried in order, per the requested fallback plan:

- Plan A (keep both prompts, client first, rewrite `<env>`): requests
  succeeded, but the model still answered "我是 Qoder" — the template's
  identity rules win because they are longer and more emphatic.
- Plan B (client system prompt replaces the template): **adopted**. The model
  now answers "我是 Codex，一个由 OpenAI 开发的编程代理" and picks `shell`.
  Requests with no client system prompt still use the template, with a
  rewritten `<env>` block.

Implementation:

- `clientSystemText` collects the client's own system messages.
- When present, they replace the template system prompt entirely; the template
  is still used for prompt-less clients, with `<env>` regenerated from this
  host (working dir, git repo, platform, OS, today's date).
- Added regression tests asserting no `You are Qoder`, `D:/Projects/Qoder`,
  `MINGW64` or `TodoWrite` leaks into a Codex request.

Verified end-to-end on a local CPA with a real CN account: identity, tool-name
selection and a plain request all return HTTP 200 with correct content.


## 0.7.2 (qoderwork 0.5.4)

### Accept Codex/Responses content parts

Codex reaches the plugin through CPA's Responses bridge, which sends message
content as a list of typed parts instead of a plain string:

```json
{"role":"user","content":[{"type":"input_text","text":"hi"}]}
```

`openAIMessage.Content` was declared `string`, so decoding failed with
`json: cannot unmarshal array into Go struct field openAIRequest.messages.0.content
of type string` and the request ended as HTTP 503
`auth_unavailable: no auth available`.

- `openAIMessage.UnmarshalJSON` now accepts a string, an array of content parts,
  or nested message parts, flattening text with newlines.
- Non-text parts (images, tool results) are ignored because the upstream agent
  body only carries a prompt string.
- Added regression tests for Codex-style content parts, output_text, nested
  content and upstream body construction.

Verified end-to-end on a local CPA with a real CN account: a content-part
request to `qfmodel` returns HTTP 200 with a normal completion.


## 0.7.1 (qoderwork 0.5.3)

### Fix "only auto works": read the current catalog scene and stop sharing a stale cache

Qoder 0.4.8 ships Qoder CLI/Worker runtime **1.1.69**. Its catalog response is
keyed by the client's configured scene, and that default is now `assistant`
(older builds used `chat`/`inline`). The plugin parsed only `chat`/`inline`, so
every real model was dropped and the list degraded to the static fallback —
which is why only `auto` appeared usable.

The international realm had a second, independent defect: the plugin called
`api2-v2.qoder.sh/model/v1/models` with a Bearer token, but CLI 1.1.69's
*catalog* endpoint is `api2.qoder.sh/algo/api/v2/model/list` with the same COSY
signing as CN (`api2-v2.qoder.sh` is the chat endpoint).

- Parse `assistant` first, then `chat`/`inline`, so both current and older
  gateway builds resolve. Entries from all scenes are merged with duplicate
  keys collapsed.
- Accept the mixed boolean encodings the catalog uses (`true/false`, `1/0`,
  `"1"/"0"`); `enable: 0` was previously treated as enabled.
- Honour `context_config` windows, `default_context_window`,
  `available_context_windows`, and string-typed token counts.
- Namespace the dynamic-model cache per credential owner. It used to be one
  process-wide entry, so the first account to succeed hid every other
  account's real catalog — a second source of a wrong, "stale" list in
  multi-account setups.
- Keep the CLI 1.1.69 wire shape first (`GET /algo/api/v2/model/list?Encode=1`,
  bodyless COSY signature) and retain bare-`/api` and encoded-body variants as
  fallbacks.
- COSY signature path derivation now also handles a bare `/api/v2/...` URL
  (signs `/v2/...`) in addition to the `/algo/api/...` form.
- International catalog discovery now uses the same COSY-signed scene catalog
  as CN, on the catalog gateway `api2.qoder.sh/algo/api/v2/model/list`
  (CLI 1.1.69), with the Bearer `/model/v1/models` candidates kept as fallback.
- Send the client identity the current Wasm client uses: `Cosy-Scene:
  assistant`, `Cosy-Business-Product: cli`, `Cosy-Business-Type: agent`, and
  `Cosy-Version: 1.1.69` (it was pinned to `0.1.43`). Verified against the live
  gateway with a bogus token: `105 Login expired` means the COSY signature was
  accepted; `101 Signature invalid` would mean it was not.
- Stop sending the legacy `Cosy-Sigpath` / `Cosy-Bodyhash` /
  `Cosy-Bodylength` headers: CLI 1.1.69's Wasm client no longer emits them. The
  signature already covers the path and the empty body.
- Fix host bridged HTTP decoding: the host marshals a real `*http.Response`
  (`StatusCode`/`Header`/`Body`), but the plugin parsed
  `status_code`/`headers`/`body`, so every bridged response arrived with status
  0 and was treated as a transport failure. That discarded a perfectly valid
  catalog and kept the static list. Both field sets are accepted now.
- Drop the trailing POST catalog fallback: the gateway is GET-only and answers
  400 "Request method 'POST' not supported", which overwrote the successful GET
  result in `/models/status`.

Evidence: REA artifact/JS analysis of `Qoder-Installer-mac-arm64.zip` ->
`Qoder-0.4.8-mac-arm64.zip` -> `app.asar`, plus the official
`qodercli-worker-runtime-darwin-arm64` 1.1.69 bundle
(`listModelsFromRemote`, `WqA`, `wb`, `yu().scene`).

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
