# Vendored / offline Cortext runtime (optional)

Production builds use [`github.com/augmem/cortext.go`](https://github.com/augmem/cortext.go),
which loads platform natives from the [augmem/cortext](https://github.com/augmem/cortext)
release asset tarball (downloaded on first open).

## Preferred offline path

```bash
# From a cortext.go checkout:
./scripts/fetch_assets.sh /path/to/assets
export CORTEXT_ASSETS_DIR=/path/to/assets
make -C /path/to/cortext-cpa-plugin build-native
```

Or set `CORTEXT_LIBRARY_PATH` to an explicit `libcortext` shared library.

## Legacy drop-in layout

For Hermes-style Git-clone installs you may still place platform artifacts here:

```text
vendor/
  darwin-arm64/libcortext.dylib
  darwin-x64/libcortext.dylib
  linux-x64/libcortext.so
  linux-arm64/libcortext.so
  windows-x64/cortext.dll
  models/…                 # encoder assets if required by your layout
```

Then point `CORTEXT_LIBRARY_PATH` (and optionally `CORTEXT_AIST_MODEL_PATH`) at
those files. The plugin never downloads a library itself — `cortext.go` owns
asset fetch when env overrides are unset.

Checksum and provenance notes: root [`PROVENANCE.md`](../PROVENANCE.md).
