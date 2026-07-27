# Ocular remote benchmark (Phase K)

Builder path uses the governed self-captured train images under
`artifacts/v2/object-benchmarks/remote/capture/images/train/` when present, or
an explicit `--train-dir`. Ground truth is evaluator-only.

```bash
.venv/bin/python scripts/run-ocular-remote.py --output artifacts/ocular/remote
```
