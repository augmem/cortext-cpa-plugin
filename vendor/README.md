# Vendored native Cortext runtime

This plugin loads Cortext through the official Go cgo binding
(`github.com/augmem/cortext/bindings/go` or a local checkout). The binding links
against a platform `libcortext` built from
[augmem/cortext](https://github.com/augmem/cortext).

## Preferred: build from a local cortext checkout

```bash
# From the cortext repository root:
cmake --preset ffi-release
cmake --build --preset ffi-release --target cortext

# Or zig:
# zig build -Doptimize=ReleaseFast
```

Then point the plugin build at that tree (see the root `Makefile`):

```bash
export CORTEXT_ROOT=/path/to/cortext
make build
```

## Optional: drop binaries under vendor/

For offline / Git-clone installs (Hermes-style), place platform artifacts here:

```text
vendor/
  darwin-arm64/libcortext.dylib
  darwin-x64/libcortext.dylib
  linux-x64/libcortext.so
  linux-arm64/libcortext.so
  windows-x64/cortext.dll
  models/…                 # encoder assets if required by your build
```

Checksum and provenance notes belong in a future `PROVENANCE.md` once release
artifacts are published for this plugin.

The plugin never downloads a library at runtime.
