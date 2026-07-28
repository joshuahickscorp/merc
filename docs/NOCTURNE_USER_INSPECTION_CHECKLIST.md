# NOCTURNE/ONE User Inspection Checklist

Open `http://127.0.0.1:4173` while the acceptance server remains running.

- [ ] Visual quality: confirm the poster-first hero, typography, spacing, contrast, and NOCTURNE/ONE visual language feel intentional.
- [ ] Desktop composition: inspect the home, Technology, Configurator, Reserve, and Receipt routes at a wide desktop window.
- [ ] Mobile composition: resize to roughly 390×844; confirm there is no horizontal overflow and controls remain legible and usable.
- [ ] Real 3D: on Home, choose **Enter 3D**; confirm the poster yields to the interactive product and the model can be orbited.
- [ ] 3D quality: inspect the shell, glass core, eclipse disk, drivers, base, rotary control, grille, board, frame, membrane, and cable for clean geometry.
- [ ] Material switching: in Configurator, change the finish/material variant and confirm the rendered product and saved configuration agree.
- [ ] Exploded animation: enter 3D, select `glass_core` in the part selector, and confirm a coherent exploded transition with meaningful component separation.
- [ ] Responsiveness: repeat Home → Configurator → Reserve → Receipt on desktop and mobile widths.
- [ ] Reservation flow: save a configuration, enter an email on Reserve, submit, and confirm a reservation ID/status appears.
- [ ] Persistence: reload Configurator and Receipt; confirm the selected configuration and reservation are restored.
- [ ] Keyboard accessibility: Tab through the skip link, navigation, Enter 3D, configurator inputs, and reservation form; confirm visible focus and sensible labels. Note that the independent H4 candidate—not this accepted H3 app—failed one frozen eight-Tab ordering check.
- [ ] Reduced motion: enable the operating system/browser reduced-motion preference, reload, and confirm auto-animation is suppressed without hiding content.
- [ ] Fallback behavior: with WebGL disabled or unavailable, confirm the product poster and readable unavailable message remain usable.
- [ ] Network recovery: throttle or interrupt the network after loading; confirm poster-first behavior and Retry recovery.
- [ ] Perceived smoothness: with real 3D loaded, orbit, switch materials, and run the exploded view; note any visible stalls or input lag.

Evidence screenshots are in `artifacts/live-sandbox/live-acceptance-20260726T052610Z/screenshots/`, including desktop/mobile poster, real-3D, configured, exploded, reduced-motion, no-WebGL, slow-network, and receipt states.
