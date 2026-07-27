#!/usr/bin/env python3
"""Execute the V2 organic and fur lane end to end and write a receipt.

Builds the four organic targets in real Blender, retopologizes and unwraps
them, grooms the synthetic animal bust, critiques the groom, and records every
measured number. Exits non-zero when a gate fails.

The animal target here is *synthetic*: its construction parameters are the
ground truth. Nothing this script produces is evidence about a real animal.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.blender.v2_executor import (  # noqa: E402
    BlenderExecutionError,
    V2BlenderExecutor,
)
from blender_vision.core.util import utc_now  # noqa: E402
from blender_vision.grooming.fur import (  # noqa: E402
    FurGroomer,
    GroomParameters,
    critique_groom,
)
from blender_vision.organic.topology import (  # noqa: E402
    TopologyService,
    lod_identity_violations,
)
from blender_vision.v2.authority import AuthorityClass  # noqa: E402

BUILD_SCRIPTS = ROOT / "src" / "blender_vision" / "organic" / "build_scripts"

TARGETS = ("organic_sculpture", "plant", "draped_cloth", "animal_bust")

# Retopology settings per target. The plant is a thin branching form that
# quadriflow cannot close, so it takes a voxel pass instead; saying so here is
# cheaper than pretending one setting fits every organic shape.
RETOPO = {
    "organic_sculpture": {"mode": "quad", "target_faces": 6000},
    "plant": {"mode": "voxel", "voxel_size": 0.004},
    "draped_cloth": {"mode": "quad", "target_faces": 5000},
    "animal_bust": {"mode": "quad", "target_faces": 4000},
}

LODS = [{"name": "L1", "ratio": 0.5}, {"name": "L2", "ratio": 0.2}, {"name": "L3", "ratio": 0.06}]

# Gates, declared before the run.
MAX_UV_ANGLE_DISTORTION_DEG = 70.0
MIN_LOD_SILHOUETTE_IOU = 0.9
MIN_UV_PACKING = 0.35


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, default=ROOT / "artifacts" / "v2" / "organic")
    parser.add_argument("--seed", type=int, default=20260726)
    arguments = parser.parse_args()

    output = arguments.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    executor = V2BlenderExecutor()

    receipt: dict = {
        "schema": "v2.organic-fur-lane/1",
        "started_at": utc_now(),
        "blender_version": executor.version,
        "targets": {},
        "gates": {
            "max_uv_angle_distortion_deg": MAX_UV_ANGLE_DISTORTION_DEG,
            "min_lod_silhouette_iou": MIN_LOD_SILHOUETTE_IOU,
            "min_uv_packing": MIN_UV_PACKING,
        },
        "failures": [],
    }

    print("== building organic targets ==")
    build = executor.run(
        BUILD_SCRIPTS / "build_organic_targets.py",
        {"output_dir": str(output / "targets"), "seed": arguments.seed, "cloth_frames": 45},
        expect_marker="V2_ORGANIC_BUILD_OK",
        timeout_seconds=2400,
    )
    ground_truth = json.loads((output / "targets" / "ground-truth.json").read_text())
    receipt["build_script_sha256"] = build.script_sha256
    for name in TARGETS:
        measured = ground_truth["targets"][name]["measured"]
        print(
            f"  {name:20s} tris={measured['triangles']:>7d} "
            f"dims={[round(d, 4) for d in measured['dimensions_m']]} "
            f"watertight={measured['boundary_edges'] == 0 and measured['non_manifold_edges'] == 0}"
        )

    print("\n== retopology, uv, lod ==")
    topology = TopologyService(executor)
    for name in TARGETS:
        try:
            result = topology.process(
                output / "targets" / f"{name}.blend",
                ground_truth["targets"][name]["object"],
                output_dir=output / "topology" / name,
                remesh=RETOPO[name],
                unwrap={"angle_limit_deg": 66.0, "island_margin": 0.003},
                lods=LODS,
            )
        except BlenderExecutionError as error:
            receipt["failures"].append({"target": name, "stage": "topology", "error": str(error)})
            print(f"  {name:20s} FAILED: {error}")
            continue

        retopo, uv = result["retopologized"], result["uv"]
        violations = lod_identity_violations(
            result["lods"], minimum_silhouette_iou=MIN_LOD_SILHOUETTE_IOU
        )
        entry = {
            "construction": ground_truth["targets"][name]["construction"],
            "source": result["source"].to_dict(),
            "retopologized": retopo.to_dict() if retopo else None,
            "uv": uv.to_dict() if uv else None,
            "lods": [item.to_dict() for item in result["lods"]],
            "lod_identity_violations": violations,
            "glb": str(result["glb_path"]),
        }
        receipt["targets"][name] = entry

        print(
            f"  {name:20s} faces={retopo.faces:>6d} quads={retopo.quad_fraction:.2f} "
            f"watertight={retopo.is_watertight} genus={retopo.genus_estimate} "
            f"uv_islands={uv.island_count if uv else 0} "
            f"pack={uv.packing_efficiency if uv else 0:.3f} "
            f"maxAngle={uv.max_angle_distortion_deg if uv else 0:.1f}"
        )
        for lod in result["lods"]:
            print(f"      LOD {lod.name}: {lod.triangles:>6d} tris  iou={lod.silhouette_iou:.4f}")

        if uv and uv.max_angle_distortion_deg > MAX_UV_ANGLE_DISTORTION_DEG:
            receipt["failures"].append(
                {
                    "target": name,
                    "gate": "uv_angle_distortion",
                    "value": uv.max_angle_distortion_deg,
                }
            )
        if uv and uv.packing_efficiency < MIN_UV_PACKING:
            receipt["failures"].append(
                {"target": name, "gate": "uv_packing", "value": uv.packing_efficiency}
            )
        if violations:
            receipt["failures"].append(
                {"target": name, "gate": "lod_identity", "value": violations}
            )

    print("\n== fur groom (synthetic ground truth) ==")
    groom = FurGroomer(executor).groom(
        output / "targets" / "animal_bust.blend",
        "animal_bust",
        output / "fur",
        parameters=GroomParameters(),
        seed=arguments.seed,
    )
    critique = critique_groom(groom, evidence=[groom.script_sha256, str(groom.offline_blend)])
    receipt["fur"] = {
        **groom.to_dict(),
        "critique_passed": critique.passed,
        "findings": [item.to_dict() for item in critique.findings],
        "claim": (
            "Synthetic target with known construction parameters. This is not "
            "evidence about any real animal; the real-animal lane remains blocked "
            "on an authorized multiview capture set."
        ),
    }
    print(
        f"  guides={groom.report['guides']['guide_count']} "
        f"guard={groom.report['guard_strands']} undercoat={groom.report['undercoat_strands']}"
    )
    print(
        f"  clump/body={groom.clump_to_body_ratio:.5f}  "
        f"density={groom.density_per_m2:.0f}/m2  "
        f"shells={groom.report['counts']['shells_triangles']} tris  "
        f"cards={groom.report['counts']['cards_triangles']} tris"
    )
    print(f"  critique passed: {critique.passed}")
    for finding in critique.findings:
        print(f"    {finding.severity}: {finding.finding_id} {finding.measured}")
    if not critique.passed:
        receipt["failures"].append({"target": "animal_bust", "gate": "groom_critique"})

    print("\n== deliberate groom regression (proves the critic bites) ==")
    broken = FurGroomer(executor).groom(
        output / "targets" / "animal_bust.blend",
        "animal_bust",
        output / "fur-broken",
        parameters=GroomParameters(length_m=0.30, clump=0.95, guide_count=60),
        seed=arguments.seed,
    )
    broken_critique = critique_groom(broken, evidence=[broken.script_sha256])
    caught = [item.finding_id for item in broken_critique.findings]
    print(f"  clump/body={broken.clump_to_body_ratio:.5f} density={broken.density_per_m2:.0f}/m2")
    print(f"  critic caught: {caught or 'NOTHING'}")
    receipt["groom_regression"] = {
        "parameters": broken.parameters.to_dict(),
        "clump_to_body_ratio": broken.clump_to_body_ratio,
        "density_per_m2": broken.density_per_m2,
        "caught": caught,
        "passed": broken_critique.passed,
    }
    if broken_critique.passed:
        receipt["failures"].append(
            {"target": "animal_bust", "gate": "groom_critic_missed_injected_regression"}
        )

    receipt["completed_at"] = utc_now()
    receipt["authority"] = AuthorityClass.PROCEDURAL_GROUND_TRUTH.value
    receipt_path = output / "organic-fur-receipt.json"
    receipt_path.write_text(json.dumps(receipt, indent=2, default=str))
    print(f"\nreceipt: {receipt_path}")

    if receipt["failures"]:
        print(f"FAILURES: {json.dumps(receipt['failures'], indent=2, default=str)}")
        return 1
    print("all organic and fur gates passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
