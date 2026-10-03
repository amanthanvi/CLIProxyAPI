# v7.3.16-slotclear.2 build record

This managed revision builds on `v7.3.16-slotclear.1` (source commit
`0a3196623e7e5084092670682c1d2e2397c5ef66`) and retains its consumed
usage-queue slot clear. Source commit
`dc6e8a277009d5ab880d7bc41bce8df50675efe8` additionally avoids cloning
outbound request bodies for error-only logging after a successful downstream
Responses WebSocket upgrade. It does not change normal HTTP error-only logging,
full request logging, or failed-upgrade handling. A single sampled live heap
had 605,366,584 in-use bytes at `bytes.Clone` under `helps.deferAPIRequest`;
the profile does not attribute every clone to WebSocket traffic or establish
an all-cause OOM cure.

Both Linux binaries were built from the code commit above on Linux amd64 with
Go 1.26.4, CGO enabled, and Zig 0.15.2 targeting glibc 2.17. Zig replaces
upstream's manylinux2014 GCC toolchain. The embedded version is
`7.3.16+slotclear.wsdefer`, the embedded commit is the full code-commit SHA,
and the fixed build date is `2026-10-02T23:51:01Z`. The build used
`-trimpath -buildvcs=false` and
`-ldflags='-s -w -X main.Version=7.3.16+slotclear.wsdefer -X main.Commit=dc6e8a277009d5ab880d7bc41bce8df50675efe8 -X main.BuildDate=2026-10-02T23:51:01Z'`.
`GOMODCACHE`, `GOCACHE`, and both Zig caches were task-local. For each
architecture, `GOOS=linux`, `CGO_ENABLED=1`, and `GOTOOLCHAIN=go1.26.4` were
fixed; `GOARCH=amd64` used `zig cc -target x86_64-linux-gnu.2.17`, while
`GOARCH=arm64` used `zig cc -target aarch64-linux-gnu.2.17`.

| Architecture | Bytes | Binary SHA-256 | Highest imported GLIBC version |
| --- | ---: | --- | --- |
| amd64 | 74,574,152 | `ab16c28826b7d334dea5f5abd26f6760ef3193a8c681e4b11087e4c356b0c634` | 2.3.2 |
| arm64 | 68,339,976 | `edb8defb909ee1a4d0d921af64c3ca0dc5a820d8415a882f8b42cb557e27f73d` | 2.17 |

Identical second builds reproduced both checksums. The amd64 binary retained
dynamic `libdl` linkage. Each archive contains only `cli-proxy-api`, with uid
and gid 0, mode 0755, and mtime 0. As in `.1`, archives were made with GNU
tar `--format=ustar --mtime=@0 --owner=0 --group=0 --numeric-owner --mode=755`,
renaming `cli-proxy-api-$arch` to `cli-proxy-api` with `--transform` and using
`-I 'gzip -n'`.

| Archive | Bytes | SHA-256 |
| --- | ---: | --- |
| `CLIProxyAPI_7.3.16-slotclear.2_linux_amd64.tar.gz` | 23,546,018 | `7660aaeb3ff7aa4c540891704432552f23d4fac7512b2a4e9af047e932454f4f` |
| `CLIProxyAPI_7.3.16-slotclear.2_linux_aarch64.tar.gz` | 21,278,391 | `c31204dd3ced5e8645555d5963e164a134d476b8e35cdb525135d38f9adde4ec` |

Archive listing and streamed member hashing verified the single executable,
fixed metadata, and exact binary bytes. The four affected package suites
(`internal/runtime/executor/helps`, `internal/api/middleware`,
`sdk/api/handlers/openai`, and `sdk/cliproxy/usage`) passed unfiltered on
Go 1.26.4. The repository-wide suite still has two failures reproduced on
pristine upstream v7.3.16:
`TestGetDevinModelsFallback` and
`TestRegisterModelsForAuth_AntigravityFetchesWebSearchCapability`; it is not
represented as green.

The amd64 binary passed startup, health GET/HEAD 200, unauthenticated models
and WebSocket 401, and synthetic dynamic-plugin mapping in a loopback-only
network namespace. The arm64 binary passed the same checks under QEMU
user-mode emulation with arm64 glibc 2.39. Both exact smoke PIDs were absent
after TERM. Neither binary has been run against the live production workload,
and arm64 has not run on native hardware or a glibc 2.17 runtime.
