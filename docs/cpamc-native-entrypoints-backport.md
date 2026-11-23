# CPAMC Native Entrypoints Backport

## Historical Evidence Boundary

This document records the managed `8.0.49-cpamc.2` backport and its historical
qualification. The upgrade candidate already contains upstream commit
`6d4102f33f1037988330d2383a950bc6ffa1001f` and uses native `ojson`; the historical
`gjson` adaptation below does not describe this candidate. The measured T3
`2.1.323` regression is retained without reimplementing the upstream feature.
No historical test or workflow result establishes validation or activation of
this source-only upgrade candidate.

## Provenance

The `8.0.49-cpamc.2` candidate keeps the managed `8.0.49-cpamc.1` baseline,
commit `c11d4efece7eb0887440449673f475b8944c362c`, including its reviewed managed
overlay. It backports only official upstream commit
`6d4102f33f1037988330d2383a950bc6ffa1001f` (November 3, 2026),
`feat(config): support native-entrypoints configuration for Claude Code clients`.
The nine upstream files retain the same added and removed lines, except that
upstream `ojson` calls use the baseline's existing `gjson` API. No JSON-library
migration or other upstream commit is required.

## Trigger And Scope

Official T3 Code Nightly `0.1.10-nightly.20261114.4695` sets its native Claude
process entrypoint to `t3-code`. Its bundled SDK preserves that supplied value.
A controlled native Claude Code `2.1.323` probe reproduced the reported Fable
5.2 failure with `t3-code`, while the same binary and model succeeded with
`sdk-cli`. The gateway's default native allowlist excludes `t3-code`, causing
an otherwise valid request to receive the gateway's `2.1.280` baseline profile.
Anthropic rejects that profile for the selected model.

The backport provides an explicit operator extension:

```yaml
config-version: 8
upstream:
  claude:
    header-defaults:
      native-entrypoints: ["t3-code"]
```

Configured entrypoints still require the existing strong native-client signals.
The setting does not authorize arbitrary headers or requests, change the
default fingerprint, alter model routing, or relabel the T3 client. The normal
non-stabilized path preserves the real client software profile; existing
stabilization and configured baseline policies still apply when enabled, with
separate configured-client profile scopes. Without the setting, default
recognition remains unchanged.

The measured failing request uses `X-App: cli` and the ordinary user agent
`claude-cli/2.1.323 (external, t3-code)`. Later upstream UA-extension and
`cli-bg` changes are not part of this backport because neither is needed for
the measured trigger. Official T3 binaries remain unmodified.

## Evidence

Before the production backport, the additional
`TestDetectClaudeCodeRequestConfiguredT3Code323` regression compiled against
the baseline and failed for Messages and count-tokens. Both had valid strong
signals but `NativeClient: false` and `Confirmed: false`. After the backport,
the complete configuration, executor-helper, and executor package tests passed,
including that same regression and the upstream native-profile preservation,
configured-helper, configuration-normalization, and profile-isolation tests.

Run validation on Linux or WSL, not Windows test executables:

```sh
go test ./internal/config ./internal/runtime/executor/helps ./internal/runtime/executor -count=1
go test ./...
go vet ./...
go test -race ./internal/config ./internal/runtime/executor/helps ./internal/runtime/executor -count=1
go build -o /path/outside/source/cli-proxy-api ./cmd/server
```

Build the exact reviewed commit with Go `1.26.10`, Zig `0.15.2`, cgo enabled,
and Linux glibc `2.17` targets for amd64 and arm64. Use the commit time as
BuildDate, deterministic archive metadata, and before/after source manifests.
Archive checksums, native amd64 startup, and QEMU arm64 startup are artifact
evidence; they do not establish native arm64 hardware or live provider acceptance.
Activation still requires scoped configuration, idle/recovery checks, and a
successful native T3 workflow on the selected model. No deployment authority
or acceptance follows from this source document alone.
