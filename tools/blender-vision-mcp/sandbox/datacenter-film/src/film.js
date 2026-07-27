/**
 * Sealed data-centre film runtime.
 *
 * Scroll is native: the document is genuinely tall and the camera reads
 * scrollY. Nothing intercepts wheel or touch. The camera samples the motion
 * table produced by the cinematic compiler, so the browser and Blender agree on
 * where the camera is at any scroll position.
 *
 * Load order is poster -> shell -> stable first frame -> detail -> network.
 * The poster is only crossfaded out once the shell has actually rendered.
 */

import * as THREE from '../vendor/three/build/three.module.min.js';
import { GLTFLoader } from '../vendor/three/examples/jsm/loaders/GLTFLoader.js';

const ASSETS = 'assets/';
const body = document.body;
const prefersReducedMotion = matchMedia('(prefers-reduced-motion: reduce)').matches;

function webglAvailable() {
  try {
    const probe = document.createElement('canvas');
    return Boolean(probe.getContext('webgl2') || probe.getContext('webgl'));
  } catch {
    return false;
  }
}

async function loadJSON(name) {
  const response = await fetch(ASSETS + name);
  if (!response.ok) throw new Error(`${name}: ${response.status}`);
  return response.json();
}

const lerpNumber = (a, b, t) => (a ?? 0) + ((b ?? 0) - (a ?? 0)) * t;
const lerp3 = (a, b, t) => [
  lerpNumber(a?.[0], b?.[0], t),
  lerpNumber(a?.[1], b?.[1], t),
  lerpNumber(a?.[2], b?.[2], t),
];

/* ------------------------------------------------------------------ content */

function buildTranscript(beats) {
  const host = document.getElementById('transcript-body');
  host.replaceChildren(...beats.flatMap(beat => {
    const index = document.createElement('span');
    index.className = 'index';
    index.textContent = beat.id;
    const heading = document.createElement('h2');
    heading.textContent = beat.label;
    const copy = document.createElement('p');
    copy.textContent = (beat.text || []).join(' ');
    return [index, heading, copy];
  }));
}

/** Map compiler zone ids (left_upper) onto CSS data-zone tokens (left-upper). */
function zoneToken(zone) {
  return String(zone || 'centre').replace(/_/g, '-');
}

function buildScript(beats) {
  const host = document.getElementById('script');
  // Fixed chrome height must match --chrome-h in film.css (56 px). Text is
  // placed in a safe band below it so chapter nav never covers beat copy.
  const chromeH = 56;
  const safeTopPx = chromeH + 20;
  host.replaceChildren(...beats.map(beat => {
    const section = document.createElement('section');
    section.className = 'beat';
    section.id = `beat-${beat.id}`;
    section.dataset.beat = beat.id;
    section.dataset.zone = zoneToken(beat.text_zone);
    // Scroll distance is the beat's own share of the path, so document length
    // and camera arc length stay in agreement.
    const span = Math.max(0.02, beat.scroll_end - beat.scroll_start);
    section.style.height = `${(span * 600).toFixed(2)}vh`;

    const inner = document.createElement('div');
    inner.className = 'beat-inner';
    // Explicit safe-area top (CSS is the source of truth; this mirrors it for
    // zones that sticky-position at the top of the viewport).
    const zone = section.dataset.zone;
    if (zone === 'left-upper' || zone === 'right-upper' || zone === 'edge') {
      inner.style.top = `${safeTopPx}px`;
    } else if (zone === 'centre') {
      inner.style.top = `max(${safeTopPx}px, 28vh)`;
    }
    const index = document.createElement('span');
    index.className = 'index';
    index.textContent = beat.id;
    const heading = document.createElement('h2');
    heading.textContent = beat.label;
    inner.append(index, heading);
    for (const line of beat.text || []) {
      const copy = document.createElement('p');
      copy.textContent = line;
      inner.append(copy);
    }
    section.append(inner);
    return section;
  }));
}

function buildChapters(beats, onJump) {
  const nav = document.getElementById('chapters');
  nav.replaceChildren(...beats.map(beat => {
    const button = document.createElement('button');
    button.type = 'button';
    button.textContent = beat.id;
    button.dataset.beat = beat.id;
    button.setAttribute('aria-label', `Chapter ${beat.id}: ${beat.label}`);
    button.addEventListener('click', () => onJump(beat));
    return button;
  }));
}

/* ------------------------------------------------------------------ camera */

class MotionTable {
  constructor(table) {
    this.samples = table.samples || table.frames || table;
    this.count = this.samples.length;
  }

  /** Camera state at normalized scroll, interpolated between samples. */
  at(scroll) {
    const clamped = Math.min(1, Math.max(0, scroll));
    // Locate by the sample's own scroll value rather than assuming the table is
    // uniformly spaced in scroll; the compiler is free to change its sampling.
    let low = 0;
    let high = this.count - 1;
    while (low < high - 1) {
      const mid = (low + high) >> 1;
      if (this.samples[mid].scroll <= clamped) low = mid; else high = mid;
    }
    const spanStart = this.samples[low].scroll;
    const spanEnd = this.samples[high].scroll;
    const blend = spanEnd > spanStart ? (clamped - spanStart) / (spanEnd - spanStart) : 0;
    const a = this.samples[low];
    const b = this.samples[high];
    return {
      position: lerp3(a.position, b.position, blend),
      focus: lerp3(a.focus_target || a.focus, b.focus_target || b.focus, blend),
      focal: lerpNumber(a.focal_length_mm, b.focal_length_mm, blend),
      exposure: lerpNumber(a.exposure, b.exposure, blend),
      beat: (blend < 0.5 ? a : b).beat_id,
    };
  }
}

/* ------------------------------------------------------------------ boot */

async function main() {
  const [beats, stills] = await Promise.all([
    loadJSON('beats.json'),
    loadJSON('reduced-motion.json'),
  ]);

  buildTranscript(beats);
  buildScript(beats);

  const goStatic = () => {
    body.dataset.mode = 'static';
    document.getElementById('transcript').scrollIntoView({ behavior: 'auto' });
  };
  document.getElementById('skip').addEventListener('click', goStatic);

  // Complete route without WebGL, and the same route for reduced motion: the
  // poster stays and the text carries everything the film says.
  if (!webglAvailable()) {
    body.dataset.mode = 'text';
    return;
  }
  if (prefersReducedMotion) {
    body.dataset.mode = 'static';
    return;
  }

  const table = new MotionTable(await loadJSON('motion-table.json'));

  const canvas = document.getElementById('scene');
  const renderer = new THREE.WebGLRenderer({
    canvas,
    antialias: true,
    powerPreference: 'high-performance',
  });
  renderer.setClearColor(0x07080a, 1);
  renderer.toneMapping = THREE.ACESFilmicToneMapping;

  const scene = new THREE.Scene();
  scene.fog = new THREE.FogExp2(0x07080a, 0.045);
  const camera = new THREE.PerspectiveCamera(50, 1, 0.1, 260);
  // The world is Blender Z-up after the load-time conversion. three.js defaults
  // camera.up to Y-up, and lookAt() derives roll from it, so leaving it default
  // rolls the whole shot onto its side.
  camera.up.set(0, 0, 1);

  // Corridor lighting: cool ambient wash plus a warm practical down the aisle.
  // Restrained, and no per-status point lights — indicator state is emissive
  // material on instanced geometry, not hundreds of lights.
  scene.add(new THREE.HemisphereLight(0x24303c, 0x05070a, 0.55));
  const key = new THREE.DirectionalLight(0xbcd4ff, 0.85);
  key.position.set(3.5, 2.0, 6.0);
  scene.add(key);
  const warm = new THREE.PointLight(0xffd9a8, 26, 26, 2);
  warm.position.set(0, 6.0, 2.4);
  scene.add(warm);

  const loader = new GLTFLoader();
  // glTF is Y-up; the motion table is in Blender world coordinates, which are
  // Z-up. Blender (x, y, z) exports as glTF (x, z, -y), so rotating the loaded
  // root by +90 degrees about X puts the asset back in the frame the camera is
  // driving in. Without this the camera flies through a wall it reads as a
  // floor. The two frames are declared in blender_vision.v2.authority as
  // BLENDER_WORLD and GLTF_WORLD; this is that conversion, applied once at load.
  const BLENDER_FROM_GLTF_X_ROTATION = Math.PI / 2;
  const loadGLB = name => new Promise((resolve, reject) =>
    loader.load(ASSETS + name, gltf => {
      gltf.scene.rotation.x = BLENDER_FROM_GLTF_X_ROTATION;
      gltf.scene.updateMatrixWorld(true);
      resolve(gltf.scene);
    }, undefined, reject));

  const tiers = { shell: await loadGLB('datacenter-shell.glb') };
  scene.add(tiers.shell);

  let sized = false;
  const resize = () => {
    const width = Math.max(1, innerWidth);
    const height = Math.max(1, innerHeight);
    // Adaptive DPR, capped, and backed off on small viewports so mobile does
    // not pay for pixels it cannot show.
    renderer.setPixelRatio(Math.min(devicePixelRatio || 1, width < 820 ? 1.5 : 2));
    renderer.setSize(width, height, false);
    camera.aspect = width / height;
    camera.updateProjectionMatrix();
    sized = true;
    needsFrame = true;
  };

  const scrollSpan = () => Math.max(1, document.documentElement.scrollHeight - innerHeight);
  const target = new THREE.Vector3();
  const pendingTarget = new THREE.Vector3();
  let smoothed = null;
  let needsFrame = true;
  let activeBeat = null;
  let posterCleared = false;

  addEventListener('resize', resize, { passive: true });
  resize();

  const applyCamera = () => {
    const scroll = scrollY / scrollSpan();
    const state = table.at(scroll);
    const desired = new THREE.Vector3(...state.position);
    pendingTarget.copy(desired);
    if (smoothed === null) {
      smoothed = desired.clone();
    } else {
      // Light damping only: enough to remove per-event jitter, not enough to
      // read as the camera trailing the scroll.
      smoothed.lerp(desired, 0.35);
    }
    camera.position.copy(smoothed);
    target.set(...state.focus);
    camera.lookAt(target);
    camera.fov = 2 * Math.atan(24 / (2 * Math.max(12, state.focal))) * (180 / Math.PI);
    camera.updateProjectionMatrix();
    renderer.toneMappingExposure = Math.pow(2, state.exposure || 0);
    return state;
  };

  const sections = [...document.querySelectorAll('.beat')];
  buildChapters(beats, beat => {
    document.getElementById(`beat-${beat.id}`)
      ?.scrollIntoView({ behavior: prefersReducedMotion ? 'auto' : 'smooth' });
  });
  const chapterButtons = new Map(
    [...document.querySelectorAll('#chapters button')].map(b => [b.dataset.beat, b])
  );

  const markBeat = id => {
    if (id === activeBeat) return;
    activeBeat = id;
    for (const section of sections) {
      section.dataset.active = String(section.dataset.beat === id);
    }
    for (const [beatId, button] of chapterButtons) {
      button.setAttribute('aria-current', String(beatId === id));
    }
  };

  // Request-on-demand rendering: a frame is drawn when scroll moved or an asset
  // arrived, not on a free-running loop.
  const requestFrame = () => { needsFrame = true; };
  addEventListener('scroll', requestFrame, { passive: true });

  const tick = () => {
    if (needsFrame && sized) {
      needsFrame = false;
      const state = applyCamera();
      renderer.render(scene, camera);
      markBeat(state.beat);
      // Keep drawing while damping is still converging. Without this,
      // request-on-demand stops the moment scrolling stops and the camera
      // freezes part-way to its target, which reads as a lagging camera.
      if (smoothed.distanceToSquared(pendingTarget) > 1e-6) needsFrame = true;
      if (!posterCleared) {
        // Only now is there a correct first frame on the canvas.
        posterCleared = true;
        body.dataset.mode = 'live';
      }
    }
    requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);

  addEventListener('keydown', event => {
    if (event.target !== document.body) return;
    const order = beats.map(beat => beat.id);
    const current = order.indexOf(activeBeat);
    let next = null;
    if (event.key === 'ArrowRight' || event.key === 'PageDown') next = current + 1;
    if (event.key === 'ArrowLeft' || event.key === 'PageUp') next = current - 1;
    if (event.key === 'Home') next = 0;
    if (event.key === 'End') next = order.length - 1;
    if (next === null || next < 0 || next >= order.length) return;
    event.preventDefault();
    document.getElementById(`beat-${order[next]}`)
      ?.scrollIntoView({ behavior: prefersReducedMotion ? 'auto' : 'smooth' });
  });

  // Detail and network stream in behind the shell, gated on their chapters.
  const enrich = async (name, tier) => {
    if (tiers[tier]) return;
    tiers[tier] = await loadGLB(name);
    scene.add(tiers[tier]);
    requestFrame();
  };
  const prefetch = () => {
    const scroll = scrollY / scrollSpan();
    if (scroll > 0.02) enrich('datacenter-detail.glb', 'detail').catch(() => {});
    if (scroll > 0.35) enrich('datacenter-network.glb', 'network').catch(() => {});
  };
  addEventListener('scroll', prefetch, { passive: true });
  setTimeout(prefetch, 300);

  window.__film = { table, beats, renderer, scene, camera, applyCamera, tiers };
}

main().catch(error => {
  // Any failure falls back to the complete text route rather than a blank page.
  console.error('film boot failed', error);
  document.body.dataset.mode = 'text';
});
