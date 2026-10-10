# v7.3.16-slotclear.5 Build Record

This additive managed release follows deployed `.4` source
`378c5d67942e4ed83de312c2db9d4d45be07ced3`. Source commit
`9cfd4cc3d29fa71d8a45bb1304919600432adaad` backports official CLIProxyAPI
v8.0.23's native response.interrupt and late-interrupt corrections, including
their terminal-connection-error prerequisite. Exact upstream commits and the
small import/test synchronization adaptations are recorded in
[`docs/INTERRUPT-BACKPORT.md`](docs/INTERRUPT-BACKPORT.md).

Native Codex 0.162.1's interrupt-and-drain continuation is supported in both
serial/default and opt-in duplex mode, and HTTP-backed turns are canceled
locally. Existing transport/model settings are unchanged. This does not add
replay, retries, model substitution or credential substitution. All five
managed `.4` behaviors are preserved. A raw v8.0.23 replacement would lose
those managed fixes and migrate the module/configuration surface; this bounded
backport avoids that urgent-upgrade regression risk.

Both binaries were built from an exact `git archive` of the source commit on
Ubuntu WSL with Go 1.26.4, CGO, and Zig 0.15.2. Flags:
`-trimpath -buildvcs=false -ldflags '-s -w ...'`; targets
`x86_64-linux-gnu.2.17` and `aarch64-linux-gnu.2.17`. Embedded version is
`7.3.16+slotclear.interrupt`, commit is the source SHA above, and build date is
`2026-10-10T03:43:27Z`. Highest imported GLIBC versions are 2.3.2 (amd64) and
2.17 (arm64). Archives use ustar, mtime 0, uid/gid 0, mode 0755, and `gzip -n`.

| Architecture | Binary Bytes | Binary SHA-256 | Archive Bytes | Archive SHA-256 |
| --- | ---: | --- | ---: | --- |
| amd64 | 74622848 | `e520005609feceda8245e8759f401c52b862f1c6dfc75bfa7041fab6b5bebae9` | 23560903 | `df1d7fe70bd1f772b6438df9cb580f7d1e10153afa079a002d6053ea7f804f2f` |
| aarch64 | 68386984 | `16c672ee7e1bd00c025b61fe6acee402ce7b0e626f9368dd2fd0fc05932f8a6a` | 21294902 | `e46ba6b9dc0133436c83290a6ea2ce20793104e8fade5384299a51745a1c3fea` |

The original managed .4 rejection was reproduced by a real in-flight WebSocket
test before implementation. Backported upstream tests cover serial and duplex
interrupt delivery, HTTP cancellation and continuation, malformed/idle/disabled
credential rejection, Home-session auth, terminal-state races, late interrupts,
sanitized diagnostics and socket cleanup. Focused Windows tests passed ten
repeats; Linux race tests passed three focused repeats and both full/unfiltered
handler and executor package suites. Windows server build and diff checks pass.
Independent review found no blocker in the final source.

Unfiltered Windows `go test ./... -count=1` is not fully green: the existing
Devin fallback ownership and Antigravity capability assertions still fail.
Both failures reproduced on the untouched baseline. No catalog files changed.
The upstream HTTP-cancel test had a scheduling race: it inspected the mock
executor immediately after downstream incomplete delivery. Its completion
channel now requires actual context cancellation without assuming goroutine
execution order. This test-only correction does not change production behavior.

The amd64 binary ran its isolated help/version banner successfully. No arm64
runtime execution occurred. Publication does not claim production activation
or a live provider-backed steering canary. Deployment requires fresh rollback,
active-work, exact-hash, legacy management API and native-client canary evidence;
preserve `.4` for rollback and keep existing configuration unchanged.
