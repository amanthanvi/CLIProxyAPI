# v7.3.16-slotclear.3 build record

This additive managed release follows `.2` release commit
`1d883b98ec90876e6318fb511069f6b3c671c753`. Code commits
`be7bfb1cc3e8843367b75021ed294a3f266cdbec` and
`9ff785bfbe0228615f11f5bd1fedb5010ace0233` preserve Fast/priority
requests and upstream effective-speed reporting across Claude/OpenAI
translations, then repair a concurrent Claude credential metadata read and
empty-map clone alias. It does not alter the `.2` artifacts or imply VM210
activation.

Both binaries were built on Ubuntu 24.04 WSL with Go 1.26.4, CGO enabled,
and Zig 0.15.2 targeting glibc 2.17. The build used `-trimpath
-buildvcs=false` and embedded `main.Version=7.3.16+slotclear.fast`,
`main.Commit=9ff785bfbe0228615f11f5bd1fedb5010ace0233`, and
`main.BuildDate=2026-10-03T02:13:29Z`. AMD64 used `zig cc -target
x86_64-linux-gnu.2.17`; arm64 used `zig cc -target
aarch64-linux-gnu.2.17`. Highest imported GLIBC version was 2.3.2 on
amd64 and 2.17 on arm64.

| Architecture | Binary bytes | Binary SHA-256 | Archive bytes | Archive SHA-256 |
| --- | ---: | --- | ---: | --- |
| amd64 | 74,588,712 | `416a3c26f42d8d8f267e49479c1009c1310b3fe5ed18b86412e2714ff76b12f7` | 23,544,994 | `c1dd1b9ccd531292f3284e838f314f8cd82cc44610bd8ac9fccfb78bb611bc31` |
| arm64 | 68,354,720 | `ff000c3fdb44faa663d5bbfe7e20bb0b90a19a8bb144a40784b715dc8dc373a4` | 21,280,911 | `e7323a15f201d0a5fd777123f8fe8a68c18da27d88c77e1b30a7ab6a897627f8` |

The archives are `CLIProxyAPI_7.3.16-slotclear.3_linux_amd64.tar.gz` and
`CLIProxyAPI_7.3.16-slotclear.3_linux_aarch64.tar.gz`. GNU tar used ustar,
mtime 0, uid/gid 0, mode 0755, and `gzip -n`; each archive contains exactly
one `cli-proxy-api` whose streamed hash matches the corresponding binary.

All translator packages passed; Windows `go build ./cmd/server` passed.
Linux `go test -race` passed 25 runs of the exact shared-credential test and
the full `internal/auth/claude`, `internal/runtime/executor`, and
`sdk/cliproxy/auth` package suites. Independent read-only review found no
blocking issue. Unfiltered `go test ./...` remains red on two tests also
reproduced on pristine `.2`: `internal/registry/TestGetDevinModelsFallback`
and `sdk/cliproxy/TestRegisterModelsForAuth_AntigravityFetchesWebSearchCapability`.
The first full run before the race fix also exposed the concurrent-map panic;
the focused race validation addresses it.

No server startup/health smoke, arm64 execution, live inference canary, or
provider entitlement/effective-speed check was made for `.3`. Fleet adoption
and VM210 activation require separate gates; the VM210 backup-owner/GO gate
remains held.
