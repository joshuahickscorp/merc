# Open-source license inventory

> **DRAFT · INTERNAL · NOT LEGAL ADVICE**
>
> Not reviewed by counsel. Does not constitute legal approval or compliance.
>
> **GENERATED** from the real dependency graph by `scripts/generate-license-inventory.py`. Do not hand-edit. Regenerate with that script. This is not a license clearance.

- Generated at: `2026-08-16T23:54:21Z`
- Source commit: `a5bca8c0abcfda4158f5c681fa67f5ae5ebccb05`
- Status: `GENERATED_DRAFT_NOT_APPROVAL`

## Graph binding

| Manifest | SHA-256 |
|---|---|
| `control/go.mod` | `6641c5a1082f921e394a03fd1e767d0533c0a78eb6241d81bdda654c0207631e` |
| `control/go.sum` | `549318fdf613e18bd4f06b7efc145e0fed5513aeb133878dc1f426b504354f84` |
| `agent/Cargo.lock` | `9fdebba0ef3baf28189837b429a079b2881bec452f24bde3d5d6f4fbb0cd0f7b` |
| `agent/Cargo.toml` | `66ad8c64ec83d2eeab6732d56c10271e0a7a309dfcc2f6e8646bfb9175ee5df9` |
| `clients/sdk/python/pyproject.toml` | `f6724cb1af5cc59632741e7804f083cd51df6da3e70e9c2e4c89adf1af9ef01a` |
| `clients/sdk/typescript/package.json` | `fa3a73b1dc6fa6564b3d6665b1ee2d258d7c7f05548a979b1b91d6d60eae4593` |
| `clients/sdk/typescript/package-lock.json` | `87ed05eea37a6bbe4d82fa5ce0d0737a610c7f570d9ed35d6c88b9737d26db52` |

## Counts

- Go modules in `control/go.mod`: 25
- Go `go.sum` module versions (including test-only checksums): 33
- `go.sum` versions not listed in `go.mod`: 8
- Cargo packages in `agent/Cargo.lock`: 403
- Python packages: 1 (first-party SDK, zero runtime deps)
- npm packages in the TypeScript lock: 2
- Incompatible copyleft in this software graph: 0
- Undeclared / unclassified: 0

Model weights (Llama 3.2, MiniLM), Geist fonts, and visual assets are
**not** in this inventory. See `docs/THIRD_PARTY_LICENSES.md` — status
**INCOMPLETE / RELEASE BLOCKING**. Those rows stay BLOCKED.

## How Merc is distributed at backend alpha

- Control plane: proprietary. Not published as source. Not a public download.
- Agent (`agent/`): Apache-2.0. May be given to operator-known suppliers.
- Python / TypeScript SDKs: Apache-2.0. Not required to run the backend alpha.
- No public website, no public package registry publish, no live-money binary.

## Components

| Ecosystem | Name | Version | Relation | Concluded SPDX | Compatibility |
|---|---|---|---|---|---|
| go | `github.com/google/uuid` | `v1.6.0` | direct | BSD-3-Clause | COMPATIBLE_PERMISSIVE |
| go | `github.com/jackc/pgx/v5` | `v5.10.0` | direct | MIT | COMPATIBLE_PERMISSIVE |
| go | `github.com/minio/minio-go/v7` | `v7.2.0` | direct | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| go | `go.yaml.in/yaml/v3` | `v3.0.4` | direct | MIT AND Apache-2.0 | COMPATIBLE_PERMISSIVE |
| go | `golang.org/x/crypto` | `v0.52.0` | direct | BSD-3-Clause | COMPATIBLE_PERMISSIVE |
| go | `golang.org/x/sync` | `v0.21.0` | direct | BSD-3-Clause | COMPATIBLE_PERMISSIVE |
| go | `github.com/cespare/xxhash/v2` | `v2.3.0` | indirect | MIT | COMPATIBLE_PERMISSIVE |
| go | `github.com/dustin/go-humanize` | `v1.0.1` | indirect | MIT | COMPATIBLE_PERMISSIVE |
| go | `github.com/jackc/pgpassfile` | `v1.0.0` | indirect | MIT | COMPATIBLE_PERMISSIVE |
| go | `github.com/jackc/pgservicefile` | `v0.0.0-20240606120523-5a60cdf6a761` | indirect | MIT | COMPATIBLE_PERMISSIVE |
| go | `github.com/jackc/puddle/v2` | `v2.2.2` | indirect | MIT | COMPATIBLE_PERMISSIVE |
| go | `github.com/klauspost/compress` | `v1.18.6` | indirect | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| go | `github.com/klauspost/cpuid/v2` | `v2.2.11` | indirect | MIT | COMPATIBLE_PERMISSIVE |
| go | `github.com/klauspost/crc32` | `v1.3.0` | indirect | BSD-3-Clause | COMPATIBLE_PERMISSIVE |
| go | `github.com/kr/text` | `v0.2.0` | indirect | MIT | COMPATIBLE_PERMISSIVE |
| go | `github.com/minio/crc64nvme` | `v1.1.1` | indirect | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| go | `github.com/minio/md5-simd` | `v1.1.2` | indirect | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| go | `github.com/philhofer/fwd` | `v1.2.0` | indirect | MIT | COMPATIBLE_PERMISSIVE |
| go | `github.com/rs/xid` | `v1.6.0` | indirect | MIT | COMPATIBLE_PERMISSIVE |
| go | `github.com/tinylib/msgp` | `v1.6.4` | indirect | MIT | COMPATIBLE_PERMISSIVE |
| go | `github.com/zeebo/xxh3` | `v1.1.0` | indirect | BSD-2-Clause | COMPATIBLE_PERMISSIVE |
| go | `golang.org/x/net` | `v0.55.0` | indirect | BSD-3-Clause | COMPATIBLE_PERMISSIVE |
| go | `golang.org/x/sys` | `v0.45.0` | indirect | BSD-3-Clause | COMPATIBLE_PERMISSIVE |
| go | `golang.org/x/text` | `v0.39.0` | indirect | BSD-3-Clause | COMPATIBLE_PERMISSIVE |
| go | `gopkg.in/ini.v1` | `v1.67.2` | indirect | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| cargo | `adler2` | `2.0.1` | dependency | 0BSD OR MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `aead` | `0.5.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `aes` | `0.8.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `aes-gcm` | `0.10.3` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `ahash` | `0.8.12` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `aho-corasick` | `1.1.4` | dependency | Unlicense OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `allocator-api2` | `0.2.21` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `anstream` | `1.0.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `anstyle` | `1.0.14` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `anstyle-parse` | `1.0.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `anstyle-query` | `1.1.5` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `anstyle-wincon` | `3.0.11` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `anyhow` | `1.0.103` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `asn1-rs` | `0.7.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `asn1-rs-derive` | `0.6.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `asn1-rs-impl` | `0.2.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `async-compression` | `0.4.42` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `async-trait` | `0.1.89` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `atomic-waker` | `1.1.2` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `autocfg` | `1.5.1` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `base64` | `0.13.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `base64` | `0.22.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `bit-set` | `0.8.0` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `bit-vec` | `0.8.0` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `bit-vec` | `0.9.1` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `bitflags` | `1.3.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `bitflags` | `2.13.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `block` | `0.1.6` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `block-buffer` | `0.10.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `block2` | `0.6.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `bumpalo` | `3.20.3` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `bytemuck` | `1.25.0` | dependency | Zlib OR Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `bytemuck_derive` | `1.10.2` | dependency | Zlib OR Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `byteorder` | `1.5.0` | dependency | Unlicense OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `bytes` | `1.12.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `candle-core` | `0.10.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `candle-metal-kernels` | `0.10.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `candle-nn` | `0.10.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `candle-transformers` | `0.10.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `candle-ug` | `0.10.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `castaway` | `0.2.4` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `cc` | `1.2.65` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `cfg-if` | `1.0.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `cfg_aliases` | `0.2.1` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `chacha20` | `0.10.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `cipher` | `0.4.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `clap` | `4.6.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `clap_builder` | `4.6.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `clap_derive` | `4.6.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `clap_lex` | `1.1.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `colorchoice` | `1.0.5` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `compact_str` | `0.9.1` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `compression-codecs` | `0.4.38` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `compression-core` | `0.4.32` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `console` | `0.16.3` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `cookie` | `0.18.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `cookie_store` | `0.22.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `core-foundation` | `0.9.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `core-foundation-sys` | `0.8.7` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `core-graphics-types` | `0.1.3` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `cpufeatures` | `0.2.17` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `cpufeatures` | `0.3.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `crc32fast` | `1.5.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `crossbeam-deque` | `0.8.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `crossbeam-epoch` | `0.9.20` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `crossbeam-utils` | `0.8.21` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `crunchy` | `0.2.4` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `crypto-common` | `0.1.7` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `ctr` | `0.9.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `daachorse` | `1.0.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `darling` | `0.20.11` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `darling_core` | `0.20.11` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `darling_macro` | `0.20.11` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `dary_heap` | `0.3.9` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `data-encoding` | `2.11.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `der-parser` | `10.0.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `deranged` | `0.5.8` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `derive_builder` | `0.20.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `derive_builder_core` | `0.20.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `derive_builder_macro` | `0.20.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `digest` | `0.10.7` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `dirs` | `6.0.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `dirs-sys` | `0.5.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `dispatch2` | `0.3.1` | dependency | Zlib OR Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `displaydoc` | `0.2.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `document-features` | `0.2.12` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `dyn-stack` | `0.13.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `dyn-stack-macros` | `0.1.3` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `either` | `1.16.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `encode_unicode` | `1.0.0` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `enum-as-inner` | `0.6.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `equivalent` | `1.0.2` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `errno` | `0.3.14` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `esaxx-rs` | `0.1.10` | dependency | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| cargo | `fancy-regex` | `0.17.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `find-msvc-tools` | `0.1.9` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `flate2` | `1.1.9` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `float8` | `0.7.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `fnv` | `1.0.7` | dependency | Apache-2.0  OR  MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `foldhash` | `0.2.0` | dependency | Zlib | COMPATIBLE_PERMISSIVE |
| cargo | `foreign-types` | `0.5.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `foreign-types-macros` | `0.2.3` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `foreign-types-shared` | `0.3.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `form_urlencoded` | `1.2.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `futures-channel` | `0.3.32` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `futures-core` | `0.3.32` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `futures-io` | `0.3.32` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `futures-macro` | `0.3.32` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `futures-sink` | `0.3.32` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `futures-task` | `0.3.32` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `futures-util` | `0.3.32` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `gemm` | `0.18.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm` | `0.19.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm-c32` | `0.18.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm-c32` | `0.19.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm-c64` | `0.18.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm-c64` | `0.19.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm-common` | `0.18.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm-common` | `0.19.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm-f16` | `0.18.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm-f16` | `0.19.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm-f32` | `0.18.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm-f32` | `0.19.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm-f64` | `0.18.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `gemm-f64` | `0.19.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `generic-array` | `0.14.7` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `getrandom` | `0.2.17` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `getrandom` | `0.3.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `getrandom` | `0.4.3` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `ghash` | `0.5.1` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `half` | `2.7.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `hashbrown` | `0.16.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `hashbrown` | `0.17.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `heck` | `0.5.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `hermit-abi` | `0.5.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `hf-hub` | `0.5.0` | dependency | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| cargo | `hmac` | `0.12.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `http` | `1.4.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `http-body` | `1.0.1` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `http-body-util` | `0.1.3` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `httparse` | `1.10.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `hyper` | `1.10.1` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `hyper-rustls` | `0.27.9` | dependency | Apache-2.0 OR ISC OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `hyper-util` | `0.1.20` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `icu_collections` | `2.2.0` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `icu_locale_core` | `2.2.0` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `icu_normalizer` | `2.2.0` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `icu_normalizer_data` | `2.2.0` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `icu_properties` | `2.2.0` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `icu_properties_data` | `2.2.0` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `icu_provider` | `2.2.0` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `ident_case` | `1.0.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `idna` | `1.1.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `idna_adapter` | `1.2.2` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `indexmap` | `2.14.0` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `indicatif` | `0.18.4` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `inout` | `0.1.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `ipnet` | `2.12.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `is_terminal_polyfill` | `1.70.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `itertools` | `0.14.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `itoa` | `1.0.18` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `js-sys` | `0.3.102` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `lazy_static` | `1.5.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `libc` | `0.2.186` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `libloading` | `0.8.9` | dependency | ISC | COMPATIBLE_PERMISSIVE |
| cargo | `libm` | `0.2.16` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `libredox` | `0.1.17` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `litemap` | `0.8.2` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `litrs` | `1.0.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `log` | `0.4.33` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `lru-slab` | `0.1.2` | dependency | MIT OR Apache-2.0 OR Zlib | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `macro_rules_attribute` | `0.2.2` | dependency | Apache-2.0 OR MIT OR Zlib | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `macro_rules_attribute-proc_macro` | `0.2.2` | dependency | Apache-2.0 OR MIT OR Zlib | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `malloc_buf` | `0.0.6` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `matchers` | `0.2.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `memchr` | `2.8.2` | dependency | Unlicense OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `memmap2` | `0.9.11` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `merc-agent` | `0.1.0` | first-party | Apache-2.0 | FIRST_PARTY |
| cargo | `metal` | `0.29.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `minimal-lexical` | `0.2.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `miniz_oxide` | `0.8.9` | dependency | MIT OR Zlib OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `mio` | `1.2.1` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `monostate` | `0.1.18` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `monostate-impl` | `0.1.18` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `nom` | `7.1.3` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `ntapi` | `0.4.3` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `nu-ansi-term` | `0.50.3` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `num` | `0.4.3` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `num-bigint` | `0.4.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `num-complex` | `0.4.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `num-conv` | `0.2.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `num-integer` | `0.1.46` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `num-iter` | `0.1.45` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `num-rational` | `0.4.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `num-traits` | `0.2.19` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `num_cpus` | `1.17.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `objc` | `0.2.7` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `objc2` | `0.6.4` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `objc2-core-foundation` | `0.3.2` | dependency | Zlib OR Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `objc2-encode` | `4.1.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `objc2-foundation` | `0.3.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `objc2-metal` | `0.3.2` | dependency | Zlib OR Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `oid-registry` | `0.8.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `once_cell` | `1.21.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `once_cell_polyfill` | `1.70.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `onig` | `6.5.3` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `onig_sys` | `69.9.3` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `opaque-debug` | `0.3.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `option-ext` | `0.2.0` | dependency | MPL-2.0 | COMPATIBLE_PERMISSIVE |
| cargo | `paste` | `1.0.15` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `pem` | `3.0.6` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `percent-encoding` | `2.3.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `pin-project-lite` | `0.2.17` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `pkg-config` | `0.3.33` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `polyval` | `0.6.2` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `portable-atomic` | `1.13.1` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `potential_utf` | `0.1.5` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `powerfmt` | `0.2.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `ppv-lite86` | `0.2.21` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `proc-macro2` | `1.0.106` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `pulp` | `0.21.5` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `pulp` | `0.22.3` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `pulp-wasm-simd-flag` | `0.1.1` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `quinn` | `0.11.9` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `quinn-proto` | `0.11.16` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `quinn-udp` | `0.5.14` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `quote` | `1.0.45` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `r-efi` | `5.3.0` | dependency | MIT OR Apache-2.0 OR LGPL-2.1-or-later | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `r-efi` | `6.0.0` | dependency | MIT OR Apache-2.0 OR LGPL-2.1-or-later | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rand` | `0.8.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rand` | `0.9.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rand` | `0.10.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rand_chacha` | `0.3.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rand_chacha` | `0.9.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rand_core` | `0.6.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rand_core` | `0.9.5` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rand_core` | `0.10.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rand_distr` | `0.5.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rand_pcg` | `0.10.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `raw-cpuid` | `11.6.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `rayon` | `1.12.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rayon-cond` | `0.4.0` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rayon-core` | `1.13.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rcgen` | `0.14.8` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `reborrow` | `0.5.5` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `redox_users` | `0.5.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `regex` | `1.12.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `regex-automata` | `0.4.14` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `regex-syntax` | `0.8.11` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `reqwest` | `0.12.28` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `ring` | `0.17.14` | dependency | Apache-2.0 AND ISC | COMPATIBLE_PERMISSIVE |
| cargo | `rustc-hash` | `2.1.2` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rusticata-macros` | `4.1.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rustls` | `0.23.40` | dependency | Apache-2.0 OR ISC OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rustls-pki-types` | `1.14.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `rustls-webpki` | `0.103.13` | dependency | ISC | COMPATIBLE_PERMISSIVE |
| cargo | `rustversion` | `1.0.22` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `ryu` | `1.0.23` | dependency | Apache-2.0 OR BSL-1.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `safetensors` | `0.4.5` | dependency | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| cargo | `safetensors` | `0.7.0` | dependency | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| cargo | `same-file` | `1.0.6` | dependency | Unlicense OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `seq-macro` | `0.3.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `serde` | `1.0.228` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `serde_core` | `1.0.228` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `serde_derive` | `1.0.228` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `serde_json` | `1.0.150` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `serde_plain` | `1.0.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `serde_spanned` | `0.6.9` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `serde_urlencoded` | `0.7.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `sha2` | `0.10.9` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `sharded-slab` | `0.1.7` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `shlex` | `2.0.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `signal-hook-registry` | `1.4.8` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `simd-adler32` | `0.3.9` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `slab` | `0.4.12` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `smallvec` | `1.15.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `socket2` | `0.6.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `socks` | `0.3.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `spm_precompiled` | `0.1.4` | dependency | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| cargo | `stable_deref_trait` | `1.2.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `static_assertions` | `1.1.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `strsim` | `0.11.1` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `subtle` | `2.6.1` | dependency | BSD-3-Clause | COMPATIBLE_PERMISSIVE |
| cargo | `syn` | `2.0.118` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `sync_wrapper` | `1.0.2` | dependency | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| cargo | `synstructure` | `0.13.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `sysctl` | `0.6.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `sysinfo` | `0.31.4` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `thiserror` | `1.0.69` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `thiserror` | `2.0.18` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `thiserror-impl` | `1.0.69` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `thiserror-impl` | `2.0.18` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `thread_local` | `1.1.9` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `time` | `0.3.49` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `time-core` | `0.1.9` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `time-macros` | `0.2.29` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `tinystr` | `0.8.3` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `tinyvec` | `1.11.0` | dependency | Zlib OR Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `tinyvec_macros` | `0.1.1` | dependency | MIT OR Apache-2.0 OR Zlib | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `tokenizers` | `0.22.2` | dependency | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| cargo | `tokenizers` | `0.23.1` | dependency | Apache-2.0 | COMPATIBLE_PERMISSIVE |
| cargo | `tokio` | `1.52.3` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `tokio-macros` | `2.7.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `tokio-rustls` | `0.26.4` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `tokio-util` | `0.7.18` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `toml` | `0.8.23` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `toml_datetime` | `0.6.11` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `toml_edit` | `0.22.27` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `toml_write` | `0.1.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `tower` | `0.5.3` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `tower-http` | `0.6.11` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `tower-layer` | `0.3.3` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `tower-service` | `0.3.3` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `tracing` | `0.1.44` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `tracing-attributes` | `0.1.31` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `tracing-core` | `0.1.36` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `tracing-log` | `0.2.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `tracing-subscriber` | `0.3.23` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `try-lock` | `0.2.5` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `typed-path` | `0.12.3` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `typenum` | `1.20.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `ug` | `0.5.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `ug-metal` | `0.5.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `unicode-ident` | `1.0.24` | dependency | (MIT OR Apache-2.0) AND Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `unicode-normalization-alignments` | `0.1.12` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `unicode-segmentation` | `1.13.3` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `unicode-width` | `0.2.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `unicode_categories` | `0.1.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `unit-prefix` | `0.5.2` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `universal-hash` | `0.5.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `untrusted` | `0.9.0` | dependency | ISC | COMPATIBLE_PERMISSIVE |
| cargo | `ureq` | `3.3.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `ureq-proto` | `0.6.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `url` | `2.5.8` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `utf8-zero` | `0.8.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `utf8_iter` | `1.0.4` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `utf8parse` | `0.2.2` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `uuid` | `1.23.3` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `valuable` | `0.1.1` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `version_check` | `0.9.5` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `walkdir` | `2.5.0` | dependency | Unlicense OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `want` | `0.3.1` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `wasi` | `0.11.1+wasi-snapshot-preview1` | dependency | Apache-2.0 WITH LLVM-exception OR Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `wasip2` | `1.0.4+wasi-0.2.12` | dependency | Apache-2.0 WITH LLVM-exception OR Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `wasm-bindgen` | `0.2.125` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `wasm-bindgen-futures` | `0.4.75` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `wasm-bindgen-macro` | `0.2.125` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `wasm-bindgen-macro-support` | `0.2.125` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `wasm-bindgen-shared` | `0.2.125` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `wasm-streams` | `0.4.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `web-sys` | `0.3.102` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `web-time` | `1.1.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `webpki-roots` | `1.0.8` | dependency | CDLA-Permissive-2.0 | COMPATIBLE_PERMISSIVE |
| cargo | `winapi` | `0.3.9` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `winapi-i686-pc-windows-gnu` | `0.4.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `winapi-util` | `0.1.11` | dependency | Unlicense OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `winapi-x86_64-pc-windows-gnu` | `0.4.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows` | `0.57.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows-core` | `0.57.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows-implement` | `0.57.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows-interface` | `0.57.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows-link` | `0.2.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows-result` | `0.1.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows-sys` | `0.52.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows-sys` | `0.60.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows-sys` | `0.61.2` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows-targets` | `0.52.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows-targets` | `0.53.5` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_aarch64_gnullvm` | `0.52.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_aarch64_gnullvm` | `0.53.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_aarch64_msvc` | `0.52.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_aarch64_msvc` | `0.53.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_i686_gnu` | `0.52.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_i686_gnu` | `0.53.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_i686_gnullvm` | `0.52.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_i686_gnullvm` | `0.53.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_i686_msvc` | `0.52.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_i686_msvc` | `0.53.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_x86_64_gnu` | `0.52.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_x86_64_gnu` | `0.53.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_x86_64_gnullvm` | `0.52.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_x86_64_gnullvm` | `0.53.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_x86_64_msvc` | `0.52.6` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `windows_x86_64_msvc` | `0.53.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `winnow` | `0.7.15` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `wit-bindgen` | `0.57.1` | dependency | Apache-2.0 WITH LLVM-exception OR Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `writeable` | `0.6.3` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `x509-parser` | `0.18.1` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `yasna` | `0.6.0` | dependency | MIT OR Apache-2.0 | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `yoke` | `0.7.5` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `yoke` | `0.8.3` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `yoke-derive` | `0.7.5` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `yoke-derive` | `0.8.2` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `zerocopy` | `0.8.52` | dependency | BSD-2-Clause OR Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `zerocopy-derive` | `0.8.52` | dependency | BSD-2-Clause OR Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `zerofrom` | `0.1.8` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `zerofrom-derive` | `0.1.7` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `zeroize` | `1.9.0` | dependency | Apache-2.0 OR MIT | COMPATIBLE_PERMISSIVE_OPTION |
| cargo | `zerotrie` | `0.2.4` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `zerovec` | `0.11.6` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `zerovec-derive` | `0.11.3` | dependency | Unicode-3.0 | COMPATIBLE_PERMISSIVE |
| cargo | `zip` | `7.2.0` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| cargo | `zmij` | `1.0.21` | dependency | MIT | COMPATIBLE_PERMISSIVE |
| python | `merc` | `dynamic` | first-party | ["LICENSE"] | FIRST_PARTY |
| npm | `merc` | `0.1.0` | first-party | Apache-2.0 | FIRST_PARTY |
| npm | `typescript` | `5.9.3` | devDependency | Apache-2.0 | COMPATIBLE_PERMISSIVE |

## `go.sum` versions not in `go.mod`

These are checksum-only (typically test or historical). They are not
the build graph. Licenses were not fetched for them.

| Module | Version |
|---|---|
| `github.com/davecgh/go-spew` | `v1.1.1` |
| `github.com/kr/pretty` | `v0.3.0` |
| `github.com/pmezard/go-difflib` | `v1.0.0` |
| `github.com/rogpeppe/go-internal` | `v1.14.1` |
| `github.com/stretchr/testify` | `v1.11.1` |
| `github.com/zeebo/assert` | `v1.3.0` |
| `gopkg.in/check.v1` | `v1.0.0-20201130134442-10cb98267c6c` |
| `gopkg.in/yaml.v3` | `v3.0.1` |
