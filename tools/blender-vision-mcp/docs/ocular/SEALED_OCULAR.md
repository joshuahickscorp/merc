# Phase S — Sealed ocular benchmarks

Eight targets with oracle / builder / evaluator isolation:

1. live_tabletop  
2. remote  
3. reflective_transparent  
4. browser_page  
5. data_centre  
6. dynamic_room_memory  
7. soft_object  
8. organic_fur  

## Single split authority

`SplitAuthority` is the only source of train/hidden indices. After `seal()`,
digests bind the index lists. Builder inputs carry **train indices only**.
Recomputing hidden indices from totals is a contract violation.

## Leakage canaries

Each target stores a secret `OCULAR-CANARY-…` under `oracle/hidden/`. Probes
verify:

- builder cannot path-resolve oracle canaries  
- canary text absent from `builder_inputs/`  
- train list matches sealed split; hidden not in builder payload  
- `assert_builder_may_read(hidden_index)` raises  

```bash
.venv/bin/python scripts/run-ocular-sealed.py --output artifacts/ocular/sealed
```
