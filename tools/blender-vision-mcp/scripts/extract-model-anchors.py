from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import bpy
from mathutils import Vector


def _arguments() -> argparse.Namespace:
    arguments = sys.argv[sys.argv.index("--") + 1 :] if "--" in sys.argv else []
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True)
    parser.add_argument("--output", required=True)
    return parser.parse_args(arguments)


def main() -> None:
    args = _arguments()
    source = Path(args.input).expanduser().resolve()
    destination = Path(args.output).expanduser().resolve()
    if source.suffix.lower() not in {".glb", ".gltf"}:
        raise ValueError("model-anchor extraction currently requires GLB or glTF input")
    bpy.ops.wm.read_factory_settings(use_empty=True)
    bpy.ops.import_scene.gltf(filepath=str(source))
    anchors = []
    overall_minimum = [float("inf")] * 3
    overall_maximum = [float("-inf")] * 3
    for obj in sorted(bpy.context.scene.objects, key=lambda item: item.name):
        if obj.type != "MESH" or not obj.data.vertices:
            continue
        corners = [obj.matrix_world @ Vector(corner) for corner in obj.bound_box]
        minimum = [min(float(point[axis]) for point in corners) for axis in range(3)]
        maximum = [max(float(point[axis]) for point in corners) for axis in range(3)]
        center = [(minimum[axis] + maximum[axis]) / 2.0 for axis in range(3)]
        dimensions = [maximum[axis] - minimum[axis] for axis in range(3)]
        overall_minimum = [
            min(overall_minimum[axis], minimum[axis]) for axis in range(3)
        ]
        overall_maximum = [
            max(overall_maximum[axis], maximum[axis]) for axis in range(3)
        ]
        anchors.append(
            {
                "object": obj.name,
                "center_model_units": center,
                "minimum_model_units": minimum,
                "maximum_model_units": maximum,
                "dimensions_model_units": dimensions,
                "vertex_count": len(obj.data.vertices),
            }
        )
    document = {
        "schema_version": 1,
        "source": str(source),
        "mesh_count": len(anchors),
        "bounds_model_units": {
            "minimum": overall_minimum,
            "maximum": overall_maximum,
            "dimensions": [
                overall_maximum[axis] - overall_minimum[axis] for axis in range(3)
            ],
        },
        "anchors": anchors,
        "authority": "MODEL_OBJECT_BOUNDS_PROPOSAL_ONLY",
    }
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(document, indent=2, sort_keys=True), encoding="utf-8")
    print(f"VISIONMCP_MODEL_ANCHORS={destination}")


if __name__ == "__main__":
    main()
