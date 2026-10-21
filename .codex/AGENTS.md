# Project rules

## Plugin versioning

`workbuddy` and `qoderwork` are versioned independently. Never reuse one plugin's version for the other.

- Every user-visible feature or fix increments each affected plugin by exactly one patch version.
- Examples: `workbuddy` 0.10.4 → 0.10.5; `qoderwork` 0.5.0 → 0.5.1.
- The QoderWork release line in this repository is 0.5.x. Do not substitute 0.7.x from another branch or release line.

When bumping a plugin, update all of these in the same change:

1. `<plugin>/VERSION`
2. The `var version = "..."` default in `<plugin>/main.go`
3. The plugin's `version` in `registry.json`

Release archives are local build outputs. Do not commit them: for every plugin, keep `*.zip` and `*.zip.sha256` out of version control, including `<plugin>_<version>_linux_amd64.zip` and its checksum. Generate them only as needed for handoff or release upload.

Build with the release version explicitly:

```bash
go build -trimpath -buildmode=c-shared \
  -ldflags "-s -w -X main.version=<version>" \
  -o dist/<plugin>.so .
```

Verify the packaged `.so` reports the intended version (for example with `strings`), that it is x86-64 ELF, that CPA exports are present, and that `sha256` verification passes. Do not include stale archives from earlier versions in the same release.
