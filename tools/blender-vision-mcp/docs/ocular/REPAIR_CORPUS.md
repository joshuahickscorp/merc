# Phase T — Full-runtime ocular repair corpus

Every drill: **detect → repair → verify → no global regression**.

## Categories

| Category | Drills |
| --- | --- |
| Sensor | wrong intrinsics, time skew, colour mismatch, axis mismatch |
| Tracking | identity swap, occlusion loss, false re-id |
| World memory | erased object, cross-session mismatch |
| Geometry | scale error, hidden-surface hallucination, coordinate-frame error |
| Material | roughness error, plastic/metal, lighting/material confusion |
| Browser | focus trap, scroll lag, blank first frame, DOM/pixel contradiction |
| Cinematic | empty beat, text collision, bad turn, camera freeze |

Unavailable external runtimes are attested BLOCKED with the exact reason — never
a silent physical PASS.

```bash
.venv/bin/python scripts/run-ocular-repair.py --output artifacts/ocular/repair
```
