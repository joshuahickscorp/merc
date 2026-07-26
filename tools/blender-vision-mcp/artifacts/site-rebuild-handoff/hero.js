// hero.js · the live tabletop hero. Loads oracles.glb (the two devices built in
// render/build_scene.py, same geometry as the Cycles stills) and lights it to match:
// one soft key high and camera-left, a dim rim behind and above, a low fill. The
// camera is a locked pitch band you can drag horizontally to orbit within ~40 degrees
// and it eases back to rest. Hover lifts a device 3 percent and fades in its spec
// label. WebGL failure falls back to the Cycles still (index.html handles the swap).
//
// Self-hosted Three via the page importmap · no CDN at runtime.
import * as THREE from 'three';
import { GLTFLoader } from 'three/addons/loaders/GLTFLoader.js';

const REST_YAW = 0;          // rest camera azimuth (radians)
const YAW_RANGE = 0.35;      // +/- ~20 degrees of horizontal orbit (about 40 deg arc)
const PITCH = 0.63;          // ~36 degrees down onto the desk (elevation of the camera)
const PITCH_RANGE = 0.05;    // a few degrees of vertical give, clamped
const DIST = 0.92;           // camera distance in metres (scene is metric)
const TARGET = new THREE.Vector3(0, 0.03, 0);

export function mountHero(canvas, opts) {
  opts = opts || {};
  const onFail = opts.onFail || function () {};
  const labelEls = opts.labels || {};       // { 'mac-studio': el, 'dgx-spark': el }
  const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  let renderer;
  try {
    renderer = new THREE.WebGLRenderer({ canvas, antialias: true, alpha: true });
    if (!renderer.getContext()) throw new Error('no webgl context');
  } catch (e) {
    onFail(e);
    return null;
  }

  renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
  renderer.toneMapping = THREE.ACESFilmicToneMapping;
  renderer.toneMappingExposure = 1.0;
  renderer.outputColorSpace = THREE.SRGBColorSpace;
  renderer.shadowMap.enabled = true;
  renderer.shadowMap.type = THREE.PCFSoftShadowMap;

  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(30, 1, 0.05, 50);

  // Abstract dark gradient environment (NOT a photographic HDRI): a tiny vertical
  // gradient so metals reflect soft tone, not a studio. Built on a canvas, run
  // through PMREM for correct roughness response.
  const env = makeGradientEnv(renderer);
  scene.environment = env;

  // Lights mirror the Blender key/rim/fill.
  const key = new THREE.DirectionalLight(0xfffdf8, 2.6);
  key.position.set(-0.9, 1.15, 0.7);
  key.castShadow = true;
  key.shadow.mapSize.set(2048, 2048);
  key.shadow.camera.near = 0.1; key.shadow.camera.far = 4;
  key.shadow.camera.left = -0.6; key.shadow.camera.right = 0.6;
  key.shadow.camera.top = 0.6; key.shadow.camera.bottom = -0.6;
  key.shadow.bias = -0.0004; key.shadow.radius = 6;
  scene.add(key);

  const rim = new THREE.DirectionalLight(0xeef4ff, 1.1);
  rim.position.set(0.55, 0.95, -1.1);
  scene.add(rim);

  const fill = new THREE.DirectionalLight(0xf5f8ff, 0.35);
  fill.position.set(1.15, 0.5, 0.9);
  scene.add(fill);

  // Shadow-catcher desk: matte, near-black, receives the contact shadow. It reads
  // as the same desk as the still but stays dark enough to sit in the site void.
  const desk = new THREE.Mesh(
    new THREE.PlaneGeometry(8, 8),
    new THREE.ShadowMaterial({ opacity: 0.5 })
  );
  desk.rotation.x = -Math.PI / 2;
  desk.receiveShadow = true;
  scene.add(desk);

  // Device groups for hover: everything whose name starts mac-studio / dgx-spark / tub.
  const groups = { 'mac-studio': new THREE.Group(), 'dgx-spark': new THREE.Group() };
  scene.add(groups['mac-studio'], groups['dgx-spark']);
  const baseEmissive = new WeakMap();

  let loaded = false;
  const loader = new GLTFLoader();
  loader.load('/assets/site/oracles.glb', (gltf) => {
    // Collect meshes FIRST · reparenting (attach) during traverse mutates the tree
    // mid-iteration and corrupts it.
    const meshes = [];
    gltf.scene.traverse((o) => { if (o.isMesh) meshes.push(o); });
    for (const o of meshes) {
      o.castShadow = true;
      o.receiveShadow = true;
      const g = o.name.startsWith('dgx-spark') || o.name.startsWith('tub') ? 'dgx-spark' : 'mac-studio';
      groups[g].attach(o);
      if (o.material) {
        o.material = o.material.clone();
        baseEmissive.set(o.material, (o.material.emissive && o.material.emissive.clone()) || new THREE.Color(0, 0, 0));
      }
    }
    loaded = true;
    frame();
  }, undefined, (err) => onFail(err));

  // ---- camera rig · scroll scrubs a 5-beat path, drag adds a temporary offset -------
  // Each beat is a camera state: target point, distance, pitch, exposure. Scroll
  // progress (0..1) lerps between adjacent beats with smoothstep. Drag adds a yaw and
  // pitch offset on top that eases back to zero on release, so the two never fight.
  const STUDIO_X = -0.135;               // device x positions in the yup glb (metres)
  const SPARK_X = 0.159;
  const BEATS = [
    // p,   tx,        ty,    tz,   dist, pitch, exp   · what the viewer is on
    [0.00, 0.012, 0.03, 0.0, 0.92, 0.63, 1.00], // 1 arrival · both, tabletop
    [0.28, 0.012, 0.02, 0.0, 0.80, 0.50, 1.00], // 2 how · lower, closer two-shot
    [0.52, 0.012, 0.11, 0.0, 0.98, 0.60, 0.42], // 3 monument · drift to void, devices dim
    [0.76, STUDIO_X, 0.03, 0.0, 0.60, 0.48, 1.00], // 4 earn · dolly to the Mac Studio
    [1.00, 0.012, 0.06, 0.0, 1.30, 0.72, 1.00], // 5 release · rise away, ease out
  ];

  let scrollP = 0;                       // set by the scroll listener (scalar only)
  let dragYaw = 0, dragPitch = 0;        // drag target offsets, decay to 0 on release
  let dispYaw = 0, dispPitch = 0;        // displayed offsets, eased toward the target
  let dragging = false, lastX = 0, lastY = 0, released = 0;
  let idlePhase = 0;
  const camT = new THREE.Vector3();      // reused, no per-frame allocation

  function smoothstep(t) { return t * t * (3 - 2 * t); }

  function beatState(p) {
    // find the segment and interpolate with smoothstep
    let i = 0;
    while (i < BEATS.length - 1 && p > BEATS[i + 1][0]) i++;
    const a = BEATS[i], b = BEATS[Math.min(i + 1, BEATS.length - 1)];
    const span = (b[0] - a[0]) || 1;
    const s = smoothstep(Math.max(0, Math.min(1, (p - a[0]) / span)));
    return {
      tx: a[1] + (b[1] - a[1]) * s, ty: a[2] + (b[2] - a[2]) * s, tz: a[3] + (b[3] - a[3]) * s,
      dist: a[4] + (b[4] - a[4]) * s, pitch: a[5] + (b[5] - a[5]) * s, exp: a[6] + (b[6] - a[6]) * s,
    };
  }

  function placeCamera() {
    // reduced-motion: snap scroll to the nearest beat centre (no continuous scrub)
    let p = scrollP;
    if (reduceMotion) {
      let best = 0, bd = 1;
      for (let i = 0; i < BEATS.length; i++) {
        const d = Math.abs(scrollP - BEATS[i][0]);
        if (d < bd) { bd = d; best = BEATS[i][0]; }
      }
      p = best;
    }
    const st = beatState(p);
    const cy = st.pitch + dispPitch;
    const yaw = dispYaw;
    camT.set(st.tx, st.ty, st.tz);
    camera.position.set(
      camT.x + st.dist * Math.cos(cy) * Math.sin(yaw),
      camT.y + st.dist * Math.sin(cy),
      camT.z + st.dist * Math.cos(cy) * Math.cos(yaw)
    );
    camera.lookAt(camT);
    renderer.toneMappingExposure = st.exp;
  }

  // scroll drives a single scalar; all interpolation happens in the rAF tick. The
  // beat scrub is opt-in (opts.beats): the engine is built and mechanically verified,
  // but the pinned-stage layout choreography (device-to-one-side framing, text scrims
  // so nothing overlaps) needs a dedicated design pass to clear the "no beat feels
  // like a slide" bar · until then the hero holds the arrival framing (beat 0) with
  // drag-to-orbit, which is the shipped, clean behaviour.
  function onScroll() {
    const max = document.documentElement.scrollHeight - window.innerHeight;
    scrollP = max > 0 ? Math.max(0, Math.min(1, window.scrollY / max)) : 0;
    frame();
  }
  if (opts.beats) window.addEventListener('scroll', onScroll, { passive: true });

  function resize() {
    const w = canvas.clientWidth, h = canvas.clientHeight;
    if (w === 0 || h === 0) return;
    renderer.setSize(w, h, false);
    camera.aspect = w / h;
    camera.updateProjectionMatrix();
    frame();
  }

  // ---- interaction ------------------------------------------------------------------
  function onDown(e) {
    dragging = true;
    lastX = (e.touches ? e.touches[0].clientX : e.clientX);
    lastY = (e.touches ? e.touches[0].clientY : e.clientY);
  }
  function onMove(e) {
    const cx = (e.touches ? e.touches[0].clientX : e.clientX);
    const cy = (e.touches ? e.touches[0].clientY : e.clientY);
    if (dragging) {
      const dx = cx - lastX, dy = cy - lastY;
      lastX = cx; lastY = cy;
      dragYaw = clamp(dragYaw - dx * 0.006, -YAW_RANGE, YAW_RANGE);
      dragPitch = clamp(dragPitch - dy * 0.003, -PITCH_RANGE, PITCH_RANGE);
      frame();
    } else {
      hover(cx, cy);
    }
  }
  function onUp() { dragging = false; released = performance.now(); frame(); }

  canvas.addEventListener('mousedown', onDown);
  canvas.addEventListener('touchstart', onDown, { passive: true });
  window.addEventListener('mousemove', onMove);
  window.addEventListener('touchmove', onMove, { passive: true });
  window.addEventListener('mouseup', onUp);
  window.addEventListener('touchend', onUp);
  window.addEventListener('resize', resize);

  // ---- hover raycast ----------------------------------------------------------------
  const ray = new THREE.Raycaster();
  const ndc = new THREE.Vector2();
  let hovered = null;
  function hover(cx, cy) {
    const rect = canvas.getBoundingClientRect();
    ndc.x = ((cx - rect.left) / rect.width) * 2 - 1;
    ndc.y = -((cy - rect.top) / rect.height) * 2 + 1;
    ray.setFromCamera(ndc, camera);
    let hit = null;
    for (const k of ['mac-studio', 'dgx-spark']) {
      if (ray.intersectObject(groups[k], true).length) { hit = k; break; }
    }
    if (hit !== hovered) {
      hovered = hit;
      for (const k of ['mac-studio', 'dgx-spark']) {
        const on = k === hit;
        setLift(groups[k], on);
        if (labelEls[k]) labelEls[k].classList.toggle('on', on);
      }
      canvas.style.cursor = hit ? 'pointer' : 'grab';
      frame();
    }
  }
  function setLift(group, on) {
    group.traverse((o) => {
      if (o.isMesh && o.material) {
        const base = baseEmissive.get(o.material);
        if (base && o.material.emissive) {
          o.material.emissive.copy(base);
          if (on) o.material.emissive.addScalar(0.03); // restrained 3 percent lift
        }
      }
    });
  }

  // ---- render on demand -------------------------------------------------------------
  let pending = false;
  function frame() { if (!pending) { pending = true; requestAnimationFrame(tick); } }

  function tick() {
    pending = false;
    // when released, the drag target decays to zero so the camera returns to the beat
    if (!dragging) { dragYaw *= 0.9; dragPitch *= 0.9; }
    // a sub-degree idle drift while at rest (killed by reduced-motion)
    let idle = 0;
    if (!reduceMotion && !dragging) { idlePhase += 0.0015; idle = Math.sin(idlePhase) * 0.006; }
    // ease the displayed offset toward the drag target
    dispYaw += (dragYaw + idle - dispYaw) * 0.14;
    dispPitch += (dragPitch - dispPitch) * 0.14;
    placeCamera();
    if (loaded) renderer.render(scene, camera);

    const settling = Math.abs(dispYaw - dragYaw) > 1e-4 || Math.abs(dispPitch - dragPitch) > 1e-4;
    if (settling || (!reduceMotion && !dragging)) frame();
  }

  // context loss → fallback
  canvas.addEventListener('webglcontextlost', (e) => { e.preventDefault(); onFail(new Error('context lost')); });

  resize();
  placeCamera();
  frame();

  return {
    info: () => renderer.info,
    dispose: () => { renderer.dispose(); env.dispose && env.dispose(); },
  };
}

function clamp(v, a, b) { return Math.max(a, Math.min(b, v)); }

// A dark abstract gradient environment (metals reflect tone, not a studio).
function makeGradientEnv(renderer) {
  const c = document.createElement('canvas');
  c.width = 16; c.height = 256;
  const g = c.getContext('2d');
  const grad = g.createLinearGradient(0, 0, 0, 256);
  grad.addColorStop(0.0, '#1b1c20');   // faint sky
  grad.addColorStop(0.5, '#0d0d10');
  grad.addColorStop(1.0, '#050506');   // floor-dark
  g.fillStyle = grad; g.fillRect(0, 0, 16, 256);
  const tex = new THREE.CanvasTexture(c);
  tex.mapping = THREE.EquirectangularReflectionMapping;
  tex.colorSpace = THREE.SRGBColorSpace;
  const pmrem = new THREE.PMREMGenerator(renderer);
  const rt = pmrem.fromEquirectangular(tex);
  tex.dispose(); pmrem.dispose();
  return rt.texture;
}
