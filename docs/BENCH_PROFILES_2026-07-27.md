# Benchmark profiles

- hardware: **Apple M3 Ultra / 96GB**
- runtime: **mlx**
- model: **mlx-community/Llama-3.2-1B-Instruct-4bit**
- quality: greedy argmax decoding, no sampling; outputs not scored for quality
- energy: **NOT MEASURED - powermetrics requires root; re-run under sudo for energy**
- market price: $0.00003600/1k units (confidence-weighted median x positioning multiplier)
- supplier share: 97%, electricity $0.15/kWh
- **energy columns are null by design**: this harness does not estimate watts and present them as measurement.

## INTERACTIVE

| batch | prompt | out | prefix | TTFT ms | ITL ms | PHYSICAL tok/s | delivered tok/s | reuse | J / M tok | supplier floor /1k | CX floor /1k | contribution $/hr | err |
|---:|---:|---:|:--|---:|---:|---:|---:|---:|---:|---:|---:|---:|:--|
| 1 | 32 | 64 | COLD | 30 | 2.68 | 476 | 476 | 1.00x | n/a | $0.00000271 | $0.00000279 | $0.0553* | 0 |
| 1 | 128 | 64 | COLD | 62 | 2.98 | 760 | 760 | 1.00x | n/a | $0.00000169 | $0.00000175 | $0.0911* | 0 |
| 1 | 128 | 64 | COLD | 25 | 2.68 | 978 | 978 | 1.00x | n/a | $0.00000132 | $0.00000136 | $0.1184* | 0 |
| 1 | 512 | 64 | COLD | 81 | 2.85 | 2185 | 2185 | 1.00x | n/a | $0.00000059 | $0.00000061 | $0.2702* | 0 |
| 1 | 2048 | 64 | COLD | 312 | 3.27 | 4045 | 4045 | 1.00x | n/a | $0.00000032 | $0.00000033 | $0.5040* | 0 |

## BATCH

| batch | prompt | out | prefix | TTFT ms | ITL ms | PHYSICAL tok/s | delivered tok/s | reuse | J / M tok | supplier floor /1k | CX floor /1k | contribution $/hr | err |
|---:|---:|---:|:--|---:|---:|---:|---:|---:|---:|---:|---:|---:|:--|
| 1 | 128 | 32 | COLD | 43 | 2.71 | 1235 | 1235 | 1.00x | n/a | $0.00000104 | $0.00000108 | $0.1508* | 0 |
| 8 | 128 | 32 | COLD | 153 | 5.22 | 3993 | 3993 | 1.00x | n/a | $0.00000032 | $0.00000033 | $0.4974* | 0 |
| 32 | 128 | 32 | COLD | 585 | 9.33 | 5797 | 5797 | 1.00x | n/a | $0.00000022 | $0.00000023 | $0.7243* | 0 |
| 64 | 64 | 16 | COLD | 603 | 13.82 | 6215 | 6215 | 1.00x | n/a | $0.00000021 | $0.00000021 | $0.7768* | 0 |
| 64 | 64 | 64 | COLD | 584 | 13.74 | 5596 | 5596 | 1.00x | n/a | $0.00000023 | $0.00000024 | $0.6990* | 0 |
| 64 | 64 | 256 | COLD | 588 | 14.26 | 4832 | 4832 | 1.00x | n/a | $0.00000027 | $0.00000027 | $0.6029* | 0 |
| 64 | 128 | 32 | COLD | 1210 | 14.04 | 6171 | 6171 | 1.00x | n/a | $0.00000021 | $0.00000022 | $0.7713* | 0 |
| 64 | 128 | 32 | COLD | 1204 | 13.87 | 6216 | 6216 | 1.00x | n/a | $0.00000021 | $0.00000021 | $0.7769* | 0 |
| 64 | 256 | 16 | COLD | 2412 | 16.30 | 6512 | 6512 | 1.00x | n/a | $0.00000020 | $0.00000020 | $0.8142* | 0 |
| 64 | 256 | 64 | COLD | 2341 | 14.99 | 6205 | 6205 | 1.00x | n/a | $0.00000021 | $0.00000021 | $0.7755* | 0 |
| 64 | 256 | 256 | COLD | 2357 | 15.04 | 5279 | 5279 | 1.00x | n/a | $0.00000024 | $0.00000025 | $0.6592* | 0 |
| 64 | 1024 | 16 | COLD | 10041 | 30.72 | 6319 | 6319 | 1.00x | n/a | $0.00000020 | $0.00000021 | $0.7899* | 0 |
| 64 | 1024 | 64 | COLD | 9735 | 22.21 | 6241 | 6241 | 1.00x | n/a | $0.00000021 | $0.00000021 | $0.7801* | 0 |
| 64 | 1024 | 256 | COLD | 9718 | 20.59 | 5465 | 5465 | 1.00x | n/a | $0.00000024 | $0.00000024 | $0.6825* | 0 |
| 128 | 128 | 32 | COLD | 2517 | 25.45 | 6148 | 6148 | 1.00x | n/a | $0.00000021 | $0.00000022 | $0.7684* | 0 |
| 256 | 128 | 32 | COLD | 5056 | 48.80 | 6189 | 6189 | 1.00x | n/a | $0.00000021 | $0.00000021 | $0.7736* | 0 |

## SHARED_PREFIX_BATCH

| batch | prompt | out | prefix | TTFT ms | ITL ms | PHYSICAL tok/s | delivered tok/s | reuse | J / M tok | supplier floor /1k | CX floor /1k | contribution $/hr | err |
|---:|---:|---:|:--|---:|---:|---:|---:|---:|---:|---:|---:|---:|:--|
| 64 | 192 | 32 | SHARED(128) | 622 | 14.26 | 5816 | 13294 | 2.29x | n/a | $0.00000022 | $0.00000023 | $0.7266* | 0 |
| 128 | 192 | 32 | COLD | 3669 | 25.95 | 6372 | 6372 | 1.00x | n/a | $0.00000020 | $0.00000021 | $0.7965* | 0 |
| 128 | 192 | 32 | SHARED(32) | 3085 | 26.02 | 6282 | 7319 | 1.17x | n/a | $0.00000021 | $0.00000021 | $0.7852* | 0 |
| 128 | 192 | 32 | SHARED(64) | 2506 | 26.35 | 6134 | 8561 | 1.40x | n/a | $0.00000021 | $0.00000022 | $0.7666* | 0 |
| 128 | 192 | 32 | SHARED(128) | 1415 | 26.54 | 5482 | 12660 | 2.31x | n/a | $0.00000024 | $0.00000024 | $0.6847* | 0 |
| 128 | 192 | 32 | SHARED(160) | 736 | 25.51 | 5380 | 18469 | 3.43x | n/a | $0.00000024 | $0.00000025 | $0.6718* | 0 |

`*` contribution derived from the documented 30 W figure, **not** a measurement. Re-run under `sudo` for real energy.

## Best observed profile

**6512 tok/s** PHYSICAL (6512 delivered, 1.00x reuse) - BATCH, batch 64, 256 prompt / 16 output, prefix COLD, mlx on Apple M3 Ultra / 96GB.

At the market price that is $0.8142/hr of supplier contribution after electricity (assumed power).
