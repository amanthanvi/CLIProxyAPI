# v7.3.16-slotclear.1 build record

This patch release is based on upstream CLIProxyAPI `v7.3.16`, commit
`c404af96ebacedf8168b3c2bdbf4449a21cd1c1e`. The sole production-code
change clears a consumed usage-queue backing-array slot. The accompanying test
checks that the slot is cleared without changing FIFO delivery, context, or
record identity. The applied patch has SHA-256
`3e46c9ff893f85beec707a49e93606a7d0b6ed020ba17b6fbac7bc7bb3f49ba1`.

Both Linux binaries were built from this source with Go 1.26.4, CGO enabled,
and Zig 0.15.2 targeting glibc 2.17. Zig was used in place of upstream's
manylinux2014 GCC toolchain; these are managed builds, not official upstream
release assets. The binary reports `7.3.16+slotclear`; the immutable release
tag and install path use `v7.3.16-slotclear.1` to identify this packaging
revision, and deployment binds both to exact SHA-256 values. From a Linux
amd64 builder with both tools installed:

```bash
export GOTOOLCHAIN=go1.26.4 CGO_ENABLED=1 GOOS=linux
export ZIG_LOCAL_CACHE_DIR="${ZIG_LOCAL_CACHE_DIR:-$PWD/.zig-local-cache}"
export ZIG_GLOBAL_CACHE_DIR="${ZIG_GLOBAL_CACHE_DIR:-$PWD/.zig-global-cache}"
for spec in 'amd64 x86_64-linux-gnu.2.17' 'arm64 aarch64-linux-gnu.2.17'; do
  read -r arch target <<< "$spec"
  GOARCH="$arch" CC="zig cc -target $target" go build -trimpath -buildvcs=false \
    -ldflags='-s -w -X main.Version=7.3.16+slotclear -X main.Commit=c404af96+patch.3e46c9ff -X main.BuildDate=2026-10-02T04:54:49Z' \
    -o "cli-proxy-api-$arch" ./cmd/server/
done
```

The build environment may set `GOMODCACHE` and `GOCACHE` to writable cache
locations. The measured binaries were:

| Architecture | Bytes | Binary SHA-256 | Highest imported GLIBC version |
|---|---:|---|---|
| amd64 | 74,584,744 | `a9c19cbed3577ea36cc3324faf087625df165ffd1ddc87561b742edf2ef018e1` | 2.3.2 |
| arm64 | 68,350,568 | `f05306e77ce4a9fc3b16af27b1a526573a96f17e65fc8cc3536070c6cd2f149a` | 2.17 |

Each binary was rebuilt with identical flags and retained its checksum. Each
release archive contains only the executable named `cli-proxy-api`, with
fixed uid/gid, mode, and mtime. For each `$arch` (`amd64` or `arm64`):

```bash
release_arch=$arch
[[ $arch == arm64 ]] && release_arch=aarch64
tar --format=ustar --mtime=@0 --owner=0 --group=0 --numeric-owner \
  --mode=755 --transform="s|^cli-proxy-api-$arch$|cli-proxy-api|" \
  -I 'gzip -n' \
  -cf "CLIProxyAPI_7.3.16-slotclear.1_linux_${release_arch}.tar.gz" \
  "cli-proxy-api-$arch"
```

| Archive | SHA-256 |
|---|---|
| `CLIProxyAPI_7.3.16-slotclear.1_linux_amd64.tar.gz` | `9e0e539577f78206d499b838362ebe4967db18c9271d3d2dbcac4ab29d80bbac` |
| `CLIProxyAPI_7.3.16-slotclear.1_linux_aarch64.tar.gz` | `d04bd13efcc24115ba7c824a90c655e1c106e605a879b73f642bcbb789085cb9` |

The amd64 binary passed isolated synthetic startup and dynamic-plugin checks
on x86_64 Linux. The arm64 binary passed the same checks under QEMU user-mode
emulation with arm64 glibc 2.39; it has **not** been run on native arm64
hardware or under a glibc 2.17 runtime. These checks do not establish
production workload behavior or that the slot fix prevents an OOM. The full
upstream test suite has two failures reproduced without this patch:
`TestGetDevinModelsFallback` and
`TestRegisterModelsForAuth_AntigravityFetchesWebSearchCapability`; it must not
be represented as green.
