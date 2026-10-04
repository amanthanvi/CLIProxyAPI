# v7.3.16-slotclear.4 build record

This additive managed release follows deployed `.3` source
`8095034e762695f7429d1daf5f249d820a6f4607`. Source commit
`47af05c669e9055e3b3f62885e55ab3991bdad5d` adds opt-in
`claude-code.bare-model-prefix`. Otherwise-unroutable native Claude model IDs
on Messages/count_tokens may use one explicitly configured existing Claude
provider route. Other entrypoints, explicit prefixes, catalog, credential
selection, context/effort/Fast fields and headers remain unchanged. Existing
bare routes, Home mode and active plugin model routers retain ownership.

Both binaries were built from an exact `git archive` of that source commit
on Ubuntu WSL using Go 1.26.4, CGO and Zig 0.15.2, targeting glibc 2.17.
Flags: `-trimpath -buildvcs=false -ldflags '-s -w ...'`. Embedded version is
`7.3.16+slotclear.native`, commit is the source SHA above, and build date is
`2026-10-04T06:19:57Z`. Targets are `x86_64-linux-gnu.2.17` and
`aarch64-linux-gnu.2.17`. Highest imported GLIBC versions are 2.3.2 and 2.17.
Archives use ustar, mtime 0, uid/gid 0, mode 0755, and `gzip -n`.

| Architecture | Binary bytes | Binary SHA-256 | Archive bytes | Archive SHA-256 |
| --- | ---: | --- | ---: | --- |
| amd64 | 74592160 | `51b415d79508757a33600c5454ead35b1801558686413ba7342e81a96d24930f` | 23547221 | `3ed95e2792ebfd35e6ab04c7b57a4206c2a49f854b4971267c6739ef73cab860` |
| arm64 | 68357616 | `b4920fe7ec7490ba095b511b38574348fa9daeac865bd14be996d29d4afce605` | 21283058 | `c845b3cba3e2043839e3ac0da0bd503f657a38bf2ee62542ebcffd5794fa6de6` |

Behavior tests reproduced the bare-model 400 before implementation. Focused
handler/config/diff tests, full Linux handler/config/diff race checks and vet,
and Windows build pass. Independent Opus 5.5/high review found no blocker;
its bounded compatibility/test recommendations were incorporated.

Unfiltered `go test -count=1 ./...` is not fully green. Devin catalog fallback
and Antigravity capability tests also fail on the untouched baseline. One
final rerun additionally failed the Antigravity connection-pooling assertion
(9 connections, at most 8 expected); its focused 20-run repeat passed. That
intermittent failure is not claimed resolved. No arm64 execution was performed.

This publication alone does not claim production activation or live model
success. Fleet deployment requires fresh recovery, idle, exact-hash and
native official-T3 canary evidence. Preserve `.3` for rollback.
