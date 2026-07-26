import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";

const root = process.cwd();
const required = [
  "base",
  "outer_shell",
  "glass_core",
  "eclipse_disk",
  "acoustic_membrane",
  "thermal_grille",
  "rotary_control",
  "braided_cable",
  "internal_frame",
  "logic_board",
  "left_driver",
  "right_driver"
];
const blenderCandidates = [
  process.env.BLENDER_PATH,
  "/Applications/Blender.app/Contents/MacOS/Blender",
  "blender"
].filter(Boolean);
const blender = blenderCandidates.find((item) =>
  item === "blender" ? true : existsSync(item)
);
assert.ok(blender, "Installed Blender is required for the editable scene check.");

const workspace = mkdtempSync(path.join(tmpdir(), "nocturne-3d-check-"));
try {
  const reportPath = path.join(workspace, "scene-report.json");
  const completed = spawnSync(
    blender,
    [
      "--background",
      "--factory-startup",
      "--disable-autoexec",
      path.join(root, "3d", "nocturne-one.blend"),
      "--python-exit-code",
      "1",
      "--python",
      path.join(root, "tests", "blender_scene_check.py"),
      "--",
      reportPath
    ],
    { cwd: root, encoding: "utf8", timeout: 180_000 }
  );
  if (completed.status !== 0) {
    throw new Error(
      `Blender scene check failed.\n${completed.stdout}\n${completed.stderr}`
    );
  }
  const sceneReport = JSON.parse(readFileSync(reportPath, "utf8"));
  assert.equal(sceneReport.passed, true);

  const outputs = [
    ["hero", path.join(root, "public", "assets", "nocturne-one-hero.glb"), 5_242_880],
    ["low", path.join(root, "public", "assets", "nocturne-one-low.glb"), 1_572_864]
  ];
  const glbReports = [];
  for (const [identity, filename, maximum] of outputs) {
    const bytes = readFileSync(filename);
    assert.equal(bytes.toString("ascii", 0, 4), "glTF", `${identity} GLB magic`);
    assert.equal(bytes.readUInt32LE(4), 2, `${identity} GLB version`);
    assert.equal(bytes.readUInt32LE(8), bytes.length, `${identity} GLB length`);
    assert.ok(statSync(filename).size <= maximum, `${identity} GLB size budget`);
    const jsonLength = bytes.readUInt32LE(12);
    const chunkType = bytes.toString("ascii", 16, 20);
    assert.equal(chunkType, "JSON");
    const manifest = JSON.parse(
      bytes.toString("utf8", 20, 20 + jsonLength).trimEnd()
    );
    const names = new Set((manifest.nodes ?? []).map((node) => node.name));
    for (const name of required) {
      assert.ok(names.has(name), `${identity} GLB is missing node ${name}`);
    }
    for (const mesh of manifest.meshes ?? []) {
      for (const primitive of mesh.primitives ?? []) {
        if (primitive.indices !== undefined) {
          assert.ok(
            primitive.indices >= 0 && primitive.indices < manifest.accessors.length
          );
        }
        for (const accessor of Object.values(primitive.attributes ?? {})) {
          assert.ok(accessor >= 0 && accessor < manifest.accessors.length);
        }
      }
    }
    glbReports.push({
      identity,
      bytes: bytes.length,
      required_nodes: required.length,
      valid: true
    });
  }
  console.log(
    JSON.stringify({
      passed: true,
      blend: {
        required_parts: sceneReport.observed_parts.length,
        primary_dimensions_mm: sceneReport.primary_dimensions_mm,
        animated_parts: sceneReport.animated_parts.length,
        mesh_issues: sceneReport.mesh_issues
      },
      glbs: glbReports
    })
  );
} finally {
  rmSync(workspace, { recursive: true, force: true });
}
