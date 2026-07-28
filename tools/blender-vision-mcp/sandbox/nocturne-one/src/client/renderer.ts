import * as THREE from "three";
import { GLTFLoader } from "three/addons/loaders/GLTFLoader.js";

interface Configuration {
  variant: "obsidian" | "lunar" | "ember";
  light_intensity: number;
  orientation: number;
  accessory: "none" | "braided-cable";
}

interface SceneOptions {
  canvas: HTMLCanvasElement;
  asset: string;
  configuration: Configuration;
  reducedMotion: boolean;
}

interface SceneController {
  updateConfiguration(configuration: Configuration): void;
  selectPart(part: string | null): void;
  setScrollProgress(progress: number): void;
  sampleFrames(count: number): Promise<number[]>;
  dispose(): void;
}

const palette = {
  obsidian: {
    shell: new THREE.Color("#111318"),
    membrane: new THREE.Color("#24262B"),
    light: new THREE.Color("#FFB46A")
  },
  lunar: {
    shell: new THREE.Color("#C7C9CA"),
    membrane: new THREE.Color("#9CA0A3"),
    light: new THREE.Color("#FFF2D6")
  },
  ember: {
    shell: new THREE.Color("#3B1E1C"),
    membrane: new THREE.Color("#4A2622"),
    light: new THREE.Color("#FF6B3D")
  }
} as const;

const explodedOffsets: Record<string, THREE.Vector3> = {
  outer_shell: new THREE.Vector3(0, 44, 0),
  glass_core: new THREE.Vector3(0, 0, 46),
  eclipse_disk: new THREE.Vector3(0, 0, -58),
  acoustic_membrane: new THREE.Vector3(0, 0, 82),
  internal_frame: new THREE.Vector3(0, 0, -28),
  logic_board: new THREE.Vector3(0, -18, -76),
  left_driver: new THREE.Vector3(-58, 0, 28),
  right_driver: new THREE.Vector3(58, 0, 28)
};

function materialList(material: THREE.Material | THREE.Material[]): THREE.Material[] {
  return Array.isArray(material) ? material : [material];
}

export async function createNocturneScene(
  options: SceneOptions
): Promise<SceneController> {
  const renderer = new THREE.WebGLRenderer({
    canvas: options.canvas,
    alpha: true,
    antialias: true,
    powerPreference: "high-performance"
  });
  renderer.setPixelRatio(Math.min(devicePixelRatio, innerWidth < 640 ? 1 : 1.25));
  renderer.setClearColor(0x000000, 0);
  renderer.outputColorSpace = THREE.SRGBColorSpace;
  renderer.toneMapping = THREE.ACESFilmicToneMapping;
  renderer.toneMappingExposure = 1.12;

  const world = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(31, 1, 1, 3000);
  camera.position.set(470, 390, 650);
  camera.lookAt(0, 168, 0);
  const ambient = new THREE.HemisphereLight(0x9db9dc, 0x090a0e, 2.2);
  const key = new THREE.DirectionalLight(0xb8d6ff, 5.4);
  key.position.set(-380, 520, 220);
  const rim = new THREE.PointLight(0xff6b3d, 7.5, 1000, 1.8);
  rim.position.set(300, 250, -260);
  world.add(ambient, key, rim);

  const gltf = await new GLTFLoader().loadAsync(options.asset);
  const product = gltf.scene;
  world.add(product);

  const bounds = new THREE.Box3().setFromObject(product);
  const center = bounds.getCenter(new THREE.Vector3());
  product.position.sub(center);
  product.position.y += 176;

  const baseTransforms = new Map<
    THREE.Object3D,
    { position: THREE.Vector3; scale: THREE.Vector3 }
  >();
  product.traverse((object) => {
    baseTransforms.set(object, {
      position: object.position.clone(),
      scale: object.scale.clone()
    });
    if ("material" in object) {
      const mesh = object as THREE.Mesh;
      for (const material of materialList(mesh.material)) {
        material.side = THREE.FrontSide;
      }
    }
  });

  let disposed = false;
  let yaw = -0.52;
  let pitch = 0.08;
  let targetYaw = yaw;
  let targetPitch = pitch;
  let scrollProgress = 0;
  let orientationOffset = THREE.MathUtils.degToRad(
    options.configuration.orientation
  );
  let dirty = true;
  let activePointer: number | null = null;
  let lastPointer = { x: 0, y: 0 };

  function resize(): void {
    const box = options.canvas.getBoundingClientRect();
    const width = Math.max(1, Math.round(box.width));
    const height = Math.max(1, Math.round(box.height));
    renderer.setSize(width, height, false);
    camera.aspect = width / height;
    camera.updateProjectionMatrix();
    dirty = true;
  }

  function updateConfiguration(configuration: Configuration): void {
    const colors = palette[configuration.variant];
    orientationOffset = THREE.MathUtils.degToRad(configuration.orientation);
    product.rotation.y = targetYaw + orientationOffset;
    product.traverse((object) => {
      if (!("material" in object)) return;
      const mesh = object as THREE.Mesh;
      for (const material of materialList(mesh.material)) {
        const standard = material as THREE.MeshStandardMaterial;
        const name = material.name;
        if (name.includes("BLACK_ANODIZED")) standard.color.copy(colors.shell);
        if (name.includes("GRAPHITE_TENSIONED")) {
          standard.color.copy(colors.membrane);
        }
        if (name.includes("WARM_EMISSIVE")) {
          standard.color.copy(colors.light);
          standard.emissive?.copy(colors.light);
          standard.emissiveIntensity = 1.4 + configuration.light_intensity / 24;
        }
      }
      if (object.name.split(".")[0] === "braided_cable") {
        object.visible = configuration.accessory === "braided-cable";
      }
    });
    rim.color.copy(colors.light);
    rim.intensity = 2 + configuration.light_intensity / 8;
    dirty = true;
  }

  function selectPart(part: string | null): void {
    product.traverse((object) => {
      const base = baseTransforms.get(object);
      if (!base) return;
      object.position.copy(base.position);
      object.scale.copy(base.scale);
      const semantic = object.name.split(".")[0]!;
      if (part && explodedOffsets[semantic]) {
        object.position.add(explodedOffsets[semantic]!);
      }
      if (part && semantic === part) object.scale.multiplyScalar(1.045);
    });
    dirty = true;
  }

  function setScrollProgress(progress: number): void {
    scrollProgress = THREE.MathUtils.clamp(progress, 0, 1);
    if (!options.reducedMotion) {
      targetYaw = -0.52 + scrollProgress * 1.1;
      targetPitch = 0.08 - scrollProgress * 0.15;
    }
    dirty = true;
  }

  function pointerDown(event: PointerEvent): void {
    activePointer = event.pointerId;
    lastPointer = { x: event.clientX, y: event.clientY };
    options.canvas.setPointerCapture?.(event.pointerId);
  }

  function pointerMove(event: PointerEvent): void {
    if (activePointer !== event.pointerId) return;
    const dx = event.clientX - lastPointer.x;
    const dy = event.clientY - lastPointer.y;
    lastPointer = { x: event.clientX, y: event.clientY };
    targetYaw += dx * 0.008;
    targetPitch = THREE.MathUtils.clamp(targetPitch + dy * 0.004, -0.35, 0.35);
    dirty = true;
  }

  function pointerUp(event: PointerEvent): void {
    if (activePointer === event.pointerId) activePointer = null;
  }

  function keyDown(event: KeyboardEvent): void {
    if (!["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"].includes(event.key)) {
      return;
    }
    event.preventDefault();
    if (event.key === "ArrowLeft") targetYaw -= 0.12;
    if (event.key === "ArrowRight") targetYaw += 0.12;
    if (event.key === "ArrowUp") targetPitch -= 0.08;
    if (event.key === "ArrowDown") targetPitch += 0.08;
    dirty = true;
  }

  options.canvas.addEventListener("pointerdown", pointerDown);
  options.canvas.addEventListener("pointermove", pointerMove);
  options.canvas.addEventListener("pointerup", pointerUp);
  options.canvas.addEventListener("pointercancel", pointerUp);
  options.canvas.addEventListener("keydown", keyDown);
  addEventListener("resize", resize);
  resize();
  updateConfiguration(options.configuration);

  function animate(): void {
    if (disposed) return;
    requestAnimationFrame(animate);
    if (!options.reducedMotion) {
      const nextYaw = THREE.MathUtils.lerp(yaw, targetYaw, 0.075);
      const nextPitch = THREE.MathUtils.lerp(pitch, targetPitch, 0.075);
      if (Math.abs(nextYaw - yaw) > 0.00005 || Math.abs(nextPitch - pitch) > 0.00005) {
        dirty = true;
      }
      yaw = nextYaw;
      pitch = nextPitch;
      product.rotation.y = yaw + orientationOffset;
      product.rotation.x = pitch;
      camera.position.y = 390 - scrollProgress * 34;
    }
    if (dirty) {
      renderer.render(world, camera);
      dirty = false;
    }
  }
  await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
  animate();

  return {
    updateConfiguration,
    selectPart,
    setScrollProgress,
    async sampleFrames(count: number): Promise<number[]> {
      const samples: number[] = [];
      while (samples.length < count) {
        await new Promise<void>((resolve) =>
          requestAnimationFrame(() => {
            const started = performance.now();
            renderer.render(world, camera);
            samples.push(Math.max(0.1, performance.now() - started));
            resolve();
          })
        );
      }
      return samples;
    },
    dispose(): void {
      disposed = true;
      removeEventListener("resize", resize);
      options.canvas.removeEventListener("pointerdown", pointerDown);
      options.canvas.removeEventListener("pointermove", pointerMove);
      options.canvas.removeEventListener("pointerup", pointerUp);
      options.canvas.removeEventListener("pointercancel", pointerUp);
      options.canvas.removeEventListener("keydown", keyDown);
      product.traverse((object) => {
        if ("geometry" in object) (object as THREE.Mesh).geometry.dispose();
        if ("material" in object) {
          for (const material of materialList((object as THREE.Mesh).material)) {
            material.dispose();
          }
        }
      });
      renderer.dispose();
    }
  };
}
