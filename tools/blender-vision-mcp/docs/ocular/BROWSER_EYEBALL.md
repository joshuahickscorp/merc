# Phase N — Browser and screen eyeball

## Unified interface

`blender_vision.ocular.browser_eyeball.BrowserEyeball` binds:

| Surface | Field |
| --- | --- |
| Pixels | `pixels_digest`, `pixels_shape` |
| DOM | `dom_nodes` |
| Accessibility tree | `accessibility` |
| Computed style | `computed_styles` |
| Animation state | `animations` |
| WebGL / canvas | `webgl` |
| Network | `network` |

Existing VisionMCP browser tools (`perception.browser.BrowserAdapter`) are
**preserved** and unchanged.

## Contradiction detectors

1. DOM visible, pixels missing  
2. Pixels without semantics  
3. Canvas scroll trap  
4. Focus-order mismatch  
5. Loading-state stall  
6. Source state ≠ rendered state  
7. Browser scene ≠ Blender export  

## Browser cap

Serialize via `scripts/with-one-browser.sh` (reentrant). Target one browser;
never launch in parallel. Use Playwright `channel="chrome"` (bundled headless
shell is not installed on this host).

## Run

```bash
.venv/bin/python scripts/run-ocular-browser.py --output artifacts/ocular/browser
scripts/with-one-browser.sh .venv/bin/python scripts/run-ocular-browser.py \
  --output artifacts/ocular/browser --physical \
  --url file://$PWD/tests/fixtures/web/static/index.html
```
