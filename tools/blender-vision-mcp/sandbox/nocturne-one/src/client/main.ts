import "./styles.css";

const declaredStates = [
  "initial_loading",
  "poster_fallback",
  "3d_ready",
  "3d_unavailable",
  "reduced_motion",
  "keyboard_navigation",
  "touch_interaction",
  "slow_network",
  "offline_retry",
  "api_validation_error",
  "api_transient_error",
  "successful_reservation",
  "empty_configuration",
  "restored_saved_configuration"
] as const;

type AppState = (typeof declaredStates)[number];
type TestCondition = "slow_network" | "api_transient_error" | null;
type Variant = "obsidian" | "lunar" | "ember";
type Accessory = "none" | "braided-cable";

interface Configuration {
  variant: Variant;
  light_intensity: number;
  orientation: number;
  accessory: Accessory;
}

interface SceneController {
  updateConfiguration(configuration: Configuration): void;
  selectPart(part: string | null): void;
  setScrollProgress(progress: number): void;
  sampleFrames(count: number): Promise<number[]>;
  dispose(): void;
}

interface NocturneProbe {
  readonly state: AppState;
  readonly stateHistory: AppState[];
  readonly declaredStates: readonly AppState[];
  readonly route: string;
  readonly config: Configuration;
  readonly sceneConfig: Configuration;
  readonly posterVisible: boolean;
  readonly glbRequested: boolean;
  readonly webglAvailable: boolean;
  readonly reducedMotion: boolean;
  readonly animationEnabled: boolean;
  readonly selectedPart: string | null;
  enter3D(): Promise<void>;
  sampleFrames(count: number): Promise<number[]>;
  selectPart(part: string | null): void;
  setConfiguration(partial: Partial<Configuration>): void;
  setTestCondition(condition: TestCondition): void;
}

declare global {
  interface Window {
    __NOCTURNE__: NocturneProbe;
  }
}

const defaultConfiguration: Configuration = {
  variant: "obsidian",
  light_intensity: 64,
  orientation: 0,
  accessory: "none"
};

const partCopy: Record<string, { title: string; detail: string; datum: string }> = {
  base: {
    title: "Monolithic base",
    detail: "A 320 × 180 mm anodized plinth stabilizes light, drivers, and touch control.",
    datum: "34 mm / black anodized aluminum"
  },
  outer_shell: {
    title: "Asymmetric arch",
    detail: "A continuous elliptical tube frames the acoustic field without enclosing it.",
    datum: "24 mm radius / depth scale 1.22"
  },
  glass_core: {
    title: "Frosted glass core",
    detail: "A translucent ellipsoid softens the rear eclipse source into a quiet spatial glow.",
    datum: "122 × 96 × 252 mm / frosted glass"
  },
  eclipse_disk: {
    title: "Eclipse disk",
    detail: "Warm emissive ceramic provides the instrument’s adjustable ambient light.",
    datum: "108 mm diameter / 8 mm deep"
  },
  acoustic_membrane: {
    title: "Acoustic membrane",
    detail: "Graphite tensioned textile controls the forward face of the directional sound field.",
    datum: "108 × 8 × 222 mm / graphite textile"
  },
  thermal_grille: {
    title: "Thermal grille",
    detail: "Twenty-three open horizontal slots release heat from the rear assembly.",
    datum: "23 slots / 6.2 mm pitch"
  },
  rotary_control: {
    title: "Rotary control",
    detail: "A machined aluminum dial gives tactile control over the light field.",
    datum: "34 mm diameter / machined aluminum"
  },
  braided_cable: {
    title: "Braided cable",
    detail: "A replaceable graphite braid exits the rear plane and remains independently configurable.",
    datum: "3.2 mm radius / braided graphite"
  },
  internal_frame: {
    title: "Internal frame",
    detail: "The narrow structural spine locates glass, drivers, and the rear light source.",
    datum: "104 × 68 × 236 mm"
  },
  logic_board: {
    title: "Logic board",
    detail: "A compact lower board coordinates light intensity and directional audio.",
    datum: "94 × 4 × 48 mm"
  },
  left_driver: {
    title: "Left driver",
    detail: "The matched lateral driver shapes the left side of the acoustic field.",
    datum: "58 mm diameter / 18 mm deep"
  },
  right_driver: {
    title: "Right driver",
    detail: "The matched lateral driver shapes the right side of the acoustic field.",
    datum: "58 mm diameter / 18 mm deep"
  }
};

const app = document.querySelector<HTMLDivElement>("#app");
if (!app) throw new Error("Application root is missing.");

function storedHistory(): AppState[] {
  try {
    const value = JSON.parse(sessionStorage.getItem("nocturne.stateHistory") ?? "[]");
    return Array.isArray(value)
      ? value.filter((item): item is AppState =>
          declaredStates.includes(item as AppState)
        )
      : [];
  } catch {
    return [];
  }
}

function storedConfiguration(): Configuration | null {
  try {
    const value = JSON.parse(localStorage.getItem("nocturne.configuration") ?? "null");
    if (
      value &&
      ["obsidian", "lunar", "ember"].includes(value.variant) &&
      Number.isInteger(value.light_intensity) &&
      value.light_intensity >= 0 &&
      value.light_intensity <= 100 &&
      Number.isInteger(value.orientation) &&
      value.orientation >= -45 &&
      value.orientation <= 45 &&
      ["none", "braided-cable"].includes(value.accessory)
    ) {
      return value as Configuration;
    }
  } catch {
    // A damaged preference is treated as an empty configuration.
  }
  return null;
}

function detectWebGL(): boolean {
  try {
    const canvas = document.createElement("canvas");
    return Boolean(
      canvas.getContext("webgl2", { failIfMajorPerformanceCaveat: false }) ||
        canvas.getContext("webgl", { failIfMajorPerformanceCaveat: false })
    );
  } catch {
    return false;
  }
}

let stateHistory = storedHistory();
let state: AppState = "initial_loading";
let config = storedConfiguration() ?? { ...defaultConfiguration };
let sceneConfig = { ...config };
let posterVisible = true;
let glbRequested = false;
const webglAvailable = detectWebGL();
const reducedMotion = matchMedia("(prefers-reduced-motion: reduce)").matches;
let selectedPart: string | null = null;
let testCondition: TestCondition = null;
let scene: SceneController | null = null;
let entering = false;

function transition(next: AppState): void {
  state = next;
  if (stateHistory.at(-1) !== next) stateHistory.push(next);
  sessionStorage.setItem(
    "nocturne.stateHistory",
    JSON.stringify(stateHistory.slice(-160))
  );
  document.documentElement.dataset.state = next;
  const stateLabel = document.querySelector<HTMLElement>("[data-runtime-state]");
  if (stateLabel) stateLabel.textContent = next.replaceAll("_", " ");
}

transition("initial_loading");
if (storedConfiguration()) transition("restored_saved_configuration");
else transition("empty_configuration");
if (reducedMotion) transition("reduced_motion");
else transition("poster_fallback");

function route(): string {
  return location.pathname.replace(/\/+$/, "") || "/";
}

function nav(): string {
  const current = route();
  const items = [
    ["/", "Overview"],
    ["/technology", "Technology"],
    ["/configurator", "Configure"],
    ["/reserve", "Reserve"],
    ["/receipt", "Receipt"]
  ];
  return `
    <a class="skip-link" href="#main">Skip to content</a>
    <header class="site-header">
      <a class="wordmark" href="/" aria-label="NOCTURNE/ONE home">
        <span>NOCTURNE</span><i>/</i><span>ONE</span>
      </a>
      <nav aria-label="Primary navigation">
        ${items
          .map(
            ([href, label]) =>
              `<a href="${href}" ${href === current ? 'aria-current="page"' : ""}>${label}</a>`
          )
          .join("")}
      </nav>
      <span class="edition">N° 001</span>
    </header>`;
}

function footer(): string {
  return `
    <footer>
      <span>NOCTURNE/ONE</span>
      <p>Fictional spatial light-and-audio instrument · Toronto / 2026</p>
      <span data-runtime-state>${state.replaceAll("_", " ")}</span>
    </footer>`;
}

function productStage(showEntry = false): string {
  return `
    <div class="product-stage" data-variant="${config.variant}">
      <img
        class="product-poster"
        src="/assets/nocturne-one-poster.webp"
        width="1024"
        height="1024"
        alt="NOCTURNE/ONE asymmetric black arch surrounding a frosted acoustic core and warm eclipse light"
      />
      <canvas
        id="product-canvas"
        width="960"
        height="720"
        tabindex="0"
        aria-label="Interactive 3D model of NOCTURNE/ONE. Drag, swipe, or use arrow keys to orbit."
      ></canvas>
      <div class="stage-vignette" aria-hidden="true"></div>
      ${
        showEntry
          ? `<button id="enter-3d" class="enter-button" type="button">
               <span>Enter 3D</span><i aria-hidden="true">↗</i>
             </button>`
          : `<p class="stage-note">Interactive view begins from the overview.</p>`
      }
      <div class="stage-data" aria-hidden="true">
        <span>320 W</span><span>360 H</span><span>180 D</span>
      </div>
      <p class="webgl-note" hidden>
        Interactive 3D is unavailable. The product image and every non-3D action remain available.
      </p>
    </div>`;
}

function home(): string {
  return `
    ${nav()}
    <main id="main">
      <section class="hero">
        <div class="hero-copy">
          <p class="eyebrow">Spatial light / directional sound</p>
          <h1>A quiet eclipse<br />for room and desk.</h1>
          <p class="lede">
            NOCTURNE/ONE gathers a warm field of light and directional sound inside
            one continuous, asymmetric arch.
          </p>
          <a class="text-link" href="/technology">Read the instrument <span>→</span></a>
        </div>
        ${productStage(true)}
        <p class="scroll-cue"><span></span> Scroll to move through the assembly</p>
      </section>
      <section class="chapter shell-chapter">
        <div>
          <p class="eyebrow">01 / Shell</p>
          <h2>One line.<br />No enclosure.</h2>
        </div>
        <p>
          The anodized arch rises from the base as a single 24 mm profile, creating
          structure without turning the instrument into a box.
        </p>
      </section>
      <section class="chapter core-chapter">
        <div>
          <p class="eyebrow">02 / Core</p>
          <h2>Sound in front.<br />Light behind.</h2>
        </div>
        <p>
          Tensioned graphite textile faces the listener. Frosted glass mediates the
          warm ceramic eclipse behind it.
        </p>
      </section>
      <section class="manifesto">
        <p>Dim the room.</p>
        <p>Orient the field.</p>
        <a href="/configurator">Configure yours <span>↗</span></a>
      </section>
    </main>
    ${footer()}`;
}

function partOptions(): string {
  return Object.entries(partCopy)
    .map(([value, copy]) => `<option value="${value}">${copy.title}</option>`)
    .join("");
}

function technology(): string {
  const copy = partCopy.glass_core!;
  selectedPart = "glass_core";
  return `
    ${nav()}
    <main id="main">
      <section class="page-intro technology-intro">
        <p class="eyebrow">Anatomy / 12 semantic parts</p>
        <h1>The eclipse,<br />made inspectable.</h1>
        <p>
          Select a named component. The assembly and its textual equivalent always
          identify the same physical part.
        </p>
      </section>
      <section class="technology-grid">
        <div class="tech-stage">${productStage(false)}</div>
        <div class="part-panel">
          <label for="part-selector">Selected component</label>
          <select id="part-selector">${partOptions()}</select>
          <div class="part-index"><span>Part</span><strong data-part-number>03 / 12</strong></div>
          <article aria-live="polite">
            <p class="eyebrow" data-part-datum>${copy.datum}</p>
            <h2 data-part-title>${copy.title}</h2>
            <p data-part-detail>${copy.detail}</p>
          </article>
          <a class="text-link" href="/configurator">Configure material state <span>→</span></a>
        </div>
      </section>
    </main>
    ${footer()}`;
}

function configurator(): string {
  return `
    ${nav()}
    <main id="main">
      <section class="page-intro configure-intro">
        <p class="eyebrow">Configuration / persistent state</p>
        <h1>Tune the field.</h1>
        <p>Material, light, orientation, and cable state are shared by the interface and 3D scene.</p>
      </section>
      <section class="config-grid">
        <div class="config-preview">
          ${productStage(false)}
          <div class="config-summary" aria-live="polite">
            <span data-summary-variant>${config.variant}</span>
            <span><b data-summary-light>${config.light_intensity}</b>% light</span>
            <span><b data-summary-orientation>${config.orientation}</b>° field</span>
          </div>
        </div>
        <form class="controls" aria-label="Product configuration">
          <div class="control-group">
            <label for="variant"><span>01</span> Material variant</label>
            <select id="variant">
              <option value="obsidian">Obsidian</option>
              <option value="lunar">Lunar</option>
              <option value="ember">Ember</option>
            </select>
          </div>
          <div class="control-group range-group">
            <label for="light"><span>02</span> Eclipse light <output for="light" data-light-output>${config.light_intensity}%</output></label>
            <input id="light" type="range" min="0" max="100" step="1" value="${config.light_intensity}" />
          </div>
          <div class="control-group range-group">
            <label for="orientation"><span>03</span> Field orientation <output for="orientation" data-orientation-output>${config.orientation}°</output></label>
            <input id="orientation" type="range" min="-45" max="45" step="1" value="${config.orientation}" />
          </div>
          <div class="control-group">
            <label for="accessory"><span>04</span> Accessory</label>
            <select id="accessory">
              <option value="none">No cable</option>
              <option value="braided-cable">Braided cable</option>
            </select>
          </div>
          <button id="save-configuration" class="primary-button" type="button">
            Save configuration <span>→</span>
          </button>
          <p id="configuration-status" class="form-status" aria-live="polite"></p>
          <a class="text-link" href="/reserve">Continue to reservation <span>→</span></a>
        </form>
      </section>
    </main>
    ${footer()}`;
}

function reserve(): string {
  return `
    ${nav()}
    <main id="main">
      <section class="reservation-layout">
        <div class="page-intro reserve-intro">
          <p class="eyebrow">Edition N° 001 / reservation</p>
          <h1>Reserve the<br />first eclipse.</h1>
          <p>
            Reservation is complimentary. We will send production timing to the
            address below.
          </p>
          <dl class="reserve-spec">
            <div><dt>Variant</dt><dd data-reserve-variant>${config.variant}</dd></div>
            <div><dt>Light</dt><dd data-reserve-light>${config.light_intensity}%</dd></div>
            <div><dt>Orientation</dt><dd data-reserve-orientation>${config.orientation}°</dd></div>
            <div><dt>Accessory</dt><dd data-reserve-accessory>${config.accessory}</dd></div>
          </dl>
        </div>
        <form id="reservation-form" class="reservation-form" novalidate>
          <label for="reserve-email">Email address</label>
          <input
            id="reserve-email"
            name="email"
            type="email"
            autocomplete="email"
            aria-describedby="reserve-email-error"
            placeholder="you@example.com"
            required
          />
          <p id="reserve-email-error" class="field-error"></p>
          <button id="reserve-submit" class="primary-button" type="submit">
            Request reservation <span>→</span>
          </button>
          <p id="reservation-status" class="form-status" aria-live="polite"></p>
          <p class="form-note">Authenticated local reservation · idempotent retry · no payment</p>
        </form>
      </section>
    </main>
    ${footer()}`;
}

function receipt(): string {
  return `
    ${nav()}
    <main id="main">
      <section class="receipt-layout">
        <div class="receipt-mark" aria-hidden="true"><span></span></div>
        <div>
          <p class="eyebrow">Reservation status</p>
          <h1>Your place in<br />the eclipse.</h1>
          <p id="reservation-status" class="receipt-status" aria-live="polite">
            Looking for your most recent reservation…
          </p>
          <dl class="receipt-data">
            <div><dt>Reservation</dt><dd data-receipt-id>—</dd></div>
            <div><dt>Status</dt><dd data-receipt-status>Not found</dd></div>
            <div><dt>Variant</dt><dd data-receipt-variant>${config.variant}</dd></div>
          </dl>
          <a class="text-link" href="/configurator">Return to configuration <span>→</span></a>
        </div>
      </section>
    </main>
    ${footer()}`;
}

const renderers: Record<string, () => string> = {
  "/": home,
  "/technology": technology,
  "/configurator": configurator,
  "/reserve": reserve,
  "/receipt": receipt
};

app.innerHTML = (renderers[route()] ?? home)();

function syncVisibleConfiguration(): void {
  document
    .querySelectorAll<HTMLElement>("[data-summary-variant], [data-reserve-variant]")
    .forEach((item) => (item.textContent = config.variant));
  document
    .querySelectorAll<HTMLElement>("[data-summary-light]")
    .forEach((item) => (item.textContent = String(config.light_intensity)));
  document
    .querySelectorAll<HTMLElement>("[data-reserve-light]")
    .forEach((item) => (item.textContent = `${config.light_intensity}%`));
  document
    .querySelectorAll<HTMLElement>("[data-summary-orientation]")
    .forEach((item) => (item.textContent = String(config.orientation)));
  document
    .querySelectorAll<HTMLElement>("[data-reserve-orientation]")
    .forEach((item) => (item.textContent = `${config.orientation}°`));
  document
    .querySelectorAll<HTMLElement>("[data-reserve-accessory]")
    .forEach((item) => (item.textContent = config.accessory));
  const stage = document.querySelector<HTMLElement>(".product-stage");
  if (stage) stage.dataset.variant = config.variant;
  const lightOutput = document.querySelector<HTMLOutputElement>("[data-light-output]");
  if (lightOutput) lightOutput.value = `${config.light_intensity}%`;
  const orientationOutput =
    document.querySelector<HTMLOutputElement>("[data-orientation-output]");
  if (orientationOutput) orientationOutput.value = `${config.orientation}°`;
}

function setConfiguration(partial: Partial<Configuration>): void {
  const next: Configuration = {
    variant: ["obsidian", "lunar", "ember"].includes(partial.variant ?? "")
      ? (partial.variant as Variant)
      : config.variant,
    light_intensity: Number.isFinite(partial.light_intensity)
      ? Math.max(0, Math.min(100, Math.round(partial.light_intensity!)))
      : config.light_intensity,
    orientation: Number.isFinite(partial.orientation)
      ? Math.max(-45, Math.min(45, Math.round(partial.orientation!)))
      : config.orientation,
    accessory: ["none", "braided-cable"].includes(partial.accessory ?? "")
      ? (partial.accessory as Accessory)
      : config.accessory
  };
  config = next;
  sceneConfig = { ...next };
  localStorage.setItem("nocturne.configuration", JSON.stringify(next));
  scene?.updateConfiguration(next);
  syncVisibleConfiguration();
}

async function enter3D(): Promise<void> {
  if (scene || entering) return;
  glbRequested = true;
  if (!webglAvailable) {
    transition("3d_unavailable");
    document.querySelector<HTMLElement>(".webgl-note")?.removeAttribute("hidden");
    return;
  }
  entering = true;
  if (testCondition === "slow_network") {
    transition("slow_network");
    await new Promise((resolve) => setTimeout(resolve, 650));
  }
  try {
    const canvas = document.querySelector<HTMLCanvasElement>("#product-canvas");
    if (!canvas) throw new Error("The product canvas is unavailable.");
    const module = await import("./renderer.js");
    const asset =
      matchMedia("(max-width: 640px)").matches
        ? "/assets/nocturne-one-low.glb"
        : "/assets/nocturne-one-hero.glb";
    scene = await module.createNocturneScene({
      canvas,
      asset,
      configuration: sceneConfig,
      reducedMotion
    });
    scene.selectPart(selectedPart);
    posterVisible = false;
    document.querySelector(".product-poster")?.classList.add("is-hidden");
    document.querySelector(".product-stage")?.classList.add("is-live");
    transition("3d_ready");
  } catch (error) {
    console.error(error);
    posterVisible = true;
    transition("3d_unavailable");
    document.querySelector<HTMLElement>(".webgl-note")?.removeAttribute("hidden");
  } finally {
    entering = false;
  }
}

function selectPart(part: string | null): void {
  if (part !== null && !(part in partCopy)) return;
  selectedPart = part;
  scene?.selectPart(part);
  if (!part) return;
  const copy = partCopy[part]!;
  const index = Object.keys(partCopy).indexOf(part) + 1;
  const title = document.querySelector<HTMLElement>("[data-part-title]");
  const detail = document.querySelector<HTMLElement>("[data-part-detail]");
  const datum = document.querySelector<HTMLElement>("[data-part-datum]");
  const number = document.querySelector<HTMLElement>("[data-part-number]");
  if (title) title.textContent = copy.title;
  if (detail) detail.textContent = copy.detail;
  if (datum) datum.textContent = copy.datum;
  if (number) number.textContent = `${String(index).padStart(2, "0")} / 12`;
}

async function sampleFrames(count: number): Promise<number[]> {
  const safe = Math.max(1, Math.min(240, Math.floor(count)));
  if (!scene) return [];
  return scene.sampleFrames(safe);
}

const probe: NocturneProbe = {
  get state() {
    return state;
  },
  get stateHistory() {
    return [...stateHistory];
  },
  get declaredStates() {
    return declaredStates;
  },
  get route() {
    return route();
  },
  get config() {
    return { ...config };
  },
  get sceneConfig() {
    return { ...sceneConfig };
  },
  get posterVisible() {
    return posterVisible;
  },
  get glbRequested() {
    return glbRequested;
  },
  get webglAvailable() {
    return webglAvailable;
  },
  get reducedMotion() {
    return reducedMotion;
  },
  get animationEnabled() {
    return !reducedMotion;
  },
  get selectedPart() {
    return selectedPart;
  },
  enter3D,
  sampleFrames,
  selectPart,
  setConfiguration,
  setTestCondition(condition) {
    if (
      condition === null ||
      condition === "slow_network" ||
      condition === "api_transient_error"
    ) {
      testCondition = condition;
    }
  }
};
window.__NOCTURNE__ = probe;

document.querySelector("#enter-3d")?.addEventListener("click", () => void enter3D());

const partSelector = document.querySelector<HTMLSelectElement>("#part-selector");
if (partSelector) {
  partSelector.value = selectedPart ?? "glass_core";
  partSelector.addEventListener("change", () => selectPart(partSelector.value));
}

const variant = document.querySelector<HTMLSelectElement>("#variant");
const light = document.querySelector<HTMLInputElement>("#light");
const orientation = document.querySelector<HTMLInputElement>("#orientation");
const accessory = document.querySelector<HTMLSelectElement>("#accessory");
if (variant && light && orientation && accessory) {
  variant.value = config.variant;
  light.value = String(config.light_intensity);
  orientation.value = String(config.orientation);
  accessory.value = config.accessory;
  variant.addEventListener("change", () =>
    setConfiguration({ variant: variant.value as Variant })
  );
  light.addEventListener("input", () =>
    setConfiguration({ light_intensity: Number(light.value) })
  );
  orientation.addEventListener("input", () =>
    setConfiguration({ orientation: Number(orientation.value) })
  );
  accessory.addEventListener("change", () =>
    setConfiguration({ accessory: accessory.value as Accessory })
  );
}

document
  .querySelector<HTMLButtonElement>("#save-configuration")
  ?.addEventListener("click", async () => {
    const status = document.querySelector<HTMLElement>("#configuration-status");
    if (status) status.textContent = "Saving configuration…";
    try {
      const response = await fetch("/api/configurations", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-NOCTURNE-ACTOR": "browser-actor",
          "Idempotency-Key": crypto.randomUUID()
        },
        body: JSON.stringify({ configuration: config })
      });
      if (!response.ok) throw new Error(`save failed (${response.status})`);
      if (status) status.textContent = "Configuration saved to this instrument.";
    } catch {
      if (status) status.textContent = "Configuration could not be saved. Try again.";
    }
  });

function reservationKey(body: string): string {
  const stored = sessionStorage.getItem("nocturne.pendingReservation");
  if (stored) {
    try {
      const value = JSON.parse(stored) as { body: string; key: string };
      if (value.body === body && value.key) return value.key;
    } catch {
      // Replace invalid pending state.
    }
  }
  const value = { body, key: crypto.randomUUID() };
  sessionStorage.setItem("nocturne.pendingReservation", JSON.stringify(value));
  return value.key;
}

document
  .querySelector<HTMLFormElement>("#reservation-form")
  ?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const email = document.querySelector<HTMLInputElement>("#reserve-email")!;
    const error = document.querySelector<HTMLElement>("#reserve-email-error")!;
    const status = document.querySelector<HTMLElement>("#reservation-status")!;
    error.textContent = "";
    email.removeAttribute("aria-invalid");
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.value)) {
      error.textContent = "Enter a valid email address.";
      email.setAttribute("aria-invalid", "true");
      status.textContent = "Check the email field and try again.";
      transition("api_validation_error");
      return;
    }
    const payload = JSON.stringify({
      configuration: config,
      email: email.value
    });
    status.textContent = "Contacting the reservation service…";
    try {
      const response = await fetch("/api/reservations", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-NOCTURNE-ACTOR": "browser-actor",
          "X-NOCTURNE-PERMISSIONS": "reservation:create",
          "Idempotency-Key": reservationKey(payload),
          ...(testCondition === "api_transient_error"
            ? { "X-NOCTURNE-SIMULATE": "transient" }
            : {})
        },
        body: payload
      });
      const value = (await response.json()) as Record<string, unknown>;
      if (response.status === 400) {
        error.textContent = "Enter a valid email address.";
        email.setAttribute("aria-invalid", "true");
        status.textContent = "The reservation needs a valid email.";
        transition("api_validation_error");
        return;
      }
      if (response.status === 503) {
        status.textContent = "The service is briefly unavailable. Retry this request.";
        transition("api_transient_error");
        return;
      }
      if (!response.ok) {
        status.textContent = "The reservation could not be completed.";
        transition("api_transient_error");
        return;
      }
      localStorage.setItem("nocturne.lastReservation", JSON.stringify(value));
      sessionStorage.removeItem("nocturne.pendingReservation");
      status.textContent = `Reservation ${String(value.id)} is confirmed.`;
      transition("successful_reservation");
    } catch {
      status.textContent = "You appear to be offline. Reconnect and retry this request.";
      transition("offline_retry");
    }
  });

async function restoreReceipt(): Promise<void> {
  if (route() !== "/receipt") return;
  const live = document.querySelector<HTMLElement>("#reservation-status")!;
  const raw = localStorage.getItem("nocturne.lastReservation");
  if (!raw) {
    live.textContent = "No reservation is stored on this device yet.";
    return;
  }
  try {
    const stored = JSON.parse(raw) as {
      id: string;
      status: string;
      configuration?: Configuration;
    };
    const response = await fetch(`/api/reservations/${encodeURIComponent(stored.id)}`, {
      headers: { "X-NOCTURNE-ACTOR": "browser-actor" }
    });
    const value = response.ok
      ? ((await response.json()) as typeof stored)
      : stored;
    document.querySelector<HTMLElement>("[data-receipt-id]")!.textContent = value.id;
    document.querySelector<HTMLElement>("[data-receipt-status]")!.textContent =
      value.status;
    document.querySelector<HTMLElement>("[data-receipt-variant]")!.textContent =
      value.configuration?.variant ?? config.variant;
    live.textContent = `Reservation ${value.id} is ${value.status}.`;
    transition("successful_reservation");
  } catch {
    live.textContent = "The saved receipt could not be read.";
  }
}
void restoreReceipt();

addEventListener("keydown", (event) => {
  if (event.key === "Tab") transition("keyboard_navigation");
});
addEventListener(
  "pointerdown",
  (event) => {
    if (event.pointerType === "touch") transition("touch_interaction");
  },
  { passive: true }
);
addEventListener(
  "scroll",
  () => {
    if (reducedMotion || route() !== "/") return;
    const maximum = Math.max(1, document.documentElement.scrollHeight - innerHeight);
    scene?.setScrollProgress(scrollY / maximum);
  },
  { passive: true }
);
addEventListener("pagehide", () => scene?.dispose(), { once: true });
syncVisibleConfiguration();
