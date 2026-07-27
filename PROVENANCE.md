# Provenance — cortext-cpa-plugin native artifacts

## Plugin identity

| Field | Value |
|---|---|
| Plugin id | `cortext` (production library basename) |
| Stub id | `cortext-stub` (CI/protocol basename only) |
| Version | `0.1.0` (`plugin/main.go` `pluginVersion`) |
| Registration Version | `0.1.0+native` or `0.1.0+stub` (`pluginVersion` + `engineFlavor`) |
| License | Apache-2.0 |

## Build modes

| Artifact | Build | Engine |
|---|---|---|
| `bin/cortext-stub.dylib` / `.so` / `.dll` | `make build-stub` | In-process **stub** (keyword overlap). CI / protocol only. |
| `bin/cortext.dylib` / `.so` / `.dll` | `make build-native` | Real Cortext via [`github.com/augmem/cortext.go`](https://github.com/augmem/cortext.go) + release natives |

Stub builds must not be labeled as Cortext retrieval quality. `make install`
refuses to copy the stub under the production basename; use
`make install-native` for production and `make install-stub` only for
protocol experiments.

## Native dependencies

1. **Go module** [`github.com/augmem/cortext.go`](https://github.com/augmem/cortext.go) (pinned in `plugin/go.mod`, currently `v1.2.4`).
2. **Release assets** from [augmem/cortext releases](https://github.com/augmem/cortext/releases) (`cortext-assets-<version>.tar.gz`), fetched by `cortext.go` on first open unless pre-provisioned.
3. Optional env overrides (see cortext.go README):
   - `CORTEXT_ASSETS_DIR` — pre-unpacked assets tree
   - `CORTEXT_LIBRARY_PATH` — explicit `libcortext` shared library
   - `CORTEXT_AIST_MODEL_PATH` — explicit GGUF only when forcing a file on disk

There is no `CORTEXT_ROOT` / cgo link step for the binding. CGO is only
used for CPA’s `-buildmode=c-shared` plugin ABI.

## Module graph

`plugin/go.mod` has **no** `replace` directives. Clean clones resolve
published modules via the module proxy; `plugin/go.sum` includes checksums
for `cortext.go` and CLIProxyAPI. Optional local sibling overlays use
uncommitted `go.work` from `go.work.example`.

## Optional vendor drop-ins

See [`vendor/README.md`](./vendor/README.md). Prefer `CORTEXT_ASSETS_DIR` from a
fetched release tarball over ad-hoc platform folders when possible.

## Offline / air-gapped install

1. On a networked machine: fetch `cortext-assets-<version>.tar.gz` from the
   cortext release page; record `sha256`.
2. Unpack to a private directory; set `CORTEXT_ASSETS_DIR` (and optionally
   `CORTEXT_LIBRARY_PATH`) before starting CPA.
3. Build with `make build-native` on the target OS/arch (or cross-build with
   appropriate CGO toolchain).
4. If native open fails, the plugin **fail-opens** (proxy continues without
   memory) and increments `open_fail` in logs — do not treat a silent
   pass-through as a healthy native deploy.

## Provenance checklist for a release build

1. Record `github.com/augmem/cortext.go` version and `AssetsVersion`.
2. Record plugin git SHA and `pluginVersion` + `engineFlavor` (`native`).
3. Record `sha256` of the plugin shared library (`bin/cortext.*`).
4. Record cortext assets tarball version + `sha256` when shipping offline bundles.
5. Attach `make test`, `make test-race`, `make build-native`, and `make test-native` logs from a clean machine (no sibling replaces).
