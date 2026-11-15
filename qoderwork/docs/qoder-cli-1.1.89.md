# Qoder CLI 1.1.89 wire analysis

This note records the shipped-artifact evidence used by the plugin's request and
context-compaction compatibility layer.

## Artifact identity

| Artifact | SHA-256 | REA evidence |
| --- | --- | --- |
| Official `qodercli-darwin-arm64.tar.gz` payload | `88fbfdafb4b1b8e3679cf406b1af22f6b1a5f34ec796c5f569b8d043ce8f96e8` | `ev_58b07e5e6c2116252ced195a1a204413e56980c8c695b8ad1773d7890da11eba` |
| Official npm `bundle/qodercli.js` | `b7a2d6d54bf868f7226abc759c10c6361ea4ab04ddd4c0615eb90ccef3524377` | Module `jag_node_43dbe9f870bf1972030c131bfcd9e658766e0d8727aa1824e2f64e9c316434d9` in `ev_49dabcecf4a91854eb24a53493a286b23dc9789a315204a30bdb4101cccc0dc1` |

The native binary is a Bun Mach-O containing an `__BUN` segment. It is signed
by `Alibaba.com Singapore E-Commerce Private Limited (T27K5A5ZWD)`; the
signature evidence is `ev_7472a99ce55150aa49ecd750ed87a0732d2e06918f7ff76728f3d4606b8184c7`.

Because the 158 MB native binary's first Ghidra import exceeded the initial
analysis window, the equivalent versioned npm JavaScript bundle was used for
protocol inspection. REA retained its complete static facts even where graph
serialization hit the transport budget.

`@qoder-ai/qoder-agent-sdk@1.0.59` was also inspected as the public host-side
contract. It launches the same `qodercli` process/daemon and exposes
`QoderModelPurpose.Compact` / `Compression`, `autoCompact.thresholdPercentage`,
and compact-boundary metadata; it does not independently define the remote HTTP
body or summarization algorithm.

## Remote-chat body

CLI 1.1.89's request builder emits:

```json
{
  "request_id": "<uuid>",
  "chat_record_id": "<same uuid>",
  "request_set_id": "<uuid>",
  "session_id": "<uuid>",
  "stream": true,
  "chat_task": "FREE_INPUT",
  "is_reply": true,
  "is_retry": false,
  "source": 1,
  "version": "3",
  "agent_id": "agent_common",
  "aliyun_user_type": "",
  "session_type": "qoderclicn | qodercli",
  "task_id": "common",
  "system": null,
  "chat_context": {},
  "messages": [],
  "tools": [],
  "parameters": {},
  "model_config": {},
  "business": {}
}
```

`chat_context` contains the latest user text as both `text` and
`extra.originalContent`, empty `context`/`features`, `imageUrls: null`, and a
model config pair `{key, is_reasoning}`.

`parameters` can carry `max_tokens`, sampling fields, `reasoning_effort`,
`enable_thinking`, `reasoning_budget_tokens`, `preserve_thinking`,
`context_length`, and `tool_choice`.

`model_config` carries `key`, `display_name`, an empty `model`, `is_vl`,
`is_reasoning`, `max_input_tokens`, `source`, `format`, and empty `api_key` /
`url` fields.

The CLI routes infer SSE with these identity headers:

- `X-Qoder-Model`
- `X-Session-ID`
- `X-RequestSet-ID`
- `Cosy-Version: 1.1.89`
- `Cosy-ClientType: 5`
- `Cosy-MachineOS`
- `Cosy-MachineHostname`
- `Cosy-MachineCode`, `Cosy-MachineToken`, `Cosy-MachineType` when a machine
  identity resolver supplies them

The inference URL remains:

```text
/algo/api/v2/service/pro/sse/agent_chat_generation
```

and is QoderEncoding-protected with `Encode=1`.

## Context compaction

The CLI computes automatic compaction pressure as follows:

```text
reserve_output      = min(model_max_output, 20000)
message_budget      = context_window - reserve_output
headroom            = max(13000, floor(message_budget * 0.05))
auto_compact_at     = message_budget - headroom
```

This yields approximately 167k tokens for a 200k model and 931k for a 1M model.
`QODER_AUTOCOMPACT_WINDOW` can cap the window and
`QODER_AUTOCOMPACT_PCT_OVERRIDE` can lower the threshold as a percentage of
`message_budget`.

Compaction is a forked, one-turn model request over the existing conversation.
It uses `tool_choice: "none"` and extracts a `<summary>` block. The resulting
history starts with a compact-boundary checkpoint followed by the summarized
content; a later request resumes from that checkpoint.

Qoder's compacted-history preamble includes:

```text
This session is being continued from a previous conversation that ran out of context.
The summary below covers the earlier portion of the conversation.
```

The plugin implements the same threshold and summary-call shape. Because CPA
plugins are stateless between complete client requests, the replacement
checkpoint is maintained in a bounded per-client-session memory cache rather
than written into a local Qoder transcript.

## Live plugin verification

A PAT-backed CN probe successfully completed token exchange, user identity,
catalog discovery and campaign query. Requests for `auto`, `fast` and
`qfmodel` reached the normal Qoder model-queue branch rather than failing
signature/request-schema validation. The account received `retry_after=60s`
because those models were capacity-queued during the probe, so a completed
assistant response was not observed in that run.

## Remaining unknowns

- The static analysis did not execute Qoder's private Wasm signing module; the
  plugin continues to use its existing COSY signer.
- A live authenticated CLI request was not available from the CLI installation
  itself because it had no logged-in account. Plugin live verification uses a
  separately supplied PAT and has confirmed request admission, not a final
  completion while models remain queued.
- Qoder's raw compaction summary instruction is partially string-obfuscated.
  The plugin preserves the verified no-tools and `<analysis>/<summary>` contract
  and adds a structured checkpoint schema to make summaries deterministic.
