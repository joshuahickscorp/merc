const LOAD_POLICY = "eager";
const LOD_POLICY = "full";
const DPR_CAP = 4;
const FRAME_WORK_MS = 72;
const INTERACTION_WORK_MS = 58;
const RETAINED_NUMBERS_PER_INTERACTION = 350000;
const RESPECT_REDUCED_MOTION = false;

const bootStarted = performance.now();
const search = new URLSearchParams(location.search);
const runId = search.get("run") || "unbound";
const reducedMotion = matchMedia("(prefers-reduced-motion: reduce)").matches;
const state = {
  runId,
  ready: false,
  loadPolicy: LOAD_POLICY,
  lodPolicy: LOD_POLICY,
  adaptiveDpr: DPR_CAP <= 2,
  effectiveDpr: Math.min(devicePixelRatio, DPR_CAP),
  reducedMotionHonored: RESPECT_REDUCED_MOTION && reducedMotion,
  animationEnabled: !(RESPECT_REDUCED_MOTION && reducedMotion),
  animationFrameCount: 0,
  selectedGlb: "NONE",
  selectedGlbBytes: 0,
  selectedTextureBytes: 0,
  shaderCompilationMs: 0,
  javascriptExecutionMs: 0,
  retainedAllocationBytes: 0,
  interactionCount: 0,
  behavior: null,
};
const retained = [];
const burn = milliseconds => {
  const start = performance.now();
  while (performance.now() - start < milliseconds) {
    Math.sqrt(144);
  }
};
const withRun = path => `${path}?run=${encodeURIComponent(runId)}`;

const canvas = document.querySelector("#scene");
const fallback = document.querySelector("#fallback");
const result = document.querySelector("#result");
canvas.width = Math.round(640 * state.effectiveDpr);
canvas.height = Math.round(360 * state.effectiveDpr);
const gl = canvas.getContext("webgl2", {
  alpha: false,
  antialias: false,
  preserveDrawingBuffer: true,
});

const loadAdaptiveAsset = async () => {
  if (state.selectedGlb !== "NONE") return;
  const high = LOD_POLICY === "full";
  const glb = high ? "scene-high.glb" : "scene-low.glb";
  const texture = high ? "texture-high.bin" : "texture-low.bin";
  const [glbResponse, textureResponse] = await Promise.all([
    fetch(withRun(glb)),
    fetch(withRun(texture)),
  ]);
  const [glbBytes, textureBytes] = await Promise.all([
    glbResponse.arrayBuffer(),
    textureResponse.arrayBuffer(),
  ]);
  state.selectedGlb = high ? "HIGH" : "LOW";
  state.selectedGlbBytes = glbBytes.byteLength;
  state.selectedTextureBytes = textureBytes.byteLength;
  if (gl) {
    const side = high ? 256 : 64;
    const textureHandle = gl.createTexture();
    gl.bindTexture(gl.TEXTURE_2D, textureHandle);
    gl.texImage2D(
      gl.TEXTURE_2D,
      0,
      gl.RGBA,
      side,
      side,
      0,
      gl.RGBA,
      gl.UNSIGNED_BYTE,
      null,
    );
  }
};

if (!gl) {
  canvas.hidden = true;
  fallback.hidden = false;
} else {
  const compile = (type, source) => {
    const shader = gl.createShader(type);
    gl.shaderSource(shader, source);
    gl.compileShader(shader);
    if (!gl.getShaderParameter(shader, gl.COMPILE_STATUS)) {
      throw new Error(gl.getShaderInfoLog(shader));
    }
    return shader;
  };
  const compileStarted = performance.now();
  const vertex = compile(gl.VERTEX_SHADER, `#version 300 es
    in vec3 position;
    void main() { gl_Position = vec4(position, 1.0); }
  `);
  const fragment = compile(gl.FRAGMENT_SHADER, `#version 300 es
    precision highp float;
    out vec4 color;
    void main() { color = vec4(0.239, 0.612, 1.0, 1.0); }
  `);
  const program = gl.createProgram();
  gl.attachShader(program, vertex);
  gl.attachShader(program, fragment);
  gl.linkProgram(program);
  if (!gl.getProgramParameter(program, gl.LINK_STATUS)) {
    throw new Error(gl.getProgramInfoLog(program));
  }
  state.shaderCompilationMs = performance.now() - compileStarted;
  const positions = new Float32Array([
    -0.62, -0.48, 0,
     0.62, -0.48, 0,
     0.0,   0.62, 0,
  ]);
  const buffer = gl.createBuffer();
  gl.bindBuffer(gl.ARRAY_BUFFER, buffer);
  gl.bufferData(gl.ARRAY_BUFFER, positions, gl.STATIC_DRAW);
  gl.useProgram(program);
  const location = gl.getAttribLocation(program, "position");
  gl.enableVertexAttribArray(location);
  gl.vertexAttribPointer(location, 3, gl.FLOAT, false, 0, 0);
  gl.viewport(0, 0, canvas.width, canvas.height);
  gl.clearColor(0.039, 0.090, 0.153, 1);
  gl.clear(gl.COLOR_BUFFER_BIT);
  gl.drawArrays(gl.TRIANGLES, 0, 3);
}

globalThis.__VISIONMCP_PERFORMANCE__ = {
  state,
  async sampleFrames(count) {
    const durations = [];
    let previous = performance.now();
    for (let index = 0; index < count; index += 1) {
      await new Promise(resolve => requestAnimationFrame(resolve));
      burn(FRAME_WORK_MS);
      const now = performance.now();
      durations.push(now - previous);
      previous = now;
    }
    return durations;
  },
};

const animate = () => {
  state.animationFrameCount += 1;
  if (state.animationEnabled) requestAnimationFrame(animate);
};
if (state.animationEnabled) requestAnimationFrame(animate);

document.querySelector("#inspect").addEventListener("click", async () => {
  burn(INTERACTION_WORK_MS);
  if (RETAINED_NUMBERS_PER_INTERACTION > 0) {
    const values = Array.from(
      {length: RETAINED_NUMBERS_PER_INTERACTION},
      (_value, index) => index,
    );
    retained.push(values);
    state.retainedAllocationBytes += values.length * 8;
  }
  await loadAdaptiveAsset();
  const response = await fetch(withRun("api/items"));
  const payload = await response.json();
  state.behavior = payload;
  state.interactionCount += 1;
  result.value = `Loaded ${payload.items.length} governed views`;
});

burn(FRAME_WORK_MS);
if (LOAD_POLICY === "eager") {
  await loadAdaptiveAsset();
}
state.javascriptExecutionMs = performance.now() - bootStarted;
state.ready = true;
