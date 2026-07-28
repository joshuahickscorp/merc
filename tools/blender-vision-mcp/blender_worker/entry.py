"""Restricted Blender worker entry point.

This module intentionally depends only on Blender's bundled Python and the standard library.
It accepts one validated operation manifest after Blender's ``--`` separator.
"""

from __future__ import annotations

import hashlib
import json
import math
import random
import struct
import sys
from colorsys import hsv_to_rgb
from fnmatch import fnmatchcase
from pathlib import Path
from zlib import compress, crc32

import bmesh
import bpy
from mathutils import Matrix, Quaternion, Vector

ALLOWED_OPERATIONS = {
    "inspect_scene",
    "validate_scene",
    "import_asset",
    "create_component",
    "update_component",
    "apply_constraints",
    "create_camera",
    "apply_camera_solution",
    "render_passes",
    "evaluate_camera_candidates",
    "export_glb",
    "export_blend",
    "generate_lod",
    "prepare_asset",
    "save_checkpoint",
    "repair_degenerate_geometry_candidate",
    "repair_mac_studio_grille",
    "revise_rtx_5090_fe_candidate",
    "refine_rtx_5090_fe_visual_candidate",
    "refine_rtx_5090_fe_front_frame_candidate",
    "refine_dgx_spark_visual_candidate",
    "refine_dgx_spark_base_foot_candidate",
    "generate_components",
    "generate_semantic_seed",
    "generate_synthetic_dataset",
    "generate_calibration_benchmark",
    "generate_asset_preparation_benchmark",
    "generate_appearance_benchmark",
}

AUDIT_MAX_EXACT_MESH_ELEMENTS = 500_000
AUDIT_MAX_SAMPLES_PER_DOMAIN = 100_000


def kelvin_to_rgb(kelvin: float) -> tuple[float, float, float]:
    """Approximate a black-body light colour without third-party dependencies."""
    temperature = min(40000.0, max(1000.0, kelvin)) / 100.0
    if temperature <= 66.0:
        red = 255.0
        green = 99.4708025861 * math.log(temperature) - 161.1195681661
        blue = (
            0.0
            if temperature <= 19.0
            else 138.5177312231 * math.log(temperature - 10.0) - 305.0447927307
        )
    else:
        red = 329.698727446 * ((temperature - 60.0) ** -0.1332047592)
        green = 288.1221695283 * ((temperature - 60.0) ** -0.0755148492)
        blue = 255.0
    return tuple(min(255.0, max(0.0, value)) / 255.0 for value in (red, green, blue))


def confined(root: Path, raw: str, *, must_exist: bool = False) -> Path:
    candidate = Path(raw).expanduser().resolve()
    if candidate != root and root not in candidate.parents:
        raise ValueError(f"path escapes project root: {raw}")
    if must_exist and not candidate.exists():
        raise FileNotFoundError(candidate)
    return candidate


def scene_bounds() -> tuple[Vector, Vector]:
    points: list[Vector] = []
    for obj in bpy.context.scene.objects:
        if obj.type == "MESH" and not obj.hide_render:
            points.extend(obj.matrix_world @ Vector(corner) for corner in obj.bound_box)
    if not points:
        return Vector((-0.5, -0.5, -0.5)), Vector((0.5, 0.5, 0.5))
    return (
        Vector(tuple(min(point[index] for point in points) for index in range(3))),
        Vector(tuple(max(point[index] for point in points) for index in range(3))),
    )


def object_world_bounds(obj: bpy.types.Object) -> dict[str, list[float]]:
    """Return an object's evaluated world-axis bounds for metric component audits."""
    points = [obj.matrix_world @ Vector(corner) for corner in obj.bound_box]
    minimum = [min(float(point[index]) for point in points) for index in range(3)]
    maximum = [max(float(point[index]) for point in points) for index in range(3)]
    return {
        "minimum": minimum,
        "maximum": maximum,
        "dimensions": [maximum[index] - minimum[index] for index in range(3)],
    }


def _sample_indices(count: int, limit: int = AUDIT_MAX_SAMPLES_PER_DOMAIN) -> range:
    """Return a deterministic, bounded, whole-domain sample."""
    stride = max(1, math.ceil(count / limit))
    return range(0, count, stride)


def mesh_topology(mesh: bpy.types.Mesh) -> dict[str, object]:
    """Return mesh-native topology facts without relying on render appearance."""
    vertex_count = len(mesh.vertices)
    edge_count = len(mesh.edges)
    polygon_count = len(mesh.polygons)
    element_count = vertex_count + edge_count + polygon_count
    if element_count > AUDIT_MAX_EXACT_MESH_ELEMENTS:
        return {
            "connected_components": None,
            "boundary_edges": None,
            "non_manifold_edges": None,
            "euler_characteristic": vertex_count - edge_count + polygon_count,
            "closed_surface_genus": None,
            "exact": False,
            "reason": "mesh exceeds bounded exact-topology audit budget",
            "element_count": element_count,
            "maximum_exact_elements": AUDIT_MAX_EXACT_MESH_ELEMENTS,
        }
    parent = list(range(vertex_count))

    def find(index: int) -> int:
        while parent[index] != index:
            parent[index] = parent[parent[index]]
            index = parent[index]
        return index

    def union(left: int, right: int) -> None:
        left_root, right_root = find(left), find(right)
        if left_root != right_root:
            parent[right_root] = left_root

    for edge in mesh.edges:
        union(int(edge.vertices[0]), int(edge.vertices[1]))
    used_vertices = {vertex for polygon in mesh.polygons for vertex in polygon.vertices}
    connected_components = len({find(int(vertex)) for vertex in used_vertices})
    face_uses: dict[tuple[int, int], int] = {}
    for polygon in mesh.polygons:
        for edge in polygon.edge_keys:
            key = tuple(sorted((int(edge[0]), int(edge[1]))))
            face_uses[key] = face_uses.get(key, 0) + 1
    boundary_edges = sum(count == 1 for count in face_uses.values())
    non_manifold_edges = sum(count != 2 for count in face_uses.values())
    euler = vertex_count - len(mesh.edges) + len(mesh.polygons)
    genus = None
    if connected_components and boundary_edges == 0 and non_manifold_edges == 0:
        genus = connected_components - euler / 2.0
    return {
        "connected_components": connected_components,
        "boundary_edges": boundary_edges,
        "non_manifold_edges": non_manifold_edges,
        "euler_characteristic": euler,
        "closed_surface_genus": genus,
        "exact": True,
        "element_count": element_count,
        "maximum_exact_elements": AUDIT_MAX_EXACT_MESH_ELEMENTS,
    }


def _json_socket_value(value: object) -> object:
    if isinstance(value, (int, float, bool, str)):
        return value
    try:
        return [float(item) for item in value]
    except (TypeError, ValueError):
        return str(value)


def _node_input(node, *names: str) -> object | None:
    for name in names:
        if name in node.inputs:
            return _json_socket_value(node.inputs[name].default_value)
    return None


def _material_inspection(root: Path, material) -> dict[str, object]:
    nodes = list(material.node_tree.nodes) if material.use_nodes and material.node_tree else []
    principled = next(
        (node for node in nodes if node.bl_idname == "ShaderNodeBsdfPrincipled"),
        None,
    )
    image_records = []
    for node in nodes:
        image = getattr(node, "image", None)
        if image is None:
            continue
        absolute_path = (
            Path(bpy.path.abspath(image.filepath)).resolve()
            if image.filepath
            else None
        )
        confined_to_project = bool(
            absolute_path
            and (absolute_path == root or root in absolute_path.parents)
        )
        file_exists = bool(absolute_path and absolute_path.is_file())
        image_records.append(
            {
                "node": node.name,
                "image": image.name,
                "source": image.source,
                "packed": image.packed_file is not None,
                "filepath": (
                    str(absolute_path.relative_to(root))
                    if confined_to_project and absolute_path
                    else None
                ),
                "project_confined": confined_to_project,
                "file_exists": file_exists,
                "file_sha256": (
                    hashlib.sha256(absolute_path.read_bytes()).hexdigest()
                    if file_exists and absolute_path
                    else None
                ),
                "size": [int(image.size[0]), int(image.size[1])],
                "colorspace": image.colorspace_settings.name,
            }
        )
    principled_values = (
        {
            "base_color": _node_input(principled, "Base Color"),
            "metallic": _node_input(principled, "Metallic"),
            "roughness": _node_input(principled, "Roughness"),
            "ior": _node_input(principled, "IOR"),
            "alpha": _node_input(principled, "Alpha"),
            "transmission": _node_input(
                principled, "Transmission Weight", "Transmission"
            ),
            "emission_color": _node_input(
                principled, "Emission Color", "Emission"
            ),
            "emission_strength": _node_input(principled, "Emission Strength"),
            "anisotropic": _node_input(
                principled, "Anisotropic IOR Level", "Anisotropic"
            ),
            "coat_weight": _node_input(principled, "Coat Weight", "Clearcoat"),
        }
        if principled is not None
        else None
    )
    record = {
        "name": material.name,
        "material_class": str(
            material.get("bvmcp_material_class", "unclassified")
        ),
        "use_nodes": bool(material.use_nodes),
        "node_types": sorted(node.bl_idname for node in nodes),
        "principled": principled_values,
        "images": image_records,
        "diffuse_color": [float(value) for value in material.diffuse_color],
        "surface_render_method": getattr(material, "surface_render_method", None),
        "pbr_authority": material.get("bvmcp_pbr_authority"),
    }
    record["structural_sha256"] = hashlib.sha256(
        json.dumps(record, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()
    return record


def _camera_inspection(camera) -> dict[str, object]:
    data = camera.data
    scene = bpy.context.scene
    width = int(scene.render.resolution_x * scene.render.resolution_percentage / 100)
    height = int(scene.render.resolution_y * scene.render.resolution_percentage / 100)
    sensor_width = float(data.sensor_width)
    fx = float(data.lens) / sensor_width * width if sensor_width else None
    record = {
        "name": camera.name,
        "type": data.type,
        "world_from_camera": [
            [float(camera.matrix_world[row][column]) for column in range(4)]
            for row in range(4)
        ],
        "lens_mm": float(data.lens),
        "sensor_width_mm": sensor_width,
        "sensor_height_mm": float(data.sensor_height),
        "sensor_fit": data.sensor_fit,
        "shift_x": float(data.shift_x),
        "shift_y": float(data.shift_y),
        "clip_start": float(data.clip_start),
        "clip_end": float(data.clip_end),
        "render_resolution": [width, height],
        "derived_intrinsics": (
            {
                "fx": fx,
                "fy": fx
                * float(scene.render.pixel_aspect_x)
                / max(float(scene.render.pixel_aspect_y), 1e-12),
                "cx": width * (0.5 - float(data.shift_x)),
                "cy": height / 2.0 + width * float(data.shift_y),
            }
            if fx is not None
            else None
        ),
    }
    record["structural_sha256"] = hashlib.sha256(
        json.dumps(record, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()
    return record


def _light_inspection(light) -> dict[str, object]:
    data = light.data
    record = {
        "name": light.name,
        "type": data.type,
        "world_from_light": [
            [float(light.matrix_world[row][column]) for column in range(4)]
            for row in range(4)
        ],
        "color": [float(value) for value in data.color],
        "energy": float(data.energy),
        "use_shadow": bool(data.use_shadow),
        "shape": getattr(data, "shape", None),
        "size": float(getattr(data, "size", 0.0)),
        "size_y": float(getattr(data, "size_y", 0.0)),
        "angle": float(getattr(data, "angle", 0.0)),
    }
    record["structural_sha256"] = hashlib.sha256(
        json.dumps(record, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()
    return record


def inspect_scene(root: Path, safe: bool) -> dict[str, object]:
    minimum, maximum = scene_bounds()
    objects = []
    totals = {"objects": 0, "meshes": 0, "vertices": 0, "edges": 0, "polygons": 0}
    findings = []
    component_bindings = []
    unbound_mesh_objects = []
    for obj in bpy.data.objects:
        component_id = obj.get("bvmcp_component_id") or obj.get("component_id")
        record: dict[str, object] = {
            "name": obj.name,
            "type": obj.type,
            "location": list(obj.location),
            "rotation_euler": list(obj.rotation_euler),
            "scale": list(obj.scale),
            "dimensions": list(obj.dimensions),
            "hidden_render": bool(obj.hide_render),
            "modifiers": [{"name": item.name, "type": item.type} for item in obj.modifiers],
            "component_id": str(component_id) if component_id else None,
        }
        totals["objects"] += 1
        if any(abs(float(component) - 1.0) > 1e-6 for component in obj.scale):
            findings.append(
                {
                    "severity": "warning",
                    "code": "UNAPPLIED_SCALE",
                    "object": obj.name,
                    "scale": list(obj.scale),
                }
            )
        if obj.type == "MESH":
            mesh = obj.data
            record["world_bounds"] = object_world_bounds(obj)
            if component_id:
                component_bindings.append({"object": obj.name, "component_id": str(component_id)})
            else:
                unbound_mesh_objects.append(obj.name)
            exact_mesh_diagnostics = (
                len(mesh.vertices) + len(mesh.edges) + len(mesh.polygons)
                <= AUDIT_MAX_EXACT_MESH_ELEMENTS
            )
            all_element_diagnostics = (
                exact_mesh_diagnostics
                and len(mesh.vertices) <= AUDIT_MAX_SAMPLES_PER_DOMAIN
                and len(mesh.polygons) <= AUDIT_MAX_SAMPLES_PER_DOMAIN
            )
            vertex_indices = _sample_indices(len(mesh.vertices))
            polygon_indices = _sample_indices(len(mesh.polygons))
            if exact_mesh_diagnostics:
                used_vertices = {vertex for polygon in mesh.polygons for vertex in polygon.vertices}
                loose_vertices = len(mesh.vertices) - len(used_vertices)
            else:
                loose_vertices = None
            degenerate_polygons = sum(
                1 for polygon_index in polygon_indices if mesh.polygons[polygon_index].area <= 1e-12
            )
            coordinate_counts: dict[tuple[float, float, float], int] = {}
            for vertex_index in vertex_indices:
                vertex = mesh.vertices[vertex_index]
                key = tuple(round(float(value), 9) for value in vertex.co)
                coordinate_counts[key] = coordinate_counts.get(key, 0) + 1
            duplicate_vertex_positions = sum(count - 1 for count in coordinate_counts.values())
            face_counts: dict[tuple[int, ...], int] = {}
            for polygon_index in polygon_indices:
                polygon = mesh.polygons[polygon_index]
                key = tuple(sorted(int(value) for value in polygon.vertices))
                face_counts[key] = face_counts.get(key, 0) + 1
            duplicate_faces = sum(count - 1 for count in face_counts.values())
            zero_length_normals = sum(
                1
                for polygon_index in polygon_indices
                if mesh.polygons[polygon_index].normal.length_squared <= 1e-16
            )
            mirrored_transform = obj.matrix_world.to_3x3().determinant() < 0.0
            record["mesh"] = {
                "vertices": len(mesh.vertices),
                "edges": len(mesh.edges),
                "polygons": len(mesh.polygons),
                "materials": [
                    slot.material.name if slot.material else None for slot in obj.material_slots
                ],
                "loose_vertices": loose_vertices,
                "degenerate_polygons": degenerate_polygons,
                "duplicate_vertex_positions": duplicate_vertex_positions,
                "duplicate_faces": duplicate_faces,
                "audit_sampling": {
                    "exact": all_element_diagnostics,
                    "maximum_samples_per_domain": AUDIT_MAX_SAMPLES_PER_DOMAIN,
                    "sampled_vertices": len(vertex_indices),
                    "sampled_polygons": len(polygon_indices),
                    "vertex_stride": vertex_indices.step,
                    "polygon_stride": polygon_indices.step,
                    "loose_vertex_count_exact": loose_vertices is not None,
                },
                "normal_diagnostics": {
                    "zero_length_polygon_normals": zero_length_normals,
                    "custom_split_normals": bool(mesh.has_custom_normals),
                    "mirrored_transform": mirrored_transform,
                },
                "topology": mesh_topology(mesh),
            }
            topology = record["mesh"]["topology"]
            semantic_name = " ".join(
                [obj.name, *[item or "" for item in record["mesh"]["materials"]]]
            ).lower()
            if (
                any(token in semantic_name for token in ("vent", "grille", "perforat"))
                and topology["closed_surface_genus"] == 0
                and len(mesh.polygons) > 4
            ):
                findings.append(
                    {
                        "severity": "warning",
                        "code": "CLOSED_SOLID_VENT_OR_GRILLE",
                        "object": obj.name,
                        "closed_surface_genus": 0,
                        "detail": (
                            "Topology contains no handles; a claimed perforated field requires "
                            "direct aperture or ray-cast evidence."
                        ),
                    }
                )
            if not all_element_diagnostics:
                findings.append(
                    {
                        "severity": "warning",
                        "code": "DENSE_MESH_DIAGNOSTICS_SAMPLED",
                        "object": obj.name,
                        "element_count": (
                            len(mesh.vertices) + len(mesh.edges) + len(mesh.polygons)
                        ),
                        "maximum_exact_elements": AUDIT_MAX_EXACT_MESH_ELEMENTS,
                        "maximum_samples_per_domain": AUDIT_MAX_SAMPLES_PER_DOMAIN,
                        "detail": (
                            "Duplicate, degeneracy, normal, and topology diagnostics are bounded; "
                            "absence of a sampled defect is not an exact all-elements claim."
                        ),
                    }
                )
            if loose_vertices:
                findings.append(
                    {
                        "severity": "warning",
                        "code": "LOOSE_VERTICES",
                        "object": obj.name,
                        "count": loose_vertices,
                    }
                )
            if degenerate_polygons:
                findings.append(
                    {
                        "severity": "warning",
                        "code": "DEGENERATE_POLYGONS",
                        "object": obj.name,
                        "count": degenerate_polygons,
                    }
                )
            if duplicate_vertex_positions or duplicate_faces:
                findings.append(
                    {
                        "severity": "warning",
                        "code": "DUPLICATE_GEOMETRY",
                        "object": obj.name,
                        "duplicate_vertex_positions": duplicate_vertex_positions,
                        "duplicate_faces": duplicate_faces,
                    }
                )
            if zero_length_normals or mirrored_transform:
                findings.append(
                    {
                        "severity": "warning",
                        "code": "NORMAL_ORIENTATION_RISK",
                        "object": obj.name,
                        "zero_length_polygon_normals": zero_length_normals,
                        "mirrored_transform": mirrored_transform,
                    }
                )
            if topology["non_manifold_edges"]:
                findings.append(
                    {
                        "severity": "warning",
                        "code": "NON_MANIFOLD_GEOMETRY",
                        "object": obj.name,
                        "count": topology["non_manifold_edges"],
                    }
                )
            if obj.name.split(".", 1)[0] in {"Cube", "Cylinder", "Cone", "Plane", "Sphere"}:
                findings.append(
                    {
                        "severity": "warning",
                        "code": "GENERIC_OBJECT_NAME",
                        "object": obj.name,
                    }
                )
            totals["meshes"] += 1
            totals["vertices"] += len(mesh.vertices)
            totals["edges"] += len(mesh.edges)
            totals["polygons"] += len(mesh.polygons)
        objects.append(record)
    libraries = []
    violations = []
    for library in bpy.data.libraries:
        path = Path(bpy.path.abspath(library.filepath)).resolve()
        libraries.append(str(path))
        if safe and path != root and root not in path.parents:
            violations.append(f"external linked library: {path.name}")
    missing_images = []
    for image in bpy.data.images:
        if image.source == "FILE" and image.packed_file is None:
            path = Path(bpy.path.abspath(image.filepath)).resolve()
            if not path.is_file():
                missing_images.append(image.name)
    scale_to_millimetres = float(bpy.context.scene.unit_settings.scale_length) * 1000.0
    canonical_minimum = [float(value) * scale_to_millimetres for value in minimum]
    canonical_maximum = [float(value) * scale_to_millimetres for value in maximum]
    canonical_dimensions = [float(value) * scale_to_millimetres for value in maximum - minimum]
    findings.extend(
        {"severity": "error", "code": "MISSING_IMAGE", "image": name} for name in missing_images
    )
    findings.extend(
        {"severity": "error", "code": "SAFE_MODE_VIOLATION", "detail": detail}
        for detail in violations
    )
    world_record: dict[str, object] = {
        "name": bpy.context.scene.world.name if bpy.context.scene.world else None,
        "use_nodes": bool(
            bpy.context.scene.world and bpy.context.scene.world.use_nodes
        ),
        "background_color": None,
        "background_strength": None,
        "environment_images": [],
    }
    world = bpy.context.scene.world
    if world and world.use_nodes and world.node_tree:
        background = world.node_tree.nodes.get("Background")
        if background:
            world_record["background_color"] = _json_socket_value(
                background.inputs["Color"].default_value
            )
            world_record["background_strength"] = float(
                background.inputs["Strength"].default_value
            )
        world_record["environment_images"] = sorted(
            node.image.name
            for node in world.node_tree.nodes
            if node.bl_idname == "ShaderNodeTexEnvironment"
            and getattr(node, "image", None) is not None
        )
    world_record["structural_sha256"] = hashlib.sha256(
        json.dumps(world_record, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()
    return {
        "blender_version": bpy.app.version_string,
        "scene": bpy.context.scene.name,
        "units": {
            "system": bpy.context.scene.unit_settings.system,
            "scale_length": bpy.context.scene.unit_settings.scale_length,
            "length_unit": bpy.context.scene.unit_settings.length_unit,
        },
        "bounds": {
            "minimum": list(minimum),
            "maximum": list(maximum),
            "dimensions": list(maximum - minimum),
        },
        "canonical_transform": {
            "source_frame": "blender_world",
            "target_frame": "bvmcp_right_handed_z_up_millimetres",
            "scale_to_millimetres": scale_to_millimetres,
            "orientation": "identity",
            "uncertainty_millimetres": None,
        },
        "canonical_bounds_mm": {
            "minimum": canonical_minimum,
            "maximum": canonical_maximum,
            "dimensions": canonical_dimensions,
        },
        "totals": totals,
        "objects": objects,
        "component_correspondence": {
            "bound": component_bindings,
            "unbound_mesh_objects": unbound_mesh_objects,
            "bound_fraction": (
                len(component_bindings)
                / max(1, len(component_bindings) + len(unbound_mesh_objects))
            ),
        },
        "materials": [material.name for material in bpy.data.materials],
        "material_details": [
            _material_inspection(root, material)
            for material in sorted(bpy.data.materials, key=lambda item: item.name)
        ],
        "camera_details": [
            _camera_inspection(camera)
            for camera in sorted(
                (item for item in bpy.context.scene.objects if item.type == "CAMERA"),
                key=lambda item: item.name,
            )
        ],
        "light_details": [
            _light_inspection(light)
            for light in sorted(
                (item for item in bpy.context.scene.objects if item.type == "LIGHT"),
                key=lambda item: item.name,
            )
        ],
        "environment": world_record,
        "linked_libraries": libraries,
        "missing_images": missing_images,
        "safe_mode_violations": violations,
        "audit_findings": findings,
    }


def point_camera(camera: bpy.types.Object, target: Vector, roll_degrees: float = 0.0) -> None:
    tracking = (target - camera.location).to_track_quat("-Z", "Y")
    if abs(roll_degrees) > 1e-12:
        camera.rotation_mode = "QUATERNION"
        camera.rotation_quaternion = tracking @ Quaternion(
            (0.0, 0.0, 1.0), math.radians(roll_degrees)
        )
    else:
        camera.rotation_mode = "XYZ"
        camera.rotation_euler = tracking.to_euler()


def ensure_studio_lighting(
    center: Vector, camera: bpy.types.Object, radius: float
) -> dict[str, object]:
    existing = [item for item in bpy.context.scene.objects if item.type == "LIGHT"]
    scene_lights = [item for item in existing if not item.name.startswith("BVMCP_")]
    if scene_lights:
        return {"mode": "scene", "lights": [item.name for item in scene_lights]}
    for item in existing:
        bpy.data.objects.remove(item, do_unlink=True)
    forward = (center - camera.location).normalized()
    camera_axes = camera.matrix_world.to_3x3()
    right = (camera_axes @ Vector((1.0, 0.0, 0.0))).normalized()
    up = (camera_axes @ Vector((0.0, 1.0, 0.0))).normalized()
    definitions = (
        (
            "BVMCP_Key",
            camera.location + right * radius * 1.1 + up * radius * 0.75,
            3.0,
            4.0,
        ),
        (
            "BVMCP_Fill",
            camera.location - right * radius * 1.1 + up * radius * 0.75,
            1.5,
            4.0,
        ),
        (
            "BVMCP_Rim",
            center + forward * radius * 1.4 + up * radius * 1.1,
            2.0,
            3.0,
        ),
    )
    names = []
    for name, location, energy, size_factor in definitions:
        light_data = bpy.data.lights.new(name, "SUN")
        # Directional validation lights remove distance/scale ambiguity and
        # remain reproducible across millimetre- and metre-authored scenes.
        light_data.energy = energy
        light_data.angle = math.radians(max(3.0, size_factor * 2.0))
        light = bpy.data.objects.new(name, light_data)
        bpy.context.scene.collection.objects.link(light)
        light.location = location
        point_camera(light, center)
        names.append(name)
    return {
        "mode": "fixed_validation_camera_relative",
        "profile": "balanced_camera_local_v2",
        "lights": names,
    }


def image_histogram_metrics(path: Path) -> dict[str, object]:
    image = bpy.data.images.load(str(path), check_existing=False)
    try:
        pixels = image.pixels[:]
        pixel_count = max(1, len(pixels) // 4)
        stride = max(1, math.ceil(pixel_count / 1_000_000))
        histogram = [0] * 16
        opaque = highlights = shadows = 0
        luminance_sum = 0.0
        for pixel_index in range(0, pixel_count, stride):
            offset = pixel_index * 4
            red, green, blue, alpha = pixels[offset : offset + 4]
            if alpha <= 0.01:
                continue
            opaque += 1
            luminance = 0.2126 * red + 0.7152 * green + 0.0722 * blue
            luminance_sum += luminance
            histogram[min(15, max(0, int(luminance * 16.0)))] += 1
            highlights += max(red, green, blue) >= 0.995
            shadows += luminance <= 0.005
        denominator = max(1, opaque)
        return {
            "sampled_opaque_pixels": opaque,
            "histogram_16": histogram,
            "mean_luminance": luminance_sum / denominator,
            "highlight_clipping_fraction": highlights / denominator,
            "shadow_floor_fraction": shadows / denominator,
            "highlight_clipping_detected": highlights / denominator > 0.01,
            "shadow_floor_detected": shadows / denominator > 0.25,
        }
    finally:
        bpy.data.images.remove(image)


def apply_temporary_mesh_material(
    objects: list[bpy.types.Object], material: bpy.types.Material
) -> dict[int, tuple[bpy.types.Mesh, list[bpy.types.Material | None]]]:
    """Apply one material to unique mesh datablocks and retain exact slot state."""
    originals: dict[int, tuple[bpy.types.Mesh, list[bpy.types.Material | None]]] = {}
    for obj in objects:
        mesh = obj.data
        key = int(mesh.as_pointer())
        if key in originals:
            continue
        originals[key] = (mesh, list(mesh.materials))
        mesh.materials.clear()
        mesh.materials.append(material)
    bpy.context.view_layer.update()
    return originals


def restore_mesh_materials(
    originals: dict[int, tuple[bpy.types.Mesh, list[bpy.types.Material | None]]],
) -> None:
    for mesh, materials in originals.values():
        mesh.materials.clear()
        for material in materials:
            mesh.materials.append(material)
    bpy.context.view_layer.update()


def index_palette_rgb(index: int) -> tuple[int, int, int]:
    return (
        (index * 73) % 251 + 1,
        (index * 151) % 251 + 1,
        (index * 199) % 251 + 1,
    )


def write_rgba_png(
    path: Path,
    width: int,
    height: int,
    pixels: bytearray,
    *,
    source_bottom_up: bool,
) -> None:
    def chunk(kind: bytes, data: bytes) -> bytes:
        return (
            struct.pack(">I", len(data))
            + kind
            + data
            + struct.pack(">I", crc32(kind + data) & 0xFFFFFFFF)
        )

    row_bytes = width * 4
    scanlines = bytearray()
    rows = range(height - 1, -1, -1) if source_bottom_up else range(height)
    for row in rows:
        scanlines.append(0)
        start = row * row_bytes
        scanlines.extend(pixels[start : start + row_bytes])
    path.write_bytes(
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0))
        + chunk(b"IDAT", compress(bytes(scanlines), level=9))
        + chunk(b"IEND", b"")
    )


def write_normal_discontinuity_map(
    smooth_normal_path: Path,
    geometric_normal_path: Path,
    output: Path,
) -> dict[str, object]:
    """Encode image-space normal jumps and smooth/geometric normal disagreement."""
    try:
        import numpy as np
        import OpenImageIO as oiio
    except ImportError as error:
        raise RuntimeError(
            "Blender's bundled NumPy and OpenImageIO are required for normal diagnostics"
        ) from error

    def read_rgba(path: Path) -> np.ndarray:
        image_input = oiio.ImageInput.open(str(path))
        if image_input is None:
            raise RuntimeError(f"normal diagnostic input could not be opened: {path}")
        try:
            specification = image_input.spec()
            pixels = image_input.read_image(oiio.FLOAT)
            if pixels is None:
                raise RuntimeError(f"normal diagnostic input could not be decoded: {path}")
            if int(specification.nchannels) < 3:
                raise RuntimeError("normal diagnostic input requires at least three channels")
            return pixels
        finally:
            image_input.close()

    smooth_pixels = read_rgba(smooth_normal_path)
    geometric_pixels = read_rgba(geometric_normal_path)
    if smooth_pixels.shape[:2] != geometric_pixels.shape[:2]:
        raise RuntimeError("normal diagnostic inputs have inconsistent dimensions")

    smooth = smooth_pixels[:, :, :3] * 2.0 - 1.0
    geometric = geometric_pixels[:, :, :3] * 2.0 - 1.0
    smooth_length = np.linalg.norm(smooth, axis=2)
    geometric_length = np.linalg.norm(geometric, axis=2)
    smooth_alpha = (
        smooth_pixels[:, :, 3] > 0.5
        if smooth_pixels.shape[2] > 3
        else smooth_length > 1e-6
    )
    geometric_alpha = (
        geometric_pixels[:, :, 3] > 0.5
        if geometric_pixels.shape[2] > 3
        else geometric_length > 1e-6
    )
    valid = (
        smooth_alpha
        & geometric_alpha
        & np.isfinite(smooth).all(axis=2)
        & np.isfinite(geometric).all(axis=2)
        & (smooth_length > 1e-6)
        & (geometric_length > 1e-6)
    )
    smooth /= np.maximum(smooth_length[:, :, None], 1e-12)
    geometric /= np.maximum(geometric_length[:, :, None], 1e-12)

    residual_radians = np.zeros(valid.shape, dtype=np.float32)
    shading_dot = np.clip(np.sum(smooth * geometric, axis=2), -1.0, 1.0)
    residual_radians[valid] = np.arccos(shading_dot[valid])
    for axis in (0, 1):
        first = [slice(None), slice(None)]
        second = [slice(None), slice(None)]
        first[axis] = slice(None, -1)
        second[axis] = slice(1, None)
        first_key = tuple(first)
        second_key = tuple(second)
        pair_valid = valid[first_key] & valid[second_key]
        pair_dot = np.clip(
            np.sum(
                geometric[first_key] * geometric[second_key],
                axis=2,
            ),
            -1.0,
            1.0,
        )
        pair_angle = np.zeros(pair_valid.shape, dtype=np.float32)
        pair_angle[pair_valid] = np.arccos(pair_dot[pair_valid])
        residual_radians[first_key] = np.maximum(
            residual_radians[first_key], pair_angle
        )
        residual_radians[second_key] = np.maximum(
            residual_radians[second_key], pair_angle
        )

    residual_degrees = np.degrees(residual_radians)
    # Suppress sub-quarter-degree quantization noise and map 25 degrees to the
    # top of the heat scale. Larger discontinuities remain visibly saturated.
    heat = np.clip((residual_degrees - 0.25) / 24.75, 0.0, 1.0)
    encoded = np.zeros((*valid.shape, 4), dtype=np.uint8)
    encoded[:, :, 0] = np.rint(np.clip(heat * 2.0, 0.0, 1.0) * 255.0).astype(
        np.uint8
    )
    encoded[:, :, 1] = np.rint(
        np.clip(4.0 * heat * (1.0 - heat), 0.0, 1.0) * 255.0
    ).astype(np.uint8)
    encoded[:, :, 2] = np.rint(
        np.clip(np.sqrt(heat) * (1.0 - heat), 0.0, 1.0) * 255.0
    ).astype(np.uint8)
    encoded[:, :, 3] = valid.astype(np.uint8) * 255
    write_rgba_png(
        output,
        encoded.shape[1],
        encoded.shape[0],
        bytearray(encoded.tobytes()),
        source_bottom_up=False,
    )
    values = residual_degrees[valid]
    return {
        "engine": "screen_space_normal_discontinuity_v1",
        "valid_pixel_count": int(np.count_nonzero(valid)),
        "nonzero_pixel_count": int(np.count_nonzero(values > 0.25)),
        "mean_degrees": round(float(np.mean(values)), 8) if values.size else None,
        "p95_degrees": round(float(np.percentile(values, 95)), 8)
        if values.size
        else None,
        "maximum_degrees": round(float(np.max(values)), 8) if values.size else None,
        "authority": "DIAGNOSTIC_RENDERED_NORMAL_EVIDENCE",
    }


def render_exact_index_pass(
    scene: bpy.types.Scene,
    meshes: list[bpy.types.Object],
    *,
    indexes_by_object: dict[str, int],
    output: Path,
    silhouette: bool = False,
) -> None:
    """Render Cycles' integer Object Index channel and emit an exact byte palette."""
    if not indexes_by_object or set(indexes_by_object) != {obj.name for obj in meshes}:
        raise ValueError("index pass requires exactly one governed index for every mesh")
    unique_indexes = sorted(set(indexes_by_object.values()))
    if (
        not unique_indexes
        or unique_indexes[0] < 1
        or unique_indexes[-1] > 32767
        or any(isinstance(value, bool) or not isinstance(value, int) for value in unique_indexes)
    ):
        raise ValueError("object pass indexes must be integers between 1 and 32767")

    try:
        import OpenImageIO as oiio
    except ImportError as error:
        raise RuntimeError(
            "Blender's bundled OpenImageIO is required for deterministic object-index decoding"
        ) from error

    original_pass_indexes = {obj.name: int(obj.pass_index) for obj in meshes}
    original_use_nodes = bool(scene.use_nodes)
    original_use_compositing = bool(scene.render.use_compositing)
    original_engine = str(scene.render.engine)
    original_film_transparent = bool(scene.render.film_transparent)
    original_filepath = str(scene.render.filepath)
    original_image_settings = {
        "file_format": str(scene.render.image_settings.file_format),
        "color_mode": str(scene.render.image_settings.color_mode),
        "color_depth": str(scene.render.image_settings.color_depth),
    }
    view_layer = bpy.context.view_layer
    original_use_pass_object_index = bool(view_layer.use_pass_object_index)
    original_cycles = {
        "samples": int(scene.cycles.samples),
        "use_denoising": bool(scene.cycles.use_denoising),
    }
    index_path = output.with_name(f".{output.stem}.object-index.exr")

    try:
        for obj in meshes:
            obj.pass_index = indexes_by_object[obj.name]
        scene.render.engine = "CYCLES"
        # IndexOB is an integer, non-antialiased identity channel. One sample is
        # sufficient and avoids spending appearance-render time on a data pass.
        scene.cycles.samples = 1
        scene.cycles.use_denoising = False
        view_layer.use_pass_object_index = True
        scene.render.film_transparent = True
        scene.use_nodes = False
        scene.render.use_compositing = False
        scene.render.image_settings.file_format = "OPEN_EXR_MULTILAYER"
        scene.render.image_settings.color_mode = "RGBA"
        scene.render.image_settings.color_depth = "32"
        bpy.context.view_layer.update()
        scene.render.filepath = str(index_path)
        bpy.ops.render.render(write_still=True)

        image_input = oiio.ImageInput.open(str(index_path))
        if image_input is None:
            raise RuntimeError("Cycles object-index EXR could not be opened")
        try:
            specification = image_input.spec()
            channel_matches = [
                offset
                for offset, name in enumerate(specification.channelnames)
                if str(name).endswith(".IndexOB.X")
            ]
            if len(channel_matches) != 1:
                raise RuntimeError(
                    "Cycles object-index EXR must contain exactly one IndexOB channel"
                )
            channel = channel_matches[0]
            index_pixels = image_input.read_image(channel, channel + 1, oiio.FLOAT)
            if index_pixels is None:
                raise RuntimeError("Cycles object-index EXR channel could not be decoded")
            result_width = int(specification.width)
            result_height = int(specification.height)
            flat_indexes = index_pixels.reshape(-1)
            if len(flat_indexes) != result_width * result_height:
                raise RuntimeError("Cycles object-index EXR has an unexpected pixel count")
        finally:
            image_input.close()

        classified = bytearray(result_width * result_height * 4)
        for pixel_index, raw_index in enumerate(flat_indexes):
            index_value = float(raw_index)
            palette_index = int(round(index_value))
            if abs(index_value - palette_index) > 1e-5:
                raise RuntimeError(
                    f"Cycles object-index pass contains a fractional value: {index_value}"
                )
            if palette_index == 0:
                continue
            if palette_index not in unique_indexes:
                raise RuntimeError(
                    f"Cycles object-index pass contains an ungoverned index: {palette_index}"
                )
            if silhouette:
                red, green, blue = 255, 255, 255
            else:
                desired = index_palette_rgb(palette_index)
                red, green, blue = desired
            offset = pixel_index * 4
            classified[offset : offset + 4] = bytes((red, green, blue, 255))
        write_rgba_png(
            output,
            result_width,
            result_height,
            classified,
            source_bottom_up=False,
        )
    finally:
        if index_path.is_file():
            index_path.unlink()
        for obj in meshes:
            obj.pass_index = original_pass_indexes[obj.name]
        scene.use_nodes = original_use_nodes
        scene.render.use_compositing = original_use_compositing
        scene.render.engine = original_engine
        scene.render.film_transparent = original_film_transparent
        scene.render.filepath = original_filepath
        scene.render.image_settings.file_format = original_image_settings["file_format"]
        scene.render.image_settings.color_mode = original_image_settings["color_mode"]
        scene.render.image_settings.color_depth = original_image_settings["color_depth"]
        view_layer.use_pass_object_index = original_use_pass_object_index
        scene.cycles.samples = original_cycles["samples"]
        scene.cycles.use_denoising = original_cycles["use_denoising"]
        bpy.context.view_layer.update()


def render_passes(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    output = confined(root, str(parameters["output_path"]))
    output.parent.mkdir(parents=True, exist_ok=True)
    width = max(64, min(int(parameters.get("width", 1024)), 4096))
    height = max(64, min(int(parameters.get("height", 1024)), 4096))
    allowed_passes = {
        "beauty",
        "appearance",
        "exposure_minus_2",
        "exposure_0",
        "exposure_plus_2",
        "neutral_grey_background",
        "white_background",
        "black_background",
        "neutral_clay",
        "material_neutral",
        "grazing_left",
        "grazing_right",
        "grazing_top",
        "silhouette",
        "depth",
        "normal",
        "world_normal",
        "geometric_normal",
        "curvature",
        "object_id",
        "component_id",
        "feature_id",
        "wireframe",
        "zebra",
        "reflected_line",
        "normal_discontinuity",
        "highlight_flow",
    }
    requested_value = parameters.get("requested_passes")
    if requested_value is None:
        requested_passes = set(allowed_passes)
    elif not isinstance(requested_value, list) or not requested_value:
        raise ValueError("requested_passes must be a non-empty array")
    else:
        requested_passes = {str(item) for item in requested_value}
        if requested_passes - allowed_passes:
            raise ValueError("requested_passes contains unsupported render products")
    requested_passes.add("beauty")
    camera_state = parameters.get("camera_state")
    if camera_state is not None and not isinstance(camera_state, dict):
        raise ValueError("camera_state must be an object")
    direction = Vector(parameters.get("view_direction", [-1.0, -1.0, -0.65])).normalized()
    horizontal_fov = float(parameters.get("horizontal_fov_degrees", 50.0))
    fit_margin = float(parameters.get("fit_margin", 1.25))
    lens_shift_x = float(parameters.get("lens_shift_x", 0.0))
    lens_shift_y = float(parameters.get("lens_shift_y", 0.0))
    camera_roll_degrees = float(parameters.get("camera_roll_degrees", 0.0))
    if not 5.0 <= horizontal_fov <= 150.0:
        raise ValueError("horizontal_fov_degrees must be between 5 and 150")
    if not 0.25 <= fit_margin <= 8.0:
        raise ValueError("fit_margin must be between 0.25 and 8")
    if not -1.0 <= lens_shift_x <= 1.0 or not -1.0 <= lens_shift_y <= 1.0:
        raise ValueError("lens shifts must be between -1 and 1")
    if not math.isfinite(camera_roll_degrees) or not -360.0 <= camera_roll_degrees <= 360.0:
        raise ValueError("camera_roll_degrees must be finite and within +/-360")
    camera_data = bpy.data.cameras.new("BVMCP_Camera")
    camera = bpy.data.objects.new("BVMCP_Camera", camera_data)
    bpy.context.scene.collection.objects.link(camera)
    camera_data.type = "PERSP"
    scene = bpy.context.scene
    exact_camera = camera_state is not None
    if exact_camera:
        state = camera_state
        matrix = state.get("extrinsics", {}).get("world_from_camera") or state.get(
            "world_from_camera"
        )
        if not isinstance(matrix, list) or len(matrix) != 4:
            raise ValueError("exact camera_state requires 4x4 world_from_camera extrinsics")
        camera.matrix_world = Matrix(matrix)
        intrinsics = state.get("intrinsics")
        if not isinstance(intrinsics, dict):
            raise ValueError("exact camera_state requires intrinsics")
        distortion = state.get("distortion_model", {})
        distortion_parameters = distortion.get("parameters", {})
        if any(abs(float(value)) > 1e-12 for value in distortion_parameters.values()) and (
            distortion.get("render_policy") != "undistorted_input"
        ):
            raise ValueError(
                "exact evaluation requires undistorted input or an applied distortion pass"
            )
        fx, fy = float(intrinsics["fx"]), float(intrinsics["fy"])
        cx, cy = float(intrinsics["cx"]), float(intrinsics["cy"])
        sensor = state.get("sensor_model", {})
        sensor_width = float(sensor.get("sensor_width_mm", 36.0))
        camera_data.sensor_fit = "HORIZONTAL"
        camera_data.sensor_width = sensor_width
        camera_data.lens = fx / width * sensor_width
        camera_data.shift_x = 0.5 - cx / width
        camera_data.shift_y = (cy - height / 2.0) / width
        clipping = state.get("clipping", {})
        camera_data.clip_start = max(0.000001, float(clipping.get("near", 0.01)))
        camera_data.clip_end = max(
            camera_data.clip_start * 2.0, float(clipping.get("far", 1_000_000.0))
        )
        scene.render.pixel_aspect_x = float(sensor.get("pixel_aspect_x", 1.0))
        scene.render.pixel_aspect_y = float(sensor.get("pixel_aspect_y", fx / fy))
        horizontal_fov = math.degrees(2.0 * math.atan(width / (2.0 * fx)))
        direction = -(camera.matrix_world.to_3x3() @ Vector((0.0, 0.0, 1.0)))
        distance = None
    else:
        minimum, maximum = scene_bounds()
        center = (minimum + maximum) * 0.5
        dimensions = maximum - minimum
        radius = max(dimensions.length * 0.5, 0.01)
        distance = radius / max(math.tan(math.radians(horizontal_fov) / 2.0), 0.05) * fit_margin
        camera.location = center - direction * distance
        point_camera(camera, center, camera_roll_degrees)
        camera_data.angle = math.radians(horizontal_fov)
        camera_data.lens_unit = "FOV"
        camera_data.shift_x = lens_shift_x
        camera_data.shift_y = lens_shift_y
        camera_data.clip_start = max(0.0001, distance / 1000.0)
        camera_data.clip_end = max(1000.0, distance * 20.0)
    minimum, maximum = scene_bounds()
    center = (minimum + maximum) * 0.5
    dimensions = maximum - minimum
    radius = max(dimensions.length * 0.5, 0.01)
    scene.camera = camera
    scene.render.engine = "BLENDER_EEVEE_NEXT"
    # Evidence renders must be replayable across isolated Blender processes.
    # Eevee's temporal reprojection and jittered soft shadows can otherwise
    # move a handful of 8-bit channels even when every scene input is fixed.
    # Retain multisample antialiasing, but use the deterministic sample path.
    eevee = getattr(scene, "eevee", None)
    if eevee is not None:
        eevee.taa_render_samples = 64
        eevee.use_taa_reprojection = False
        eevee.use_soft_shadows = False
        eevee.use_shadow_jitter_viewport = False
    scene.render.resolution_x = width
    scene.render.resolution_y = height
    scene.render.resolution_percentage = 100
    crop_roi = parameters.get("crop_roi_px")
    reported_crop: dict[str, int] | None = None
    if crop_roi is not None:
        if not isinstance(crop_roi, dict):
            raise ValueError("crop_roi_px must be an object")
        reported_crop = {key: int(crop_roi.get(key, -1)) for key in ("x", "y", "width", "height")}
        x_value, y_value = reported_crop["x"], reported_crop["y"]
        crop_width, crop_height = reported_crop["width"], reported_crop["height"]
        if (
            x_value < 0
            or y_value < 0
            or crop_width <= 0
            or crop_height <= 0
            or x_value + crop_width > width
            or y_value + crop_height > height
        ):
            raise ValueError("crop_roi_px must lie inside the full camera frame")
        scene.render.use_border = True
        scene.render.use_crop_to_border = True
        scene.render.border_min_x = x_value / width
        scene.render.border_max_x = (x_value + crop_width) / width
        scene.render.border_min_y = 1.0 - (y_value + crop_height) / height
        scene.render.border_max_y = 1.0 - y_value / height
    else:
        scene.render.use_border = False
        scene.render.use_crop_to_border = False
    # Blender's output dither intentionally perturbs 8-bit quantization.  That
    # is useful for display images but makes independent evidence renders vary
    # by one channel value, so technical renders disable it explicitly.
    scene.render.dither_intensity = 0.0
    scene.render.image_settings.file_format = "PNG"
    scene.render.image_settings.color_mode = "RGBA"
    scene.render.film_transparent = True
    scene.render.filepath = str(output)
    scene.render.use_file_extension = True
    silhouette_only = bool(parameters.get("silhouette_only", False))
    lighting: dict[str, object] = {
        "mode": "scene",
        "lights": [item.name for item in scene.objects if item.type == "LIGHT"],
    }
    if silhouette_only:
        scene.render.engine = "BLENDER_WORKBENCH"
        scene.display.shading.light = "FLAT"
        scene.display.shading.color_type = "OBJECT"
        scene.display.shading.show_shadows = False
        scene.display.shading.show_cavity = False
        scene.display.shading.show_specular_highlight = False
        lighting = {"mode": "silhouette_only", "lights": []}
    elif bool(parameters.get("review_lighting", False)):
        scene.view_settings.exposure = float(parameters.get("review_exposure", -2.5))
        if scene.world is None:
            scene.world = bpy.data.worlds.new("BVMCP_ReviewWorld")
        scene.world.use_nodes = True
        background = scene.world.node_tree.nodes.get("Background")
        if background:
            background.inputs["Color"].default_value = (0.035, 0.035, 0.04, 1.0)
            background.inputs["Strength"].default_value = 0.12
        lighting = ensure_studio_lighting(center, camera, radius)
    bpy.ops.render.render(write_still=True)
    actual_output = output if output.suffix.lower() == ".png" else output.with_suffix(".png")
    passes: dict[str, str] = {"beauty": str(actual_output.relative_to(root))}
    governed = bool(parameters.get("governed_validation", False))
    render_diagnostics: dict[str, object] = {"beauty": image_histogram_metrics(actual_output)}
    if governed and not silhouette_only:
        base_exposure = float(scene.view_settings.exposure)
        if "appearance" in requested_passes:
            passes["appearance"] = str(actual_output.relative_to(root))
        if "exposure_0" in requested_passes:
            passes["exposure_0"] = str(actual_output.relative_to(root))
        for offset in (-2.0, 2.0):
            scene.view_settings.exposure = base_exposure + offset
            label = "minus_2" if offset < 0 else "plus_2"
            pass_name = f"exposure_{label}"
            if pass_name not in requested_passes:
                continue
            bracket_output = actual_output.with_name(f"{actual_output.stem}.exposure-{label}.png")
            scene.render.filepath = str(bracket_output)
            bpy.ops.render.render(write_still=True)
            passes[pass_name] = str(bracket_output.relative_to(root))
            render_diagnostics[pass_name] = image_histogram_metrics(bracket_output)
        scene.view_settings.exposure = base_exposure
        background_passes = {
            "neutral_grey_background": (0.18, 0.18, 0.18, 1.0),
            "white_background": (1.0, 1.0, 1.0, 1.0),
            "black_background": (0.0, 0.0, 0.0, 1.0),
        }
        if requested_passes & set(background_passes):
            if scene.world is None:
                scene.world = bpy.data.worlds.new("BVMCP_ValidationWorld")
            scene.world.use_nodes = True
            world_background = scene.world.node_tree.nodes.get("Background")
            if world_background is None:
                raise RuntimeError("validation world has no Background node")
            original_background_color = tuple(
                world_background.inputs["Color"].default_value
            )
            original_background_strength = float(
                world_background.inputs["Strength"].default_value
            )
            original_film_transparent = bool(scene.render.film_transparent)
            scene.render.film_transparent = False
            for pass_name, color in background_passes.items():
                if pass_name not in requested_passes:
                    continue
                world_background.inputs["Color"].default_value = color
                world_background.inputs["Strength"].default_value = 1.0
                background_output = actual_output.with_name(
                    f"{actual_output.stem}.{pass_name.replace('_', '-')}.png"
                )
                scene.render.filepath = str(background_output)
                bpy.ops.render.render(write_still=True)
                passes[pass_name] = str(background_output.relative_to(root))
                render_diagnostics[pass_name] = image_histogram_metrics(background_output)
            world_background.inputs["Color"].default_value = original_background_color
            world_background.inputs["Strength"].default_value = original_background_strength
            scene.render.film_transparent = original_film_transparent
    object_ids: dict[str, dict[str, object]] = {}
    feature_ids: dict[str, dict[str, object]] = {}
    component_ids: dict[str, dict[str, object]] = {}
    evidence_products = {
        "object_id",
        "component_id",
        "feature_id",
        "silhouette",
        "neutral_clay",
        "material_neutral",
        "grazing_left",
        "grazing_right",
        "grazing_top",
        "depth",
        "normal",
        "world_normal",
        "geometric_normal",
        "curvature",
        "wireframe",
        "zebra",
        "reflected_line",
        "normal_discontinuity",
        "highlight_flow",
    }
    if bool(parameters.get("evidence_passes", False)) and (requested_passes & evidence_products):
        meshes = [obj for obj in scene.objects if obj.type == "MESH" and not obj.hide_render]
        original_colors = {obj.name: tuple(obj.color) for obj in meshes}
        for index, obj in enumerate(sorted(meshes, key=lambda item: item.name), start=1):
            rgb = index_palette_rgb(index)
            object_ids[obj.name] = {
                "rgb": list(rgb),
                "component_id": obj.get("bvmcp_component_id"),
                "component_type": obj.get("bvmcp_component_type"),
            }
        mask_output = actual_output.with_name(f"{actual_output.stem}.instance-mask.png")
        if requested_passes & {
            "object_id",
            "component_id",
            "feature_id",
            "silhouette",
            "neutral_clay",
            "material_neutral",
            "grazing_left",
            "grazing_right",
            "grazing_top",
            "curvature",
            "wireframe",
            "zebra",
            "reflected_line",
            "normal_discontinuity",
            "highlight_flow",
        }:
            scene.render.engine = "BLENDER_WORKBENCH"
            scene.display.shading.light = "FLAT"
            scene.display.shading.color_type = "OBJECT"
            scene.display.shading.show_shadows = False
            scene.display.shading.show_cavity = False
            scene.display.shading.show_specular_highlight = False
            scene.render.image_settings.file_format = "PNG"
            scene.render.image_settings.color_mode = "RGBA"
        original_view_transform = str(scene.view_settings.view_transform)
        original_exposure = float(scene.view_settings.exposure)
        original_gamma = float(scene.view_settings.gamma)
        id_color_state_active = bool(
            requested_passes & {"object_id", "component_id", "feature_id", "silhouette"}
        )
        if id_color_state_active:
            # Technical ID colors are data, not photographed radiance. Raw color management
            # and a compositor-built Object Index product preserve the recorded byte palette
            # without Workbench shading or material/display transforms.
            scene.view_settings.view_transform = "Raw"
            scene.view_settings.exposure = 0.0
            scene.view_settings.gamma = 1.0
            scene.render.image_settings.color_depth = "8"
        if "object_id" in requested_passes:
            render_exact_index_pass(
                scene,
                meshes,
                indexes_by_object={
                    obj.name: index
                    for index, obj in enumerate(
                        sorted(meshes, key=lambda item: item.name), start=1
                    )
                },
                output=mask_output,
            )
            passes["object_id"] = str(mask_output.relative_to(root))
            passes["instance_mask"] = str(mask_output.relative_to(root))
        if governed:
            if "component_id" in requested_passes:
                component_output = actual_output.with_name(f"{actual_output.stem}.component-id.png")
                component_names = {
                    obj.name: str(
                        obj.get("bvmcp_component_id") or obj.get("component_id") or "UNASSIGNED"
                    )
                    for obj in meshes
                }
                component_indexes = {
                    value: index
                    for index, value in enumerate(sorted(set(component_names.values())), start=1)
                }
                for obj in meshes:
                    component_id = component_names[obj.name]
                    rgb = index_palette_rgb(component_indexes[component_id])
                    component_ids[obj.name] = {
                        "component_id": component_id,
                        "rgb": list(rgb),
                    }
                render_exact_index_pass(
                    scene,
                    meshes,
                    indexes_by_object={
                        obj.name: component_indexes[component_names[obj.name]] for obj in meshes
                    },
                    output=component_output,
                )
                passes["component_id"] = str(component_output.relative_to(root))

            if "feature_id" in requested_passes:
                feature_output = actual_output.with_name(f"{actual_output.stem}.feature-id.png")
                feature_names = {
                    obj.name: str(obj.get("bvmcp_feature_id") or "UNASSIGNED") for obj in meshes
                }
                feature_indexes = {
                    value: index
                    for index, value in enumerate(sorted(set(feature_names.values())), start=1)
                }
                for obj in meshes:
                    feature_id = feature_names[obj.name]
                    rgb = index_palette_rgb(feature_indexes[feature_id])
                    feature_ids[obj.name] = {"feature_id": feature_id, "rgb": list(rgb)}
                render_exact_index_pass(
                    scene,
                    meshes,
                    indexes_by_object={
                        obj.name: feature_indexes[feature_names[obj.name]] for obj in meshes
                    },
                    output=feature_output,
                )
                passes["feature_id"] = str(feature_output.relative_to(root))

            if "silhouette" in requested_passes:
                silhouette_output = actual_output.with_name(f"{actual_output.stem}.silhouette.png")
                render_exact_index_pass(
                    scene,
                    meshes,
                    indexes_by_object={obj.name: 1 for obj in meshes},
                    output=silhouette_output,
                    silhouette=True,
                )
                passes["silhouette"] = str(silhouette_output.relative_to(root))

            if id_color_state_active:
                scene.view_settings.view_transform = original_view_transform
                scene.view_settings.exposure = original_exposure
                scene.view_settings.gamma = original_gamma

            if "material_neutral" in requested_passes:
                scene.render.engine = "BLENDER_WORKBENCH"
                neutral_output = actual_output.with_name(
                    f"{actual_output.stem}.material-neutral.png"
                )
                for obj in meshes:
                    obj.color = (0.5, 0.5, 0.5, 1.0)
                scene.display.shading.light = "STUDIO"
                scene.display.shading.show_cavity = True
                scene.render.filepath = str(neutral_output)
                bpy.ops.render.render(write_still=True)
                passes["material_neutral"] = str(neutral_output.relative_to(root))

            clay_family = {
                "neutral_clay": (0.0, True, True),
            }
            original_rotation = float(scene.display.shading.studiolight_rotate_z)
            original_cavity = bool(scene.display.shading.show_cavity)
            original_shadows = bool(scene.display.shading.show_shadows)
            for pass_name, (rotation, shadows, cavity) in clay_family.items():
                if pass_name not in requested_passes:
                    continue
                for obj in meshes:
                    obj.color = (0.52, 0.52, 0.52, 1.0)
                scene.render.engine = "BLENDER_WORKBENCH"
                scene.display.shading.light = "STUDIO"
                scene.display.shading.color_type = "OBJECT"
                scene.display.shading.show_specular_highlight = False
                scene.display.shading.show_shadows = shadows
                scene.display.shading.show_cavity = cavity
                scene.display.shading.studiolight_rotate_z = rotation
                diagnostic_output = actual_output.with_name(
                    f"{actual_output.stem}.{pass_name.replace('_', '-')}.png"
                )
                scene.render.filepath = str(diagnostic_output)
                bpy.ops.render.render(write_still=True)
                passes[pass_name] = str(diagnostic_output.relative_to(root))

            grazing_names = {"grazing_left", "grazing_right", "grazing_top"}
            if requested_passes & grazing_names:
                original_light_visibility = {
                    item.name: bool(item.hide_render)
                    for item in scene.objects
                    if item.type == "LIGHT"
                }
                for item in scene.objects:
                    if item.type == "LIGHT":
                        item.hide_render = True
                grazing_material = bpy.data.materials.new("BVMCP_grazing_clay")
                grazing_material.use_nodes = True
                principled = grazing_material.node_tree.nodes.get("Principled BSDF")
                principled.inputs["Base Color"].default_value = (0.42, 0.42, 0.42, 1.0)
                principled.inputs["Roughness"].default_value = 0.72
                grazing_original_materials = apply_temporary_mesh_material(meshes, grazing_material)
                scene.render.engine = "BLENDER_EEVEE_NEXT"
                scene.render.image_settings.file_format = "PNG"
                scene.render.image_settings.color_mode = "RGBA"
                camera_axes = camera.matrix_world.to_3x3()
                right = (camera_axes @ Vector((1.0, 0.0, 0.0))).normalized()
                up = (camera_axes @ Vector((0.0, 1.0, 0.0))).normalized()
                forward = (camera_axes @ Vector((0.0, 0.0, -1.0))).normalized()
                locations = {
                    "grazing_left": center - right * radius * 2.2 - forward * radius * 1.0,
                    "grazing_right": center + right * radius * 2.2 - forward * radius * 1.0,
                    "grazing_top": center + up * radius * 2.2 - forward * radius * 1.0,
                }
                for pass_name in sorted(requested_passes & grazing_names):
                    light_data = bpy.data.lights.new(f"BVMCP_{pass_name}", "SUN")
                    light_data.energy = 4.0
                    light_data.angle = math.radians(1.5)
                    light = bpy.data.objects.new(f"BVMCP_{pass_name}", light_data)
                    scene.collection.objects.link(light)
                    light.location = locations[pass_name]
                    point_camera(light, center)
                    diagnostic_output = actual_output.with_name(
                        f"{actual_output.stem}.{pass_name.replace('_', '-')}.png"
                    )
                    scene.render.filepath = str(diagnostic_output)
                    bpy.ops.render.render(write_still=True)
                    passes[pass_name] = str(diagnostic_output.relative_to(root))
                    bpy.data.objects.remove(light, do_unlink=True)
                    bpy.data.lights.remove(light_data)
                restore_mesh_materials(grazing_original_materials)
                bpy.data.materials.remove(grazing_material)
                for name, hidden in original_light_visibility.items():
                    scene.objects[name].hide_render = hidden

            if "curvature" in requested_passes:
                scene.render.engine = "BLENDER_WORKBENCH"
                for obj in meshes:
                    obj.color = (0.5, 0.5, 0.5, 1.0)
                scene.display.shading.light = "FLAT"
                scene.display.shading.show_shadows = False
                scene.display.shading.show_cavity = True
                scene.display.shading.cavity_type = "BOTH"
                scene.display.shading.curvature_ridge_factor = 2.0
                scene.display.shading.curvature_valley_factor = 2.0
                curvature_output = actual_output.with_name(f"{actual_output.stem}.curvature.png")
                scene.render.filepath = str(curvature_output)
                bpy.ops.render.render(write_still=True)
                passes["curvature"] = str(curvature_output.relative_to(root))

            if "wireframe" in requested_passes:
                wire_material = bpy.data.materials.new("BVMCP_wireframe_material")
                wire_material.use_nodes = True
                nodes = wire_material.node_tree.nodes
                nodes.clear()
                wire = nodes.new("ShaderNodeWireframe")
                wire.use_pixel_size = True
                wire.inputs["Size"].default_value = 1.0
                mix = nodes.new("ShaderNodeMixRGB")
                mix.blend_type = "MIX"
                mix.inputs[1].default_value = (0.52, 0.52, 0.52, 1.0)
                mix.inputs[2].default_value = (0.02, 0.02, 0.02, 1.0)
                emission = nodes.new("ShaderNodeEmission")
                output_node = nodes.new("ShaderNodeOutputMaterial")
                wire_material.node_tree.links.new(wire.outputs[0], mix.inputs[0])
                wire_material.node_tree.links.new(mix.outputs[0], emission.inputs["Color"])
                wire_material.node_tree.links.new(
                    emission.outputs[0], output_node.inputs["Surface"]
                )
                wire_original_materials = apply_temporary_mesh_material(meshes, wire_material)
                scene.render.engine = "BLENDER_EEVEE_NEXT"
                scene.render.image_settings.file_format = "PNG"
                scene.render.image_settings.color_mode = "RGBA"
                wireframe_output = actual_output.with_name(f"{actual_output.stem}.wireframe.png")
                scene.render.filepath = str(wireframe_output)
                bpy.ops.render.render(write_still=True)
                passes["wireframe"] = str(wireframe_output.relative_to(root))
                restore_mesh_materials(wire_original_materials)
                bpy.data.materials.remove(wire_material)

            for pass_name, direction_value, frequency in (
                ("zebra", (1.0, 0.35, 0.0), 22.0),
                ("reflected_line", (0.15, 0.2, 1.0), 38.0),
            ):
                if pass_name not in requested_passes:
                    continue
                stripe_material = bpy.data.materials.new(f"BVMCP_{pass_name}_material")
                stripe_material.use_nodes = True
                nodes = stripe_material.node_tree.nodes
                nodes.clear()
                geometry = nodes.new("ShaderNodeNewGeometry")
                dot = nodes.new("ShaderNodeVectorMath")
                dot.operation = "DOT_PRODUCT"
                dot.inputs[1].default_value = direction_value
                multiply = nodes.new("ShaderNodeMath")
                multiply.operation = "MULTIPLY"
                multiply.inputs[1].default_value = frequency
                sine = nodes.new("ShaderNodeMath")
                sine.operation = "SINE"
                threshold = nodes.new("ShaderNodeMath")
                threshold.operation = "GREATER_THAN"
                threshold.inputs[1].default_value = 0.0
                mix = nodes.new("ShaderNodeMixRGB")
                mix.blend_type = "MIX"
                mix.inputs[1].default_value = (0.005, 0.005, 0.005, 1.0)
                mix.inputs[2].default_value = (0.98, 0.98, 0.98, 1.0)
                emission = nodes.new("ShaderNodeEmission")
                output_node = nodes.new("ShaderNodeOutputMaterial")
                stripe_material.node_tree.links.new(geometry.outputs["Normal"], dot.inputs[0])
                stripe_material.node_tree.links.new(dot.outputs["Value"], multiply.inputs[0])
                stripe_material.node_tree.links.new(multiply.outputs[0], sine.inputs[0])
                stripe_material.node_tree.links.new(sine.outputs[0], threshold.inputs[0])
                stripe_material.node_tree.links.new(threshold.outputs[0], mix.inputs[0])
                stripe_material.node_tree.links.new(mix.outputs[0], emission.inputs["Color"])
                stripe_material.node_tree.links.new(
                    emission.outputs[0], output_node.inputs["Surface"]
                )
                stripe_original_materials = apply_temporary_mesh_material(meshes, stripe_material)
                scene.render.engine = "BLENDER_EEVEE_NEXT"
                scene.render.image_settings.file_format = "PNG"
                scene.render.image_settings.color_mode = "RGBA"
                stripe_output = actual_output.with_name(
                    f"{actual_output.stem}.{pass_name.replace('_', '-')}.png"
                )
                scene.render.filepath = str(stripe_output)
                bpy.ops.render.render(write_still=True)
                passes[pass_name] = str(stripe_output.relative_to(root))
                restore_mesh_materials(stripe_original_materials)
                bpy.data.materials.remove(stripe_material)

            if "highlight_flow" in requested_passes:
                flow_material = bpy.data.materials.new("BVMCP_highlight_flow_material")
                flow_material.use_nodes = True
                nodes = flow_material.node_tree.nodes
                nodes.clear()
                geometry = nodes.new("ShaderNodeNewGeometry")
                normal_direction = nodes.new("ShaderNodeVectorMath")
                normal_direction.operation = "DOT_PRODUCT"
                normal_direction.inputs[1].default_value = (0.702, 0.201, 0.683)
                frequency = nodes.new("ShaderNodeMath")
                frequency.operation = "MULTIPLY"
                frequency.inputs[1].default_value = 18.0
                sine = nodes.new("ShaderNodeMath")
                sine.operation = "SINE"
                half = nodes.new("ShaderNodeMath")
                half.operation = "MULTIPLY"
                half.inputs[1].default_value = 0.5
                offset = nodes.new("ShaderNodeMath")
                offset.operation = "ADD"
                offset.inputs[1].default_value = 0.5
                flow_ramp = nodes.new("ShaderNodeValToRGB")
                flow_ramp.color_ramp.elements[0].color = (0.005, 0.008, 0.02, 1.0)
                flow_ramp.color_ramp.elements[1].color = (0.95, 0.98, 1.0, 1.0)
                cyan = flow_ramp.color_ramp.elements.new(0.45)
                cyan.color = (0.0, 0.35, 0.7, 1.0)
                emission = nodes.new("ShaderNodeEmission")
                output_node = nodes.new("ShaderNodeOutputMaterial")
                flow_material.node_tree.links.new(
                    geometry.outputs["Normal"], normal_direction.inputs[0]
                )
                flow_material.node_tree.links.new(
                    normal_direction.outputs["Value"], frequency.inputs[0]
                )
                flow_material.node_tree.links.new(frequency.outputs[0], sine.inputs[0])
                flow_material.node_tree.links.new(sine.outputs[0], half.inputs[0])
                flow_material.node_tree.links.new(half.outputs[0], offset.inputs[0])
                flow_material.node_tree.links.new(offset.outputs[0], flow_ramp.inputs["Fac"])
                flow_material.node_tree.links.new(
                    flow_ramp.outputs["Color"], emission.inputs["Color"]
                )
                flow_material.node_tree.links.new(
                    emission.outputs[0], output_node.inputs["Surface"]
                )
                flow_original_materials = apply_temporary_mesh_material(
                    meshes, flow_material
                )
                scene.render.engine = "BLENDER_EEVEE_NEXT"
                scene.render.image_settings.file_format = "PNG"
                scene.render.image_settings.color_mode = "RGBA"
                flow_output = actual_output.with_name(
                    f"{actual_output.stem}.highlight-flow.png"
                )
                scene.render.filepath = str(flow_output)
                bpy.ops.render.render(write_still=True)
                passes["highlight_flow"] = str(flow_output.relative_to(root))
                restore_mesh_materials(flow_original_materials)
                bpy.data.materials.remove(flow_material)

            scene.display.shading.studiolight_rotate_z = original_rotation
            scene.display.shading.show_cavity = original_cavity
            scene.display.shading.show_shadows = original_shadows
        for obj in meshes:
            obj.color = original_colors[obj.name]

        surface_normal_outputs: dict[str, Path] = {}
        if requested_passes & {
            "world_normal",
            "geometric_normal",
            "normal_discontinuity",
        }:
            normal_view_transform = str(scene.view_settings.view_transform)
            normal_exposure = float(scene.view_settings.exposure)
            normal_gamma = float(scene.view_settings.gamma)
            scene.view_settings.view_transform = "Raw"
            scene.view_settings.exposure = 0.0
            scene.view_settings.gamma = 1.0
            for pass_name, socket_name in (
                ("world_normal", "Normal"),
                ("geometric_normal", "True Normal"),
            ):
                if (
                    pass_name not in requested_passes
                    and "normal_discontinuity" not in requested_passes
                ):
                    continue
                normal_material = bpy.data.materials.new(f"BVMCP_{pass_name}_material")
                normal_material.use_nodes = True
                nodes = normal_material.node_tree.nodes
                nodes.clear()
                geometry = nodes.new("ShaderNodeNewGeometry")
                multiply_add = nodes.new("ShaderNodeVectorMath")
                multiply_add.operation = "MULTIPLY_ADD"
                multiply_add.inputs[1].default_value = (0.5, 0.5, 0.5)
                multiply_add.inputs[2].default_value = (0.5, 0.5, 0.5)
                emission = nodes.new("ShaderNodeEmission")
                output_node = nodes.new("ShaderNodeOutputMaterial")
                normal_material.node_tree.links.new(
                    geometry.outputs[socket_name], multiply_add.inputs[0]
                )
                normal_material.node_tree.links.new(
                    multiply_add.outputs[0], emission.inputs["Color"]
                )
                normal_material.node_tree.links.new(
                    emission.outputs[0], output_node.inputs["Surface"]
                )
                normal_original_materials = apply_temporary_mesh_material(meshes, normal_material)
                scene.render.engine = "BLENDER_EEVEE_NEXT"
                scene.render.image_settings.file_format = "PNG"
                scene.render.image_settings.color_mode = "RGBA"
                normal_output = actual_output.with_name(
                    f"{actual_output.stem}.{pass_name.replace('_', '-')}.png"
                )
                scene.render.filepath = str(normal_output)
                bpy.ops.render.render(write_still=True)
                surface_normal_outputs[pass_name] = normal_output
                if pass_name in requested_passes:
                    passes[pass_name] = str(normal_output.relative_to(root))
                restore_mesh_materials(normal_original_materials)
                bpy.data.materials.remove(normal_material)
            scene.view_settings.view_transform = normal_view_transform
            scene.view_settings.exposure = normal_exposure
            scene.view_settings.gamma = normal_gamma

        if "normal_discontinuity" in requested_passes:
            discontinuity_output = actual_output.with_name(
                f"{actual_output.stem}.normal-discontinuity.png"
            )
            render_diagnostics["normal_discontinuity"] = (
                write_normal_discontinuity_map(
                    surface_normal_outputs["world_normal"],
                    surface_normal_outputs["geometric_normal"],
                    discontinuity_output,
                )
            )
            passes["normal_discontinuity"] = str(
                discontinuity_output.relative_to(root)
            )

        if requested_passes & {"depth", "normal"}:
            evidence_output = actual_output.with_name(f"{actual_output.stem}.depth-normal.exr")
            scene.render.engine = "BLENDER_EEVEE_NEXT"
            bpy.context.view_layer.material_override = None
            bpy.context.view_layer.update()
            bpy.context.view_layer.use_pass_z = True
            bpy.context.view_layer.use_pass_normal = True
            scene.render.image_settings.file_format = "OPEN_EXR_MULTILAYER"
            scene.render.image_settings.color_depth = "32"
            scene.render.filepath = str(evidence_output)
            bpy.ops.render.render(write_still=True)
            if "depth" in requested_passes:
                passes["depth"] = str(evidence_output.relative_to(root))
            if "normal" in requested_passes:
                passes["normal"] = str(evidence_output.relative_to(root))
    reported_intrinsics = (
        dict(camera_state["intrinsics"])
        if exact_camera
        else {
            "fx": width / (2.0 * math.tan(math.radians(horizontal_fov) / 2.0)),
            "fy": width / (2.0 * math.tan(math.radians(horizontal_fov) / 2.0)),
            "cx": width / 2.0,
            "cy": height / 2.0,
        }
    )
    return {
        "render_path": str(actual_output.relative_to(root)),
        "passes": passes,
        "object_ids": object_ids,
        "component_ids": component_ids,
        "feature_ids": feature_ids,
        "id_pass_policy": {
            "palette_encoding": "EXACT_8_BIT_RGBA",
            "identity_assignment": "CYCLES_INTEGER_OBJECT_INDEX",
            "requires_projected_bounds_crosscheck": True,
            "acceptance_policy": (
                "integer object-index identity is exact; governed projected-bounds "
                "cross-check remains mandatory defense in depth"
            ),
        },
        "render_diagnostics": render_diagnostics,
        "validation_policy": {
            "governed": governed,
            "geometry_separated_from_appearance": governed,
            "visual_geometry_families": {
                "form": [
                    "silhouette",
                    "neutral_clay",
                    "material_neutral",
                    "grazing_left",
                    "grazing_right",
                    "grazing_top",
                    "depth",
                    "world_normal",
                    "geometric_normal",
                    "curvature",
                    "wireframe",
                    "zebra",
                    "reflected_line",
                    "normal_discontinuity",
                    "highlight_flow",
                ],
                "semantics": ["object_id", "component_id", "feature_id"],
                "appearance": [
                    "beauty",
                    "appearance",
                    "exposure_minus_2",
                    "exposure_0",
                    "exposure_plus_2",
                ],
                "diagnostic_backgrounds": [
                    "neutral_grey_background",
                    "white_background",
                    "black_background",
                ],
            },
            "fixed_lighting_profile": lighting.get("profile"),
            "exposure_bracket_offsets": [-2.0, 0.0, 2.0] if governed else [],
        },
        "width": reported_crop["width"] if reported_crop else width,
        "height": reported_crop["height"] if reported_crop else height,
        "full_frame_width": width,
        "full_frame_height": height,
        "crop_roi_px": reported_crop,
        "camera": {
            "location": list(camera.location),
            "rotation_euler": list(camera.rotation_euler),
            "world_from_camera": [
                [float(camera.matrix_world[row][column]) for column in range(4)] for row in range(4)
            ],
            "intrinsics": reported_intrinsics,
            "horizontal_fov_degrees": horizontal_fov,
            "fit_margin": fit_margin,
            "lens_shift_x": lens_shift_x,
            "lens_shift_y": lens_shift_y,
            "camera_roll_degrees": camera_roll_degrees,
            "view_direction": list(direction),
            "lighting": lighting,
            "exposure": scene.view_settings.exposure,
            "render_mode": "silhouette_only" if silhouette_only else "beauty",
            "framing_authority": (
                "immutable_exact_camera_state" if exact_camera else "legacy_scene_bounds_auto_fit"
            ),
            "source_camera_sha256": (
                camera_state.get("source_camera_sha256") or camera_state.get("immutable_sha256")
                if exact_camera
                else None
            ),
            "dither_intensity": scene.render.dither_intensity,
        },
        "lighting": lighting,
        "bounds": {
            "minimum": list(minimum),
            "maximum": list(maximum),
            "dimensions": list(dimensions),
        },
    }


def evaluate_camera_candidates(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    """Render a bounded data-only camera sweep in one isolated Blender process."""
    candidates = parameters.get("candidates")
    if not isinstance(candidates, list) or not 1 <= len(candidates) <= 128:
        raise ValueError("camera evaluation requires between one and 128 candidates")
    output_directory = confined(root, str(parameters["output_directory"]))
    output_directory.mkdir(parents=True, exist_ok=True)
    width = max(64, min(int(parameters.get("width", 256)), 1024))
    height = max(64, min(int(parameters.get("height", 256)), 1024))
    allowed = {
        "view_direction",
        "horizontal_fov_degrees",
        "fit_margin",
        "lens_shift_x",
        "lens_shift_y",
        "camera_roll_degrees",
    }
    evaluations: list[dict[str, object]] = []
    for index, candidate in enumerate(candidates):
        if not isinstance(candidate, dict):
            raise ValueError("camera candidates must be JSON objects")
        unknown = set(candidate) - allowed
        if unknown:
            raise ValueError(f"unsupported camera candidate fields: {sorted(unknown)}")
        output = output_directory / f"candidate-{index:03d}.png"
        rendered = render_passes(
            root,
            {
                **candidate,
                "output_path": str(output),
                "width": width,
                "height": height,
                "review_lighting": True,
                "review_exposure": float(parameters.get("review_exposure", -0.5)),
                "silhouette_only": True,
                "evidence_passes": False,
            },
        )
        evaluations.append(
            {
                "index": index,
                "candidate": candidate,
                "render_path": rendered["render_path"],
                "camera": rendered["camera"],
            }
        )
    return {
        "evaluation_count": len(evaluations),
        "width": width,
        "height": height,
        "evaluations": evaluations,
    }


def generate_calibration_benchmark(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    """Create the owned Benchmark 0 scene and its exact six-view reference set."""
    output = confined(root, str(parameters["output_path"]))
    reference_dir = confined(root, str(parameters["reference_dir"]))
    output.parent.mkdir(parents=True, exist_ok=True)
    reference_dir.mkdir(parents=True, exist_ok=True)
    for item in list(bpy.data.objects):
        bpy.data.objects.remove(item, do_unlink=True)
    scene = bpy.context.scene
    scene.unit_settings.system = "METRIC"
    scene.unit_settings.scale_length = 0.001
    scene.unit_settings.length_unit = "MILLIMETERS"
    scene.render.film_transparent = True
    scene["bvmcp_benchmark_id"] = "synthetic-technical-calibration-v1"
    scene["bvmcp_ground_truth_dimensions_mm"] = [120.0, 80.0, 40.0]
    body_dimensions = (120.0, 80.0, 40.0)
    body_material = _material("Calibration_Anodized", (0.22, 0.24, 0.28, 1.0), 0.7, 0.28)
    dark_material = _material("Calibration_Feature", (0.025, 0.03, 0.04, 1.0), 0.2, 0.38)
    metal_material = _material("Calibration_Fastener", (0.5, 0.52, 0.55, 1.0), 0.85, 0.2)
    body = _local_cube(
        "Calibration_Body",
        None,
        (0.0, 0.0, 20.0),
        body_dimensions,
        body_material,
    )
    bpy.context.view_layer.objects.active = body
    body.select_set(True)
    bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
    bevel = body.modifiers.new("Calibration_RoundedCorners", "BEVEL")
    bevel.width = 4.0
    bevel.segments = 6
    bpy.context.view_layer.objects.active = body
    bpy.ops.object.modifier_apply(modifier=bevel.name)

    ports = []
    for index, (x_value, width, height) in enumerate(
        ((-34.0, 16.0, 7.0), (-10.0, 10.0, 5.0), (12.0, 14.0, 6.0))
    ):
        port = _local_cube(
            f"Calibration_Port_{index + 1:02d}",
            None,
            (x_value, -39.5, 16.0),
            (width, 1.0, height),
            dark_material,
        )
        bpy.context.view_layer.objects.active = port
        port.select_set(True)
        bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
        ports.append(port.name)
    recess = _local_cube(
        "Calibration_Recess",
        None,
        (35.0, -39.25, 20.0),
        (26.0, 1.5, 16.0),
        dark_material,
    )
    bpy.context.view_layer.objects.active = recess
    recess.select_set(True)
    bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)

    grille = []
    for row in range(5):
        for column in range(7):
            bpy.ops.mesh.primitive_cylinder_add(
                vertices=24,
                radius=1.6,
                depth=1.0,
                location=(-18.0 + column * 6.0, 39.5, 8.0 + row * 6.0),
                rotation=(math.pi / 2.0, 0.0, 0.0),
            )
            hole = bpy.context.active_object
            hole.name = f"Calibration_Hole_{row + 1:02d}_{column + 1:02d}"
            hole.data.materials.append(dark_material)
            grille.append(hole.name)

    screws = []
    for index, (x_value, z_value) in enumerate(
        ((-52.0, 7.0), (52.0, 7.0), (-52.0, 33.0), (52.0, 33.0))
    ):
        bpy.ops.mesh.primitive_cylinder_add(
            vertices=32,
            radius=2.0,
            depth=1.0,
            location=(x_value, 39.5, z_value),
            rotation=(math.pi / 2.0, 0.0, 0.0),
        )
        screw = bpy.context.active_object
        screw.name = f"Calibration_Screw_{index + 1:02d}"
        screw.data.materials.append(metal_material)
        screws.append(screw.name)

    fan_objects = _create_fan(
        "Calibration_Fan",
        {
            "radius_mm": 17.0,
            "depth_mm": 1.0,
            "blade_count": 9,
            "location_mm": [0.0, 0.0, 39.5],
        },
        scene.collection,
        1.0,
        dark_material,
    )
    for item in bpy.context.selected_objects:
        item.select_set(False)
    bpy.context.view_layer.update()
    bpy.ops.wm.save_as_mainfile(filepath=str(output), copy=True, compress=True)

    views = (
        ("front", [0.0, 1.0, 0.0]),
        ("rear", [0.0, -1.0, 0.0]),
        ("left", [1.0, 0.0, 0.0]),
        ("right", [-1.0, 0.0, 0.0]),
        ("top", [0.0, 0.0, -1.0]),
        ("bottom", [0.0, 0.0, 1.0]),
    )
    references = []
    for label, direction in views:
        render = render_passes(
            root,
            {
                "output_path": str(reference_dir / f"calibration-{label}.png"),
                "width": 320,
                "height": 320,
                "horizontal_fov_degrees": 50.0,
                "view_direction": direction,
                "review_lighting": True,
                "review_exposure": -0.5,
            },
        )
        references.append({"viewpoint_label": label, "view_direction": direction, **render})
    return {
        "scene_path": str(output.relative_to(root)),
        "scene_sha256": hashlib.sha256(output.read_bytes()).hexdigest(),
        "body_dimensions_mm": list(body_dimensions),
        "references": references,
        "features": {
            "rounded_body": body.name,
            "ports": ports,
            "fan": [item.name for item in fan_objects],
            "grille_holes": grille,
            "screws": screws,
            "recess": recess.name,
        },
        "ownership": "procedurally generated CC0-1.0 benchmark; generator is Apache-2.0",
    }


def generate_synthetic_dataset(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    """Render deterministic beauty, instance-label, depth, normal, and camera evidence."""
    from bpy_extras.object_utils import world_to_camera_view

    output_dir = confined(root, str(parameters["output_dir"]))
    output_dir.mkdir(parents=True, exist_ok=True)
    bpy.context.scene.render.dither_intensity = 0.0
    sample_count = int(parameters.get("sample_count", 1))
    if not 1 <= sample_count <= 64:
        raise ValueError("one local synthetic batch must contain between 1 and 64 samples")
    sample_start = int(parameters.get("sample_start", 0))
    if sample_start < 0:
        raise ValueError("synthetic sample_start cannot be negative")
    seed = int(parameters.get("seed", 0))
    width = max(64, min(int(parameters.get("width", 256)), 2048))
    height = max(64, min(int(parameters.get("height", 256)), 2048))
    randomization = parameters.get("domain_randomization", {})
    if not isinstance(randomization, dict):
        raise ValueError("domain_randomization must be a JSON object")

    def numeric_range(name: str, default: tuple[float, float]) -> tuple[float, float]:
        value = randomization.get(name, list(default))
        if (
            not isinstance(value, list)
            or len(value) != 2
            or not all(isinstance(item, (int, float)) for item in value)
        ):
            raise ValueError(f"domain_randomization.{name} must be a two-number range")
        low, high = float(value[0]), float(value[1])
        if low > high:
            raise ValueError(f"domain_randomization.{name} range is reversed")
        return low, high

    lighting_specification = randomization.get("lighting", {})
    if not isinstance(lighting_specification, dict):
        raise ValueError("domain_randomization.lighting must be a JSON object")
    energy_value = lighting_specification.get("energy_range", [0.3, 3.0])
    if (
        not isinstance(energy_value, list)
        or len(energy_value) != 2
        or not all(isinstance(item, (int, float)) for item in energy_value)
    ):
        raise ValueError("domain_randomization.lighting.energy_range must be a two-number range")
    energy_range = (float(energy_value[0]), float(energy_value[1]))
    if energy_range[0] <= 0.0 or energy_range[0] > energy_range[1]:
        raise ValueError("domain_randomization.lighting.energy_range must be positive and ordered")
    temperature_value = lighting_specification.get("temperature_kelvin", [3200.0, 7500.0])
    if (
        not isinstance(temperature_value, list)
        or len(temperature_value) != 2
        or not all(isinstance(item, (int, float)) for item in temperature_value)
    ):
        raise ValueError(
            "domain_randomization.lighting.temperature_kelvin must be a two-number range"
        )
    temperature_range = (float(temperature_value[0]), float(temperature_value[1]))
    if temperature_range[0] < 1000.0 or temperature_range[0] > temperature_range[1]:
        raise ValueError(
            "domain_randomization.lighting.temperature_kelvin must be ordered and at least 1000"
        )
    azimuth_range = numeric_range("camera_azimuth_degrees", (-180.0, 180.0))
    elevation_range = numeric_range("camera_elevation_degrees", (-35.0, 55.0))
    fov_range = numeric_range("camera_fov_degrees", (28.0, 72.0))
    exposure_range = numeric_range("exposure_ev", (-2.0, 2.0))
    roughness_range = numeric_range("roughness", (0.15, 0.85))
    surface_color_scale = numeric_range("surface_color_scale", (0.82, 1.18))
    blur_range = numeric_range("blur_sigma_px", (0.0, 1.5))
    noise_range = numeric_range("noise_sigma", (0.0, 0.035))
    occlusion_range = numeric_range("occlusion_fraction", (0.0, 0.25))
    manufacturing_range = numeric_range("manufacturing_variation_fraction", (-0.01, 0.01))
    if blur_range[0] < 0.0 or noise_range[0] < 0.0:
        raise ValueError("synthetic blur and noise ranges cannot be negative")
    if occlusion_range[0] < 0.0 or occlusion_range[1] > 0.95:
        raise ValueError("synthetic occlusion fraction must be between zero and 0.95")
    if manufacturing_range[0] <= -1.0:
        raise ValueError("manufacturing variation cannot collapse or invert geometry")
    background_value = randomization.get("background_hsv", [[0.0, 0.0, 0.03], [1.0, 0.35, 0.9]])
    if (
        not isinstance(background_value, list)
        or len(background_value) != 2
        or not all(
            isinstance(endpoint, list) and len(endpoint) == 3 for endpoint in background_value
        )
        or not all(
            isinstance(component, (int, float))
            for endpoint in background_value
            for component in endpoint
        )
    ):
        raise ValueError("domain_randomization.background_hsv must contain two HSV triples")
    background_low = [float(component) for component in background_value[0]]
    background_high = [float(component) for component in background_value[1]]
    if any(low > high for low, high in zip(background_low, background_high, strict=True)):
        raise ValueError("domain_randomization.background_hsv range is reversed")
    meshes = [
        obj for obj in bpy.context.scene.objects if obj.type == "MESH" and not obj.hide_render
    ]
    if not meshes:
        raise ValueError("synthetic generation requires at least one renderable mesh")
    minimum, maximum = scene_bounds()
    center = (minimum + maximum) * 0.5
    radius = max((maximum - minimum).length * 0.5, 0.01)
    scene = bpy.context.scene
    camera_data = bpy.data.cameras.new("BVMCP_SyntheticCamera")
    camera = bpy.data.objects.new("BVMCP_SyntheticCamera", camera_data)
    scene.collection.objects.link(camera)
    scene.camera = camera
    light_data = bpy.data.lights.new("BVMCP_SyntheticKey", "AREA")
    light = bpy.data.objects.new("BVMCP_SyntheticKey", light_data)
    scene.collection.objects.link(light)
    existing_lights = [obj for obj in scene.objects if obj.type == "LIGHT" and obj != light]
    original_light_visibility = {obj.name: obj.hide_render for obj in existing_lights}
    for obj in existing_lights:
        obj.hide_render = True
    scene.render.resolution_x = width
    scene.render.resolution_y = height
    scene.render.resolution_percentage = 100
    scene.render.film_transparent = False
    scene.render.image_settings.color_mode = "RGBA"
    if scene.world is None:
        scene.world = bpy.data.worlds.new("BVMCP_SyntheticWorld")
    scene.world.use_nodes = True
    background_node = scene.world.node_tree.nodes.get("Background")
    if background_node is None:
        raise ValueError("synthetic world has no Background node")
    original_colors = {obj.name: tuple(obj.color) for obj in meshes}
    original_scales = {obj.name: tuple(obj.scale) for obj in meshes}
    materials = sorted(
        {
            slot.material
            for obj in meshes
            for slot in obj.material_slots
            if slot.material is not None
        },
        key=lambda item: item.name,
    )
    original_materials = {
        material.name: {
            "roughness": float(material.roughness),
            "diffuse_color": tuple(material.diffuse_color),
            "principled": next(
                (node for node in material.node_tree.nodes if node.type == "BSDF_PRINCIPLED"),
                None,
            )
            if material.use_nodes and material.node_tree
            else None,
        }
        for material in materials
    }
    for material in materials:
        principled = original_materials[material.name]["principled"]
        original_materials[material.name]["node_roughness"] = (
            float(principled.inputs["Roughness"].default_value) if principled else None
        )
        original_materials[material.name]["node_base_color"] = (
            tuple(principled.inputs["Base Color"].default_value) if principled else None
        )
    object_ids = {}
    for index, obj in enumerate(meshes, start=1):
        red = ((index * 73) % 251 + 1) / 255.0
        green = ((index * 151) % 251 + 1) / 255.0
        blue = ((index * 199) % 251 + 1) / 255.0
        obj.color = (red, green, blue, 1.0)
        object_ids[obj.name] = {
            "rgb": [round(red * 255), round(green * 255), round(blue * 255)],
            "component_id": obj.get("bvmcp_component_id"),
            "component_type": obj.get("bvmcp_component_type"),
        }
    instance_colors = {obj.name: tuple(obj.color) for obj in meshes}
    base_component_types = {"", "basebody", "body", "panel", "shell", "enclosure"}
    feature_meshes = [
        obj
        for obj in meshes
        if obj.get("bvmcp_feature_id")
        or str(obj.get("bvmcp_component_type") or "").lower() not in base_component_types
    ]
    feature_ids = {}
    for index, obj in enumerate(sorted(feature_meshes, key=lambda item: item.name), start=1):
        rgb = (
            (index * 89) % 251 + 1,
            (index * 167) % 251 + 1,
            (index * 211) % 251 + 1,
        )
        feature_ids[obj.name] = {
            "rgb": list(rgb),
            "feature_id": obj.get("bvmcp_feature_id") or obj.get("bvmcp_component_id"),
            "feature_type": obj.get("bvmcp_component_type"),
        }
    bpy.ops.mesh.primitive_plane_add(size=2.0, location=center)
    occluder = bpy.context.object
    occluder.name = "BVMCP_SyntheticOccluder"
    occluder.hide_render = True
    occluder.color = (0.0, 0.0, 0.0, 1.0)
    occluder_material = bpy.data.materials.new("BVMCP_SyntheticOccluderMaterial")
    occluder_material.diffuse_color = (0.1, 0.1, 0.1, 1.0)
    occluder.data.materials.append(occluder_material)
    original_use_nodes = bool(scene.use_nodes)
    compositor_nodes = []
    compositor_texture = None
    samples = []
    files = []
    for local_index in range(sample_count):
        sample_index = sample_start + local_index
        generator = random.Random(seed + sample_index)
        manufacturing_variation = [generator.uniform(*manufacturing_range) for _axis in range(3)]
        for obj in meshes:
            obj.scale = tuple(
                original_scales[obj.name][axis] * (1.0 + manufacturing_variation[axis])
                for axis in range(3)
            )
        bpy.context.view_layer.update()
        azimuth = math.radians(generator.uniform(*azimuth_range))
        elevation = math.radians(generator.uniform(*elevation_range))
        fov = math.radians(generator.uniform(*fov_range))
        distance = radius / max(math.tan(fov / 2.0), 0.05) * generator.uniform(1.1, 1.5)
        direction = Vector(
            (
                math.cos(elevation) * math.cos(azimuth),
                math.cos(elevation) * math.sin(azimuth),
                math.sin(elevation),
            )
        )
        camera.location = center + direction * distance
        point_camera(camera, center)
        camera_data.type = "PERSP"
        camera_data.angle = fov
        camera_data.clip_start = max(distance / 1000.0, 0.0001)
        camera_data.clip_end = max(distance * 20.0, 1000.0)
        light.location = camera.location + Vector(
            (
                generator.uniform(-radius, radius),
                generator.uniform(-radius, radius),
                generator.uniform(0.0, radius * 1.5),
            )
        )
        point_camera(light, center)
        energy_multiplier = generator.uniform(*energy_range)
        # Blender's inverse-square lights operate in scene units.  Scaling energy by the
        # squared subject radius keeps millimetre-authored and metre-authored scenes visible.
        light_data.energy = max(250.0, radius * radius * energy_multiplier * 45.0)
        light_data.size = radius * generator.uniform(1.0, 4.0)
        light_temperature = generator.uniform(*temperature_range)
        light_data.color = kelvin_to_rgb(light_temperature)
        scene.view_settings.exposure = generator.uniform(*exposure_range)
        background_hsv = [
            generator.uniform(low, high)
            for low, high in zip(background_low, background_high, strict=True)
        ]
        background_rgb = hsv_to_rgb(*background_hsv)
        background_node.inputs["Color"].default_value = (*background_rgb, 1.0)
        # A modest environment fill preserves metallic/recess detail while the key light
        # remains the dominant source and continues to vary across samples.
        background_node.inputs["Strength"].default_value = generator.uniform(0.3, 0.8)
        material_samples = {}
        for material in materials:
            roughness = generator.uniform(*roughness_range)
            color_multiplier = generator.uniform(*surface_color_scale)
            original_color = original_materials[material.name]["diffuse_color"]
            randomized_color = tuple(
                min(1.0, max(0.0, float(component) * color_multiplier))
                for component in original_color[:3]
            ) + (float(original_color[3]),)
            material.roughness = roughness
            material.diffuse_color = randomized_color
            principled = original_materials[material.name]["principled"]
            if principled:
                principled.inputs["Roughness"].default_value = roughness
                principled.inputs["Base Color"].default_value = randomized_color
            material_samples[material.name] = {
                "roughness": roughness,
                "base_color": list(randomized_color),
            }
        occlusion_fraction = generator.uniform(*occlusion_range)
        occluder.hide_render = occlusion_fraction <= 0.0001
        if not occluder.hide_render:
            forward = (center - camera.location).normalized()
            right = forward.cross(Vector((0.0, 0.0, 1.0)))
            if right.length < 1e-6:
                right = Vector((1.0, 0.0, 0.0))
            else:
                right.normalize()
            up = right.cross(forward).normalized()
            side = radius * math.sqrt(occlusion_fraction) * 1.8
            occluder.location = (
                center
                - forward * radius * 0.7
                + right * generator.uniform(-radius * 0.55, radius * 0.55)
                + up * generator.uniform(-radius * 0.45, radius * 0.45)
            )
            occluder.scale = (side, side, 1.0)
            point_camera(occluder, center)
            occluder_color = tuple(generator.uniform(0.02, 0.95) for _channel in range(3))
            occluder_material.diffuse_color = (*occluder_color, 1.0)
        blur_sigma = generator.uniform(*blur_range)
        noise_sigma = generator.uniform(*noise_range)
        stem = f"sample-{sample_index:06d}"
        beauty = output_dir / f"{stem}-beauty.png"
        scene.render.engine = "BLENDER_EEVEE_NEXT"
        scene.render.film_transparent = False
        scene.render.image_settings.file_format = "PNG"
        scene.render.filepath = str(beauty)
        compositor_applied = False
        if not original_use_nodes and (blur_sigma > 0.0 or noise_sigma > 0.0):
            scene.use_nodes = True
            tree = scene.node_tree
            tree.nodes.clear()
            render_layers = tree.nodes.new("CompositorNodeRLayers")
            compositor_nodes.append(render_layers)
            output_socket = render_layers.outputs["Image"]
            if blur_sigma > 0.0:
                blur = tree.nodes.new("CompositorNodeBlur")
                blur.filter_type = "GAUSS"
                blur.size_x = max(1, round(blur_sigma * 2.0))
                blur.size_y = max(1, round(blur_sigma * 2.0))
                tree.links.new(output_socket, blur.inputs["Image"])
                output_socket = blur.outputs["Image"]
                compositor_nodes.append(blur)
            if noise_sigma > 0.0:
                compositor_texture = bpy.data.textures.new(
                    f"BVMCP_SyntheticNoise_{sample_index}", type="CLOUDS"
                )
                compositor_texture.noise_scale = generator.uniform(0.03, 0.2)
                texture = tree.nodes.new("CompositorNodeTexture")
                texture.texture = compositor_texture
                subtract = tree.nodes.new("CompositorNodeMath")
                subtract.operation = "SUBTRACT"
                subtract.inputs[1].default_value = 0.5
                scale_noise = tree.nodes.new("CompositorNodeMath")
                scale_noise.operation = "MULTIPLY"
                scale_noise.inputs[1].default_value = noise_sigma * 2.0
                mix = tree.nodes.new("CompositorNodeMixRGB")
                mix.blend_type = "ADD"
                mix.inputs[0].default_value = 1.0
                tree.links.new(texture.outputs["Value"], subtract.inputs[0])
                tree.links.new(subtract.outputs[0], scale_noise.inputs[0])
                tree.links.new(output_socket, mix.inputs[1])
                tree.links.new(scale_noise.outputs[0], mix.inputs[2])
                output_socket = mix.outputs[0]
                compositor_nodes.extend((texture, subtract, scale_noise, mix))
            composite = tree.nodes.new("CompositorNodeComposite")
            tree.links.new(output_socket, composite.inputs["Image"])
            compositor_nodes.append(composite)
            compositor_applied = True
        bpy.ops.render.render(write_still=True)
        if compositor_applied:
            scene.use_nodes = False
        mask = output_dir / f"{stem}-instance-mask.png"
        scene.render.engine = "BLENDER_WORKBENCH"
        scene.render.film_transparent = True
        scene.display.shading.light = "FLAT"
        scene.display.shading.color_type = "OBJECT"
        scene.display.shading.show_shadows = False
        scene.display.shading.show_cavity = False
        scene.display.shading.show_specular_highlight = False
        scene.render.image_settings.file_format = "PNG"
        scene.render.filepath = str(mask)
        occluder.color = (0.0, 0.0, 0.0, 1.0)
        bpy.ops.render.render(write_still=True)
        feature_mask = output_dir / f"{stem}-feature-mask.png"
        for obj in meshes:
            obj.color = (0.0, 0.0, 0.0, 1.0)
        for obj in feature_meshes:
            rgb = feature_ids[obj.name]["rgb"]
            obj.color = tuple(channel / 255.0 for channel in rgb) + (1.0,)
        scene.render.filepath = str(feature_mask)
        bpy.ops.render.render(write_still=True)
        for obj in meshes:
            obj.color = instance_colors[obj.name]
        evidence = output_dir / f"{stem}-depth-normal.exr"
        scene.render.engine = "BLENDER_EEVEE_NEXT"
        scene.render.film_transparent = False
        scene.view_layers[0].use_pass_z = True
        scene.view_layers[0].use_pass_normal = True
        scene.render.image_settings.file_format = "OPEN_EXR_MULTILAYER"
        scene.render.image_settings.color_depth = "32"
        scene.render.filepath = str(evidence)
        bpy.ops.render.render(write_still=True)
        keypoints = []
        for obj in meshes:
            projected = [
                world_to_camera_view(scene, camera, obj.matrix_world @ Vector(corner))
                for corner in obj.bound_box
            ]
            visible = [point for point in projected if point.z > 0]
            if not visible:
                continue
            xs = [point.x * width for point in visible]
            ys = [(1.0 - point.y) * height for point in visible]
            unclipped_area = max(0.0, max(xs) - min(xs)) * max(0.0, max(ys) - min(ys))
            clipped_area = max(0.0, min(width, max(xs)) - max(0.0, min(xs))) * max(
                0.0, min(height, max(ys)) - max(0.0, min(ys))
            )
            keypoints.append(
                {
                    "object": obj.name,
                    "cross_view_identity": obj.get("bvmcp_feature_id")
                    or obj.get("bvmcp_component_id")
                    or obj.name,
                    "bounding_box_xyxy": [min(xs), min(ys), max(xs), max(ys)],
                    "center_xy": [sum(xs) / len(xs), sum(ys) / len(ys)],
                    "pose_world": [list(row) for row in obj.matrix_world],
                    "orientation_quaternion_wxyz": list(obj.matrix_world.to_quaternion()),
                    "visible_fraction": (
                        min(1.0, clipped_area / unclipped_area) if unclipped_area > 0.0 else 0.0
                    ),
                    "occlusion_fraction": occlusion_fraction,
                    "dimensions_scene_units": list(obj.dimensions),
                    "dimensions_mm": [
                        float(value) * scene.unit_settings.scale_length * 1000.0
                        for value in obj.dimensions
                    ],
                }
            )
        metadata = output_dir / f"{stem}-metadata.json"
        metadata_value = {
            "sample_index": sample_index,
            "seed": seed + sample_index,
            "beauty": beauty.name,
            "instance_mask": mask.name,
            "feature_mask": feature_mask.name,
            "depth_normal": evidence.name,
            "camera": {
                "world_from_camera": [list(row) for row in camera.matrix_world],
                "projection_matrix": [
                    list(row)
                    for row in camera.calc_matrix_camera(
                        bpy.context.evaluated_depsgraph_get(), x=width, y=height
                    )
                ],
                "horizontal_fov_radians": fov,
            },
            "lighting": {
                "location": list(light.location),
                "energy": light_data.energy,
                "energy_multiplier": energy_multiplier,
                "temperature_kelvin": light_temperature,
                "color_rgb": list(light_data.color),
                "size": light_data.size,
                "exposure": scene.view_settings.exposure,
                "background_hsv": background_hsv,
                "background_rgb": list(background_rgb),
                "environment_strength": background_node.inputs["Strength"].default_value,
            },
            "materials": material_samples,
            "object_ids": object_ids,
            "feature_ids": feature_ids,
            "keypoints": keypoints,
            "occlusion": {
                "requested_fraction": occlusion_fraction,
                "enabled": not occluder.hide_render,
                "object": occluder.name,
                "location": list(occluder.location),
                "scale": list(occluder.scale),
            },
            "manufacturing_variation_fraction_xyz": manufacturing_variation,
            "image_degradation": {
                "blur_sigma_px": blur_sigma,
                "noise_sigma": noise_sigma,
                "applied_in_compositor": compositor_applied,
                "skipped_for_existing_compositor": bool(
                    original_use_nodes and (blur_sigma > 0.0 or noise_sigma > 0.0)
                ),
            },
            "domain_randomization": randomization,
        }
        metadata.write_text(json.dumps(metadata_value, indent=2, sort_keys=True), encoding="utf-8")
        sample_files = [beauty, mask, feature_mask, evidence, metadata]
        files.extend(sample_files)
        samples.append(
            {
                "index": sample_index,
                "files": [path.name for path in sample_files],
                "camera_location": list(camera.location),
                "fov_degrees": math.degrees(fov),
            }
        )
    for obj in meshes:
        obj.color = original_colors[obj.name]
        obj.scale = original_scales[obj.name]
    for material in materials:
        original = original_materials[material.name]
        material.roughness = original["roughness"]
        material.diffuse_color = original["diffuse_color"]
        principled = original["principled"]
        if principled:
            principled.inputs["Roughness"].default_value = original["node_roughness"]
            principled.inputs["Base Color"].default_value = original["node_base_color"]
    for obj in existing_lights:
        obj.hide_render = original_light_visibility[obj.name]
    index_path = output_dir / f"dataset-index-{sample_start:06d}.json"
    index_value = {
        "schema_version": 1,
        "sample_count": sample_count,
        "sample_start": sample_start,
        "seed": seed,
        "object_ids": object_ids,
        "feature_ids": feature_ids,
        "samples": samples,
    }
    index_path.write_text(json.dumps(index_value, indent=2, sort_keys=True), encoding="utf-8")
    files.append(index_path)
    return {
        "output_directory": str(output_dir.relative_to(root)),
        "sample_count": sample_count,
        "index_path": str(index_path.relative_to(root)),
        "files": [
            {
                "path": str(path.relative_to(root)),
                "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                "size": path.stat().st_size,
            }
            for path in files
        ],
        "outputs": [
            "beauty",
            "instance_mask",
            "feature_mask",
            "depth",
            "normals",
            "keypoints",
            "dimensions_mm",
            "feature_ids",
            "materials",
            "lighting",
            "occlusion",
            "pose",
            "orientation",
            "visible_fraction",
            "cross_view_identity",
        ],
        "network_used": False,
    }


def export_glb(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    output = confined(root, str(parameters["output_path"]))
    output.parent.mkdir(parents=True, exist_ok=True)
    bpy.ops.export_scene.gltf(
        filepath=str(output),
        export_format="GLB",
        export_apply=True,
        use_renderable=True,
        export_yup=True,
        export_cameras=False,
        export_lights=False,
    )
    return {"export_path": str(output.relative_to(root)), "size": output.stat().st_size}


def _material(
    name: str,
    color: tuple[float, float, float, float],
    metallic: float,
    roughness: float,
    *,
    specular_ior_level: float | None = None,
):
    material = bpy.data.materials.get(name) or bpy.data.materials.new(name)
    material.diffuse_color = color
    material.use_nodes = True
    node = material.node_tree.nodes.get("Principled BSDF") if material.node_tree else None
    if node:
        node.inputs["Base Color"].default_value = color
        node.inputs["Metallic"].default_value = metallic
        node.inputs["Roughness"].default_value = roughness
        if specular_ior_level is not None and "Specular IOR Level" in node.inputs:
            node.inputs["Specular IOR Level"].default_value = specular_ior_level
    return material


def _rounded_rectangle_distance(
    x: float, z: float, half_width: float, half_height: float, radius: float
) -> float:
    dx = max(abs(x) - (half_width - radius), 0.0)
    dz = max(abs(z) - (half_height - radius), 0.0)
    return max(0.0, math.sqrt(dx * dx + dz * dz) - radius)


def _grille_centers(
    width: float,
    height: float,
    pitch: float,
    diameter: float,
    target_count: int,
    corner_radius: float,
) -> list[tuple[float, float]]:
    row_step = pitch * math.sqrt(3.0) / 2.0
    row_count = math.floor((height - diameter) / row_step) + 1
    column_count = math.floor((width - diameter) / pitch) + 1
    candidates = []
    for row in range(row_count):
        z = (row - (row_count - 1) / 2.0) * row_step
        shift = pitch / 2.0 if row % 2 else 0.0
        for column in range(-column_count, column_count + 1):
            x = column * pitch + shift
            if abs(x) > (width - diameter) / 2.0:
                continue
            distance = _rounded_rectangle_distance(
                x,
                z,
                width / 2.0 - diameter / 2.0,
                height / 2.0 - diameter / 2.0,
                max(0.0, corner_radius - diameter / 2.0),
            )
            candidates.append((distance, abs(x) + abs(z), x, z))
    candidates.sort()
    if target_count <= 0 or target_count > len(candidates):
        raise ValueError(
            f"target hole count {target_count} is outside generated capacity {len(candidates)}"
        )
    selected = candidates[:target_count]
    return [(item[2], item[3]) for item in selected]


def _create_annular_hex_panel(
    name: str,
    centers: list[tuple[float, float]],
    *,
    pitch: float,
    diameter: float,
    thickness: float,
    y_center: float,
    z_center: float,
    parent,
):
    """Build one welded, closed manifold whose genus equals its aperture count."""
    segments = 12
    cell_radius = pitch / math.sqrt(3.0)
    hole_radius = diameter / 2.0
    outer_profile = [
        (
            cell_radius * math.cos(math.radians(30.0 + index * 60.0)),
            cell_radius * math.sin(math.radians(30.0 + index * 60.0)),
        )
        for index in range(6)
    ]
    inner_profile = [
        (
            hole_radius * math.cos(math.radians(30.0 + index * 30.0)),
            hole_radius * math.sin(math.radians(30.0 + index * 30.0)),
        )
        for index in range(segments)
    ]
    vertices: list[tuple[float, float, float]] = []
    faces: list[tuple[int, ...]] = []
    y_front = y_center + thickness / 2.0
    y_back = y_center - thickness / 2.0
    shared_outer: dict[tuple[float, float, float], int] = {}
    outer_edges: dict[
        tuple[tuple[float, float], tuple[float, float]],
        list[tuple[int, int, int, int]],
    ] = {}

    def add_vertex(x: float, y: float, z: float, *, shared: bool = False) -> int:
        key = (round(x, 12), round(y, 12), round(z, 12))
        if shared and key in shared_outer:
            return shared_outer[key]
        index = len(vertices)
        vertices.append((x, y, z))
        if shared:
            shared_outer[key] = index
        return index

    for center_x, center_local_z in centers:
        outer_coordinates = [
            (center_x + x, z_center + center_local_z + z) for x, z in outer_profile
        ]
        inner_coordinates = [
            (center_x + x, z_center + center_local_z + z) for x, z in inner_profile
        ]
        outer_front = [add_vertex(x, y_front, z, shared=True) for x, z in outer_coordinates]
        outer_back = [add_vertex(x, y_back, z, shared=True) for x, z in outer_coordinates]
        inner_front = [add_vertex(x, y_front, z) for x, z in inner_coordinates]
        inner_back = [add_vertex(x, y_back, z) for x, z in inner_coordinates]
        for sector in range(6):
            following = (sector + 1) % 6
            inner_start = sector * 2
            inner_middle = inner_start + 1
            inner_end = (inner_start + 2) % segments
            faces.extend(
                [
                    (
                        outer_front[sector],
                        outer_front[following],
                        inner_front[inner_end],
                        inner_front[inner_middle],
                    ),
                    (
                        outer_front[sector],
                        inner_front[inner_middle],
                        inner_front[inner_start],
                    ),
                    (
                        outer_back[following],
                        outer_back[sector],
                        inner_back[inner_middle],
                        inner_back[inner_end],
                    ),
                    (
                        outer_back[sector],
                        inner_back[inner_start],
                        inner_back[inner_middle],
                    ),
                ]
            )
            edge_key = tuple(
                sorted(
                    (round(point[0], 12), round(point[1], 12))
                    for point in (
                        outer_coordinates[sector],
                        outer_coordinates[following],
                    )
                )
            )
            outer_edges.setdefault(edge_key, []).append(
                (
                    outer_front[sector],
                    outer_front[following],
                    outer_back[sector],
                    outer_back[following],
                )
            )
        for index in range(segments):
            following = (index + 1) % segments
            faces.append(
                (
                    inner_front[index],
                    inner_front[following],
                    inner_back[following],
                    inner_back[index],
                )
            )
    for occurrences in outer_edges.values():
        if len(occurrences) == 1:
            front_start, front_end, back_start, back_end = occurrences[0]
            faces.append((front_end, front_start, back_start, back_end))
        elif len(occurrences) != 2:
            raise ValueError("hex-panel construction produced an invalid shared cell edge")
    mesh = bpy.data.meshes.new(f"{name}-mesh")
    face_counts: dict[tuple[int, ...], int] = {}
    for face in faces:
        key = tuple(sorted(face))
        face_counts[key] = face_counts.get(key, 0) + 1
    duplicate_faces = sum(count - 1 for count in face_counts.values())
    if duplicate_faces:
        examples = [key for key, count in face_counts.items() if count > 1][:3]
        raise ValueError(
            f"hex-panel construction produced {duplicate_faces} duplicate faces; "
            f"examples={examples}; vertices={len(vertices)} faces={len(faces)}"
        )
    mesh.from_pydata(vertices, [], faces)
    mesh.update(calc_edges=True)
    panel = bpy.data.objects.new(name, mesh)
    bpy.context.scene.collection.objects.link(panel)
    panel.parent = parent
    panel.matrix_parent_inverse = Matrix.Identity(4)
    panel["bvmcp_construction_method"] = "welded_hex_voronoi_cells"
    panel["bvmcp_cell_count"] = len(centers)
    panel["bvmcp_shared_cell_edges"] = sum(
        len(occurrences) == 2 for occurrences in outer_edges.values()
    )
    panel["bvmcp_boundary_cell_edges"] = sum(
        len(occurrences) == 1 for occurrences in outer_edges.values()
    )
    panel["bvmcp_duplicate_faces"] = duplicate_faces
    return panel


def _local_cube(name: str, parent, location, dimensions, material=None):
    bpy.ops.mesh.primitive_cube_add(size=2.0)
    cube = bpy.context.active_object
    cube.name = name
    cube.parent = parent
    cube.matrix_parent_inverse = Matrix.Identity(4)
    cube.location = location
    cube.scale = tuple(value / 2.0 for value in dimensions)
    bpy.context.view_layer.update()
    if material:
        cube.data.materials.append(material)
    return cube


COMPONENT_TYPES = {
    "Body",
    "Panel",
    "Shell",
    "Cutout",
    "Port",
    "Button",
    "Screw",
    "Foot",
    "Fan",
    "BladeArray",
    "VentArray",
    "HoleArray",
    "Grille",
    "Bracket",
    "HeatSink",
    "Logo",
    "PCB",
    "SplineBodySection",
    "LoftedSurface",
    "WheelArch",
    "PanelCut",
    "PanelGap",
    "SurfaceCrease",
    "Aerofoil",
    "Duct",
    "Vent",
    "LightHousing",
    "GlassPanel",
    "TireProfile",
    "WheelSpokeArray",
    "BrakeAssembly",
    "DiffuserChannel",
    "UnderbodyPanel",
    "Bezier",
    "NURBS",
    "CurveNetwork",
    "Sweep",
    "PatchSurface",
    "ControlledShrinkwrap",
    "RetopologyCage",
}


def _component_number(
    parameters: dict[str, object], name: str, default: float, *, minimum: float = 0.0
) -> float:
    value = float(parameters.get(name, default))
    if not math.isfinite(value) or value < minimum:
        raise ValueError(f"component parameter {name} must be finite and >= {minimum}")
    return value


def _move_to_collection(obj, collection) -> None:
    for current in list(obj.users_collection):
        current.objects.unlink(obj)
    collection.objects.link(obj)


def _component_location(
    parameters: dict[str, object], to_local: float
) -> tuple[float, float, float]:
    raw = parameters.get("location_mm", [0.0, 0.0, 0.0])
    if not isinstance(raw, list) or len(raw) != 3:
        raise ValueError("component location_mm must be a three-value list")
    result = tuple(float(value) * to_local for value in raw)
    if not all(math.isfinite(value) for value in result):
        raise ValueError("component location contains a non-finite value")
    return result


def _component_dimensions(
    parameters: dict[str, object], to_local: float
) -> tuple[float, float, float]:
    if isinstance(parameters.get("dimensions_mm"), list):
        raw = parameters["dimensions_mm"]
        if len(raw) != 3:
            raise ValueError("component dimensions_mm must contain three values")
        dimensions = tuple(float(value) * to_local for value in raw)
    else:
        dimensions = (
            _component_number(parameters, "width_mm", 10.0, minimum=0.001) * to_local,
            _component_number(parameters, "depth_mm", 10.0, minimum=0.001) * to_local,
            _component_number(parameters, "height_mm", 10.0, minimum=0.001) * to_local,
        )
    if not all(math.isfinite(value) and value > 0 for value in dimensions):
        raise ValueError("component dimensions must be finite and positive")
    return dimensions


def _bevel_object(obj, width: float) -> None:
    if width <= 0:
        return
    modifier = obj.modifiers.new("BVMCP_Bevel", "BEVEL")
    modifier.width = width
    modifier.segments = 3


def _create_cube_component(name, parameters, collection, to_local, material):
    cube = _local_cube(
        name,
        None,
        _component_location(parameters, to_local),
        _component_dimensions(parameters, to_local),
        material,
    )
    _move_to_collection(cube, collection)
    _bevel_object(
        cube,
        _component_number(parameters, "bevel_mm", 0.0, minimum=0.0) * to_local,
    )
    return [cube]


def _create_cylinder_component(name, parameters, collection, to_local, material):
    radius = _component_number(
        parameters,
        "radius_mm",
        _component_number(parameters, "diameter_mm", 10.0, minimum=0.001) / 2.0,
        minimum=0.001,
    )
    depth = _component_number(parameters, "depth_mm", 4.0, minimum=0.001)
    vertices = max(8, min(int(parameters.get("segments", 32)), 128))
    bpy.ops.mesh.primitive_cylinder_add(
        vertices=vertices,
        radius=radius * to_local,
        depth=depth * to_local,
        location=_component_location(parameters, to_local),
    )
    cylinder = bpy.context.active_object
    cylinder.name = name
    _move_to_collection(cylinder, collection)
    if material:
        cylinder.data.materials.append(material)
    axis = str(parameters.get("axis", "z")).lower()
    if axis == "x":
        cylinder.rotation_euler[1] = math.pi / 2.0
    elif axis == "y":
        cylinder.rotation_euler[0] = math.pi / 2.0
    elif axis != "z":
        raise ValueError("cylinder axis must be x, y, or z")
    return [cylinder]


def _create_geometry_nodes_array(name, parameters, collection, to_local, material):
    count_x = max(1, min(int(parameters.get("count_x", parameters.get("columns", 4))), 256))
    count_y = max(1, min(int(parameters.get("count_y", parameters.get("rows", 4))), 256))
    if count_x * count_y > 10000:
        raise ValueError("component array exceeds the 10,000-instance safety limit")
    pitch_x = _component_number(parameters, "pitch_x_mm", 3.0, minimum=0.001) * to_local
    pitch_y = (
        _component_number(
            parameters,
            "pitch_y_mm",
            float(parameters.get("pitch_x_mm", 3.0)),
            minimum=0.001,
        )
        * to_local
    )
    diameter = _component_number(parameters, "diameter_mm", 1.0, minimum=0.001) * to_local
    depth = _component_number(parameters, "depth_mm", 1.0, minimum=0.001) * to_local
    mesh = bpy.data.meshes.new(f"{name}-source")
    obj = bpy.data.objects.new(name, mesh)
    collection.objects.link(obj)
    obj.location = _component_location(parameters, to_local)
    node_group = bpy.data.node_groups.new(f"BVMCP_{name}_Array", "GeometryNodeTree")
    node_group.interface.new_socket(
        name="Geometry", in_out="OUTPUT", socket_type="NodeSocketGeometry"
    )
    output = node_group.nodes.new("NodeGroupOutput")
    grid = node_group.nodes.new("GeometryNodeMeshGrid")
    grid.inputs["Size X"].default_value = pitch_x * max(0, count_x - 1)
    grid.inputs["Size Y"].default_value = pitch_y * max(0, count_y - 1)
    grid.inputs["Vertices X"].default_value = count_x
    grid.inputs["Vertices Y"].default_value = count_y
    points = node_group.nodes.new("GeometryNodeMeshToPoints")
    points.mode = "VERTICES"
    cylinder = node_group.nodes.new("GeometryNodeMeshCylinder")
    cylinder.inputs["Vertices"].default_value = max(8, min(int(parameters.get("segments", 16)), 64))
    cylinder.inputs["Radius"].default_value = diameter / 2.0
    cylinder.inputs["Depth"].default_value = depth
    instances = node_group.nodes.new("GeometryNodeInstanceOnPoints")
    realize = node_group.nodes.new("GeometryNodeRealizeInstances")
    node_group.links.new(grid.outputs["Mesh"], points.inputs["Mesh"])
    node_group.links.new(points.outputs["Points"], instances.inputs["Points"])
    node_group.links.new(cylinder.outputs["Mesh"], instances.inputs["Instance"])
    node_group.links.new(instances.outputs["Instances"], realize.inputs["Geometry"])
    node_group.links.new(realize.outputs["Geometry"], output.inputs["Geometry"])
    modifier = obj.modifiers.new("BVMCP_GeometryNodes", "NODES")
    modifier.node_group = node_group
    if material:
        obj.data.materials.append(material)
    obj["bvmcp_array_count"] = count_x * count_y
    obj["bvmcp_pitch_x_mm"] = pitch_x / to_local
    obj["bvmcp_pitch_y_mm"] = pitch_y / to_local
    obj["bvmcp_generator"] = "functional_geometry_nodes_array"
    return [obj]


def _create_fan(name, parameters, collection, to_local, material):
    location = Vector(_component_location(parameters, to_local))
    radius = _component_number(parameters, "radius_mm", 20.0, minimum=0.001) * to_local
    depth = _component_number(parameters, "depth_mm", 4.0, minimum=0.001) * to_local
    blade_count = max(3, min(int(parameters.get("blade_count", 7)), 64))
    objects = _create_cylinder_component(
        f"{name}_hub",
        {
            "radius_mm": radius / to_local * 0.2,
            "depth_mm": depth / to_local,
            "location_mm": [value / to_local for value in location],
        },
        collection,
        to_local,
        material,
    )
    for index in range(blade_count):
        angle = 2.0 * math.pi * index / blade_count
        blade = _local_cube(
            f"{name}_blade_{index + 1:02d}",
            None,
            location
            + Vector((math.cos(angle) * radius * 0.55, math.sin(angle) * radius * 0.55, 0.0)),
            (radius * 0.65, radius * 0.16, depth * 0.55),
            material,
        )
        blade.rotation_euler[2] = angle + 0.35
        _move_to_collection(blade, collection)
        objects.append(blade)
    return objects


def _component_point_list(parameters, key, to_local, default):
    raw = parameters.get(key, default)
    if not isinstance(raw, list) or len(raw) < 2:
        raise ValueError(f"component parameter {key} must contain at least two points")
    points = []
    for index, point in enumerate(raw):
        if not isinstance(point, list) or len(point) != 3:
            raise ValueError(f"component parameter {key}[{index}] must contain x, y, z")
        value = tuple(float(item) * to_local for item in point)
        if not all(math.isfinite(item) for item in value):
            raise ValueError(f"component parameter {key}[{index}] must be finite")
        points.append(value)
    return points


def _create_curve_component(
    name, parameters, collection, to_local, material, *, spline_kind="POLY"
):
    dimensions = _component_dimensions(parameters, to_local)
    default = [
        [-dimensions[0] / to_local / 2.0, 0.0, 0.0],
        [0.0, dimensions[1] / to_local / 4.0, dimensions[2] / to_local / 4.0],
        [dimensions[0] / to_local / 2.0, 0.0, 0.0],
    ]
    points = _component_point_list(parameters, "control_points_mm", to_local, default)
    curve_data = bpy.data.curves.new(f"{name}-curve", "CURVE")
    curve_data.dimensions = "3D"
    curve_data.resolution_u = max(1, min(int(parameters.get("resolution", 12)), 64))
    curve_data.bevel_depth = (
        _component_number(parameters, "profile_radius_mm", 0.5, minimum=0.0) * to_local
    )
    curve_data.bevel_resolution = 3
    if spline_kind == "BEZIER":
        spline = curve_data.splines.new("BEZIER")
        spline.bezier_points.add(len(points) - 1)
        for point, coordinate in zip(spline.bezier_points, points, strict=True):
            point.co = coordinate
            point.handle_left_type = "AUTO"
            point.handle_right_type = "AUTO"
    else:
        spline = curve_data.splines.new(spline_kind)
        spline.points.add(len(points) - 1)
        for point, coordinate in zip(spline.points, points, strict=True):
            point.co = (*coordinate, 1.0)
        if spline_kind == "NURBS":
            spline.order_u = min(4, len(points))
            spline.use_endpoint_u = True
    curve = bpy.data.objects.new(name, curve_data)
    collection.objects.link(curve)
    curve.location = _component_location(parameters, to_local)
    if material:
        curve.data.materials.append(material)
    curve["bvmcp_surface_method"] = spline_kind.lower()
    return [curve]


def _create_loft_surface(name, parameters, collection, to_local, material):
    raw_sections = parameters.get("sections_mm")
    if raw_sections is None:
        width, depth, height = _component_dimensions(parameters, to_local)
        raw_sections = []
        for x, factor in ((-0.5, 0.82), (0.0, 1.0), (0.5, 0.82)):
            raw_sections.append(
                [
                    [x * width / to_local, -depth * factor / to_local / 2.0, 0.0],
                    [x * width / to_local, 0.0, height * factor / to_local / 2.0],
                    [x * width / to_local, depth * factor / to_local / 2.0, 0.0],
                    [x * width / to_local, 0.0, -height * factor / to_local / 2.0],
                ]
            )
    if not isinstance(raw_sections, list) or len(raw_sections) < 2:
        raise ValueError("lofted surface requires at least two sections_mm")
    sections = [
        _component_point_list({"points": section}, "points", to_local, [])
        for section in raw_sections
    ]
    section_size = len(sections[0])
    if section_size < 2 or any(len(section) != section_size for section in sections):
        raise ValueError("lofted surface sections must have equal point counts")
    vertices = [point for section in sections for point in section]
    faces = []
    closed = bool(parameters.get("closed_sections", True))
    segment_count = section_size if closed else section_size - 1
    for section_index in range(len(sections) - 1):
        first = section_index * section_size
        second = (section_index + 1) * section_size
        for point_index in range(segment_count):
            following = (point_index + 1) % section_size
            faces.append(
                (
                    first + point_index,
                    first + following,
                    second + following,
                    second + point_index,
                )
            )
    mesh = bpy.data.meshes.new(f"{name}-loft")
    mesh.from_pydata(vertices, [], faces)
    mesh.update()
    obj = bpy.data.objects.new(name, mesh)
    collection.objects.link(obj)
    obj.location = _component_location(parameters, to_local)
    if material:
        obj.data.materials.append(material)
    if bool(parameters.get("subdivision", True)):
        modifier = obj.modifiers.new("BVMCP_Subdivision", "SUBSURF")
        modifier.levels = max(1, min(int(parameters.get("subdivision_levels", 2)), 4))
        modifier.render_levels = modifier.levels
    obj["bvmcp_surface_method"] = "semantic_loft"
    obj["bvmcp_section_count"] = len(sections)
    return [obj]


def _create_patch_surface(name, parameters, collection, to_local, material):
    raw_grid = parameters.get("control_grid_mm")
    if raw_grid is None:
        width, depth, _height = _component_dimensions(parameters, to_local)
        raw_grid = [
            [
                [-width / to_local / 2.0, -depth / to_local / 2.0, 0.0],
                [width / to_local / 2.0, -depth / to_local / 2.0, 0.0],
            ],
            [
                [-width / to_local / 2.0, depth / to_local / 2.0, 0.0],
                [width / to_local / 2.0, depth / to_local / 2.0, 0.0],
            ],
        ]
    if not isinstance(raw_grid, list) or len(raw_grid) < 2:
        raise ValueError("patch surface requires a control_grid_mm")
    rows = [_component_point_list({"points": row}, "points", to_local, []) for row in raw_grid]
    columns = len(rows[0])
    if columns < 2 or any(len(row) != columns for row in rows):
        raise ValueError("patch surface control grid must be rectangular")
    vertices = [point for row in rows for point in row]
    faces = [
        (
            row * columns + column,
            row * columns + column + 1,
            (row + 1) * columns + column + 1,
            (row + 1) * columns + column,
        )
        for row in range(len(rows) - 1)
        for column in range(columns - 1)
    ]
    mesh = bpy.data.meshes.new(f"{name}-patch")
    mesh.from_pydata(vertices, [], faces)
    mesh.update()
    obj = bpy.data.objects.new(name, mesh)
    collection.objects.link(obj)
    obj.location = _component_location(parameters, to_local)
    if material:
        obj.data.materials.append(material)
    modifier = obj.modifiers.new("BVMCP_Subdivision", "SUBSURF")
    modifier.levels = max(1, min(int(parameters.get("subdivision_levels", 2)), 4))
    modifier.render_levels = modifier.levels
    obj["bvmcp_surface_method"] = "controlled_patch"
    return [obj]


def _create_tire_profile(name, parameters, collection, to_local, material):
    major_radius = _component_number(parameters, "radius_mm", 180.0, minimum=0.001)
    minor_radius = _component_number(parameters, "section_radius_mm", 35.0, minimum=0.001)
    bpy.ops.mesh.primitive_torus_add(
        major_segments=max(16, min(int(parameters.get("radial_segments", 64)), 128)),
        minor_segments=max(8, min(int(parameters.get("section_segments", 20)), 64)),
        major_radius=major_radius * to_local,
        minor_radius=minor_radius * to_local,
        location=_component_location(parameters, to_local),
    )
    tire = bpy.context.active_object
    tire.name = name
    axis = str(parameters.get("axis", "z")).lower()
    if axis == "x":
        tire.rotation_euler[1] = math.pi / 2.0
    elif axis == "y":
        tire.rotation_euler[0] = math.pi / 2.0
    elif axis != "z":
        raise ValueError("tire axis must be x, y, or z")
    _move_to_collection(tire, collection)
    if material:
        tire.data.materials.append(material)
    tire["bvmcp_surface_method"] = "parametric_tire_profile"
    return [tire]


def _create_spoke_array(name, parameters, collection, to_local, material):
    location = Vector(_component_location(parameters, to_local))
    radius = _component_number(parameters, "radius_mm", 160.0, minimum=0.001) * to_local
    depth = _component_number(parameters, "depth_mm", 18.0, minimum=0.001) * to_local
    count = max(3, min(int(parameters.get("spoke_count", 10)), 128))
    objects = _create_cylinder_component(
        f"{name}_hub",
        {
            "radius_mm": radius / to_local * 0.18,
            "depth_mm": depth / to_local,
            "location_mm": [value / to_local for value in location],
        },
        collection,
        to_local,
        material,
    )
    for index in range(count):
        angle = 2.0 * math.pi * index / count
        spoke = _local_cube(
            f"{name}_spoke_{index + 1:03d}",
            None,
            location
            + Vector((math.cos(angle) * radius * 0.58, math.sin(angle) * radius * 0.58, 0.0)),
            (radius * 0.82, radius * 0.055, depth * 0.55),
            material,
        )
        spoke.rotation_euler[2] = angle
        _move_to_collection(spoke, collection)
        objects.append(spoke)
    return objects


def _create_controlled_shrinkwrap(name, parameters, collection, to_local, material):
    objects = _create_patch_surface(name, parameters, collection, to_local, material)
    target_name = str(parameters.get("target_object", "")).strip()
    target = bpy.data.objects.get(target_name)
    if target is None:
        raise ValueError("controlled shrinkwrap requires an existing target_object")
    modifier = objects[0].modifiers.new("BVMCP_ControlledShrinkwrap", "SHRINKWRAP")
    modifier.target = target
    modifier.wrap_method = "NEAREST_SURFACEPOINT"
    modifier.offset = _component_number(parameters, "offset_mm", 0.0, minimum=0.0) * to_local
    objects[0]["bvmcp_shrinkwrap_target"] = target_name
    return objects


def _component_material(component: dict[str, object]):
    parameters = component.get("parameters", {})
    color = parameters.get("color_rgba", [0.28, 0.3, 0.33, 1.0])
    if not isinstance(color, list) or len(color) != 4:
        raise ValueError("component color_rgba must contain four values")
    return _material(
        f"BVMCP_{component['id']}_Material",
        tuple(float(value) for value in color),
        float(parameters.get("metallic", 0.2)),
        float(parameters.get("roughness", 0.45)),
    )


def _validate_component_constraints(component: dict[str, object]) -> list[dict[str, object]]:
    parameters = component.get("parameters", {})
    checks = []
    for constraint in component.get("constraints", []):
        kind = str(constraint.get("type", ""))
        values = constraint.get("parameters", {})
        parameter = values.get("parameter")
        expected = values.get("value", values.get("millimetres"))
        tolerance = float(values.get("tolerance", values.get("tolerance_mm", 0.0)))
        if (
            kind in {"known_dimension", "fixed_offset"}
            and parameter in parameters
            and isinstance(expected, (int, float))
        ):
            residual = float(parameters[parameter]) - float(expected)
            checks.append(
                {
                    "constraint_id": constraint.get("id"),
                    "type": kind,
                    "passed": abs(residual) <= tolerance,
                    "residual": residual,
                    "tolerance": tolerance,
                }
            )
        else:
            checks.append(
                {
                    "constraint_id": constraint.get("id"),
                    "type": kind,
                    "passed": None,
                    "validation": "retained_for_scene_or_cross_view_validation",
                }
            )
    return checks


def generate_components(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    output = confined(root, str(parameters["output_path"]))
    output.parent.mkdir(parents=True, exist_ok=True)
    components = parameters.get("components")
    if not isinstance(components, list) or not components:
        raise ValueError("generate_components requires at least one component record")
    if len(components) > 256:
        raise ValueError("generate_components exceeds the 256-component safety limit")
    collection = bpy.data.collections.get("BVMCP_Generated")
    if collection is None:
        collection = bpy.data.collections.new("BVMCP_Generated")
        bpy.context.scene.collection.children.link(collection)
    scale_to_mm = float(bpy.context.scene.unit_settings.scale_length) * 1000.0
    if not math.isfinite(scale_to_mm) or scale_to_mm <= 0:
        raise ValueError("scene unit scale cannot be converted to millimetres")
    to_local = 1.0 / scale_to_mm
    generated = []
    all_constraint_checks = []
    for component in components:
        if not isinstance(component, dict):
            raise ValueError("component records must be JSON objects")
        component_id = str(component.get("id", ""))
        if not component_id or any(
            character not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-"
            for character in component_id
        ):
            raise ValueError("component id contains unsafe characters")
        component_type = str(component.get("type", ""))
        if component_type not in COMPONENT_TYPES:
            raise ValueError(f"unsupported component type: {component_type}")
        if bpy.data.objects.get(component_id) is not None:
            raise ValueError(f"component object already exists: {component_id}")
        component_parameters = component.get("parameters", {})
        if not isinstance(component_parameters, dict):
            raise ValueError("component parameters must be a JSON object")
        material = _component_material(component)
        if component_type in {"VentArray", "HoleArray", "Grille", "HeatSink"}:
            objects = _create_geometry_nodes_array(
                component_id, component_parameters, collection, to_local, material
            )
        elif component_type in {"Button", "Screw", "Foot"}:
            objects = _create_cylinder_component(
                component_id, component_parameters, collection, to_local, material
            )
        elif component_type in {"Fan", "BladeArray"}:
            objects = _create_fan(
                component_id, component_parameters, collection, to_local, material
            )
        elif component_type in {"TireProfile"}:
            objects = _create_tire_profile(
                component_id, component_parameters, collection, to_local, material
            )
        elif component_type in {"WheelSpokeArray", "BrakeAssembly"}:
            objects = _create_spoke_array(
                component_id, component_parameters, collection, to_local, material
            )
        elif component_type in {"LoftedSurface", "Aerofoil", "Duct"}:
            objects = _create_loft_surface(
                component_id, component_parameters, collection, to_local, material
            )
        elif component_type in {
            "PatchSurface",
            "GlassPanel",
            "UnderbodyPanel",
            "DiffuserChannel",
            "RetopologyCage",
        }:
            objects = _create_patch_surface(
                component_id, component_parameters, collection, to_local, material
            )
        elif component_type == "ControlledShrinkwrap":
            objects = _create_controlled_shrinkwrap(
                component_id, component_parameters, collection, to_local, material
            )
        elif component_type in {"Bezier", "SplineBodySection", "WheelArch"}:
            objects = _create_curve_component(
                component_id,
                component_parameters,
                collection,
                to_local,
                material,
                spline_kind="BEZIER",
            )
        elif component_type == "NURBS":
            objects = _create_curve_component(
                component_id,
                component_parameters,
                collection,
                to_local,
                material,
                spline_kind="NURBS",
            )
        elif component_type in {"CurveNetwork", "Sweep", "PanelGap", "SurfaceCrease"}:
            objects = _create_curve_component(
                component_id,
                component_parameters,
                collection,
                to_local,
                material,
                spline_kind="POLY",
            )
        else:
            objects = _create_cube_component(
                component_id, component_parameters, collection, to_local, material
            )
        bindings = component.get("evidence_bindings", [])
        for obj in objects:
            obj["bvmcp_component_id"] = component_id
            obj["bvmcp_component_type"] = component_type
            obj["bvmcp_generator_version"] = str(component.get("generator_version", "1"))
            obj["bvmcp_evidence_bindings"] = json.dumps(bindings, sort_keys=True)
            if obj.type == "MESH" and any(abs(value - 1.0) > 1e-9 for value in obj.scale):
                bpy.context.view_layer.objects.active = obj
                obj.select_set(True)
                bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
                obj.select_set(False)
        constraint_checks = _validate_component_constraints(component)
        all_constraint_checks.extend(constraint_checks)
        generated.append(
            {
                "component_id": component_id,
                "component_type": component_type,
                "objects": [obj.name for obj in objects],
                "geometry_nodes": [
                    modifier.node_group.name
                    for obj in objects
                    for modifier in obj.modifiers
                    if modifier.type == "NODES" and modifier.node_group
                ],
                "evidence_bindings": bindings,
                "constraint_checks": constraint_checks,
            }
        )
    bpy.context.view_layer.update()
    bpy.ops.wm.save_as_mainfile(filepath=str(output), copy=True, compress=True)
    digest = hashlib.sha256(output.read_bytes()).hexdigest()
    return {
        "checkpoint_path": str(output.relative_to(root)),
        "checkpoint_sha256": digest,
        "generated": generated,
        "component_count": len(generated),
        "object_count": sum(len(item["objects"]) for item in generated),
        "constraint_checks": all_constraint_checks,
        "failed_constraints": [
            item for item in all_constraint_checks if item.get("passed") is False
        ],
        "unit_scale_to_millimetres": scale_to_mm,
    }


def generate_semantic_seed(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    """Create a fresh editable hypothesis scene without loading a starting model."""
    bpy.ops.object.select_all(action="SELECT")
    bpy.ops.object.delete(use_global=False)
    for collection in list(bpy.data.collections):
        if collection.name != bpy.context.scene.collection.name:
            bpy.data.collections.remove(collection)
    scene = bpy.context.scene
    scene.unit_settings.system = "METRIC"
    scene.unit_settings.scale_length = 1.0
    scene["bvmcp_evidence_authority"] = "INFERRED_PARAMETRIC_HYPOTHESIS"
    scene["bvmcp_acceptance_eligible"] = False
    scene["bvmcp_private_starting_model"] = False
    result = generate_components(root, parameters)
    return {
        **result,
        "seed_kind": "fresh_semantic_parametric_hypothesis",
        "private_starting_model": False,
        "accepted": False,
    }


def save_checkpoint(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    output = confined(root, str(parameters["output_path"]))
    if output.suffix.lower() != ".blend":
        raise ValueError("checkpoint output must use the .blend extension")
    output.parent.mkdir(parents=True, exist_ok=True)
    bpy.ops.wm.save_as_mainfile(filepath=str(output), copy=True, compress=True)
    return {
        "checkpoint_path": str(output.relative_to(root)),
        "checkpoint_sha256": hashlib.sha256(output.read_bytes()).hexdigest(),
        "size": output.stat().st_size,
    }


def export_blend(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    return save_checkpoint(root, parameters)


RTX_5090_FE_BODY_PATTERNS = (
    "fe-shroud",
    "fe-front-*",
    "fan-*",
    "fe-linr*",
    "fe-recessback*",
    "fe-flowfin*",
    "fe-edge-*",
    "fe-pwr-*",
    "fe-backplate",
    "fe-rear-*",
    "fe-topfin*",
    "fe-side-*",
)


def _name_matches(name: str, patterns: tuple[str, ...]) -> bool:
    return any(fnmatchcase(name, pattern) for pattern in patterns)


def _translate_world(obj, shift: Vector) -> None:
    local_shift = shift
    if obj.parent:
        local_shift = obj.parent.matrix_world.inverted().to_3x3() @ shift
    obj.location += local_shift


def _combined_object_bounds(objects) -> dict[str, list[float]]:
    bounds = [object_world_bounds(obj) for obj in objects]
    minimum = [min(item["minimum"][axis] for item in bounds) for axis in range(3)]
    maximum = [max(item["maximum"][axis] for item in bounds) for axis in range(3)]
    return {
        "minimum": minimum,
        "maximum": maximum,
        "dimensions": [maximum[axis] - minimum[axis] for axis in range(3)],
    }


def revise_rtx_5090_fe_candidate(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    """Create a distinct strict candidate while retaining the historical source scene."""
    source_revision = str(parameters.get("source_revision", "")).strip()
    if not source_revision:
        raise ValueError("RTX 5090 FE candidate revision requires a source revision")
    output = confined(root, str(parameters["output_path"]))
    output.parent.mkdir(parents=True, exist_ok=True)
    scale_to_mm = float(bpy.context.scene.unit_settings.scale_length) * 1000.0
    if scale_to_mm <= 0.0:
        raise ValueError("scene unit scale must be positive")
    millimetres_to_world = 1.0 / scale_to_mm
    body_objects = [
        obj
        for obj in bpy.context.scene.objects
        if obj.type == "MESH"
        and not obj.hide_render
        and _name_matches(obj.name, RTX_5090_FE_BODY_PATTERNS)
    ]
    if not body_objects or bpy.data.objects.get("fe-shroud") is None:
        raise ValueError("scene is not the expected RTX 5090 FE candidate")

    before = _combined_object_bounds(body_objects)
    half_width = 68.5 * millimetres_to_world
    front_plane = -20.0 * millimetres_to_world
    adjustments = []
    for obj in sorted(body_objects, key=lambda item: item.name):
        bounds = object_world_bounds(obj)
        shift_x = 0.0
        shift_y = 0.0
        if bounds["minimum"][0] < -half_width:
            shift_x = -half_width - bounds["minimum"][0]
        elif bounds["maximum"][0] > half_width:
            shift_x = half_width - bounds["maximum"][0]

        target_surface_mm = None
        if _name_matches(obj.name, ("fan-*", "fe-front-*", "fe-linr*")):
            if bounds["minimum"][1] < front_plane:
                shift_y = front_plane - bounds["minimum"][1]
                target_surface_mm = -20.0
        elif fnmatchcase(obj.name, "fe-flowfin*"):
            target_surface_mm = 17.0
        elif obj.name == "fe-backplate":
            target_surface_mm = 18.0
        elif fnmatchcase(obj.name, "fe-rear-*"):
            target_surface_mm = 20.0
        if target_surface_mm is not None and target_surface_mm >= 0.0:
            target_surface = target_surface_mm * millimetres_to_world
            shift_y = target_surface - bounds["maximum"][1]

        if abs(shift_x) > 1e-12 or abs(shift_y) > 1e-12:
            _translate_world(obj, Vector((shift_x, shift_y, 0.0)))
            adjustments.append(
                {
                    "object": obj.name,
                    "x_shift_mm": shift_x * scale_to_mm,
                    "y_shift_mm": shift_y * scale_to_mm,
                    "target_surface_mm": target_surface_mm,
                }
            )

    bpy.context.view_layer.update()
    after = _combined_object_bounds(body_objects)
    dimensions_mm = [value * scale_to_mm for value in after["dimensions"]]
    targets_mm = [137.0, 40.0, 304.0]
    tolerance_mm = float(parameters.get("tolerance_mm", 0.25))
    checks = {
        axis: {
            "actual_mm": dimensions_mm[index],
            "target_mm": targets_mm[index],
            "absolute_delta_mm": abs(dimensions_mm[index] - targets_mm[index]),
            "tolerance_mm": tolerance_mm,
            "passed": abs(dimensions_mm[index] - targets_mm[index]) <= tolerance_mm,
        }
        for index, axis in enumerate(("x", "y", "z"))
    }
    failed = [axis for axis, check in checks.items() if not check["passed"]]
    if failed:
        raise ValueError("strict RTX 5090 FE envelope still fails axes: " + ", ".join(failed))

    bpy.context.scene["bvmcp_benchmark"] = "rtx_5090_fe"
    bpy.context.scene["bvmcp_benchmark_revision"] = "strict-v2"
    bpy.context.scene["bvmcp_source_revision"] = source_revision
    bpy.context.scene["bvmcp_candidate_accepted"] = False
    checkpoint = save_checkpoint(root, parameters)
    return {
        **checkpoint,
        "benchmark": "rtx_5090_fe",
        "revision": "strict-v2",
        "source_revision": source_revision,
        "before_bounds": before,
        "after_bounds": after,
        "dimensions_mm": dict(zip(("x", "y", "z"), dimensions_mm, strict=True)),
        "dimension_checks": checks,
        "adjustments": adjustments,
        "adjustment_count": len(adjustments),
        "accepted": False,
        "reason": (
            "Automated candidate revision requires visual, feature, material, and human review."
        ),
    }


def _rtx_fan_blade_mesh(
    name: str,
    *,
    center_x: float,
    center_z: float,
    scale_to_mm: float,
    blade_count: int = 7,
) -> bpy.types.Mesh:
    to_world = 1.0 / scale_to_mm
    vertices: list[tuple[float, float, float]] = []
    faces: list[tuple[int, ...]] = []
    station_count = 9
    for blade in range(blade_count):
        blade_start = len(vertices)
        base_angle = 2.0 * math.pi * blade / blade_count
        for station in range(station_count):
            t = station / (station_count - 1)
            radius = (15.0 + (54.4 - 15.0) * t) * to_world
            center_angle = base_angle + math.radians(31.0) * t**1.3
            half_angle = math.radians(15.0 - 2.0 * t + 2.0 * math.sin(math.pi * t))
            leading_angle = center_angle - half_angle
            trailing_angle = center_angle + half_angle
            front_leading = (-19.2 + 0.8 * t) * to_world
            front_trailing = (-18.0 + 1.0 * t) * to_world
            back_leading = front_leading + 1.1 * to_world
            back_trailing = front_trailing + 1.1 * to_world
            leading_x = center_x + radius * math.cos(leading_angle)
            leading_z = center_z + radius * math.sin(leading_angle)
            trailing_x = center_x + radius * math.cos(trailing_angle)
            trailing_z = center_z + radius * math.sin(trailing_angle)
            vertices.extend(
                (
                    (leading_x, front_leading, leading_z),
                    (trailing_x, front_trailing, trailing_z),
                    (leading_x, back_leading, leading_z),
                    (trailing_x, back_trailing, trailing_z),
                )
            )
        for station in range(station_count - 1):
            current = blade_start + station * 4
            following = current + 4
            faces.extend(
                (
                    (current, following, following + 1, current + 1),
                    (current + 2, current + 3, following + 3, following + 2),
                    (current, current + 2, following + 2, following),
                    (current + 1, following + 1, following + 3, current + 3),
                )
            )
        root = blade_start
        tip = blade_start + (station_count - 1) * 4
        faces.extend(
            (
                (root, root + 1, root + 3, root + 2),
                (tip, tip + 2, tip + 3, tip + 1),
            )
        )
    mesh = bpy.data.meshes.new(name)
    mesh.from_pydata(vertices, [], faces)
    mesh.validate(verbose=False)
    mesh.update()
    graph = bmesh.new()
    graph.from_mesh(mesh)
    bmesh.ops.recalc_face_normals(graph, faces=list(graph.faces))
    graph.to_mesh(mesh)
    graph.free()
    return mesh


def _rtx_fan_liner_mesh(
    name: str,
    *,
    center_x: float,
    center_z: float,
    front_y: float,
    back_y: float,
    inner_radius: float,
    outer_radius: float,
    segments: int = 96,
) -> bpy.types.Mesh:
    vertices: list[tuple[float, float, float]] = []
    faces: list[tuple[int, ...]] = []
    for index in range(segments):
        angle = 2.0 * math.pi * index / segments
        cosine = math.cos(angle)
        sine = math.sin(angle)
        vertices.extend(
            (
                (
                    center_x + outer_radius * cosine,
                    front_y,
                    center_z + outer_radius * sine,
                ),
                (
                    center_x + outer_radius * cosine,
                    back_y,
                    center_z + outer_radius * sine,
                ),
                (
                    center_x + inner_radius * cosine,
                    front_y,
                    center_z + inner_radius * sine,
                ),
                (
                    center_x + inner_radius * cosine,
                    back_y,
                    center_z + inner_radius * sine,
                ),
            )
        )
    for index in range(segments):
        current = index * 4
        following = ((index + 1) % segments) * 4
        faces.extend(
            (
                (current, following, following + 1, current + 1),
                (current + 2, current + 3, following + 3, following + 2),
                (current, current + 2, following + 2, following),
                (current + 1, following + 1, following + 3, current + 3),
            )
        )
    mesh = bpy.data.meshes.new(name)
    mesh.from_pydata(vertices, [], faces)
    mesh.validate(verbose=False)
    mesh.update()
    graph = bmesh.new()
    graph.from_mesh(mesh)
    bmesh.ops.recalc_face_normals(graph, faces=list(graph.faces))
    graph.to_mesh(mesh)
    graph.free()
    return mesh


def _rtx_xz_prism_mesh(
    name: str,
    *,
    points: list[tuple[float, float]],
    front_y: float,
    back_y: float,
) -> bpy.types.Mesh:
    if len(points) < 3:
        raise ValueError("an XZ prism requires at least three footprint points")
    vertices = [(x, front_y, z) for x, z in points] + [(x, back_y, z) for x, z in points]
    count = len(points)
    faces: list[tuple[int, ...]] = [
        tuple(range(count)),
        tuple(reversed(range(count, count * 2))),
    ]
    for index in range(count):
        following = (index + 1) % count
        faces.append((index, following, following + count, index + count))
    mesh = bpy.data.meshes.new(name)
    mesh.from_pydata(vertices, [], faces)
    mesh.validate(verbose=False)
    mesh.update()
    graph = bmesh.new()
    graph.from_mesh(mesh)
    bmesh.ops.recalc_face_normals(graph, faces=list(graph.faces))
    graph.to_mesh(mesh)
    graph.free()
    return mesh


def _rtx_strip_points(
    start: tuple[float, float], end: tuple[float, float], width: float
) -> list[tuple[float, float]]:
    delta_x = end[0] - start[0]
    delta_z = end[1] - start[1]
    length = math.hypot(delta_x, delta_z)
    if length <= 1e-12:
        raise ValueError("an XZ strip requires distinct endpoints")
    offset_x = -delta_z / length * width / 2.0
    offset_z = delta_x / length * width / 2.0
    return [
        (start[0] + offset_x, start[1] + offset_z),
        (end[0] + offset_x, end[1] + offset_z),
        (end[0] - offset_x, end[1] - offset_z),
        (start[0] - offset_x, start[1] - offset_z),
    ]


def _rtx_rounded_rect_ring_mesh(
    name: str,
    *,
    half_width: float,
    half_height: float,
    radius: float,
    ring_width: float,
    front_y: float,
    back_y: float,
    corner_segments: int = 12,
) -> bpy.types.Mesh:
    if ring_width <= 0.0 or ring_width >= radius:
        raise ValueError("rounded rectangle ring width must be positive and below radius")

    def perimeter_points(
        selected_half_width: float,
        selected_half_height: float,
        selected_radius: float,
    ) -> list[tuple[float, float]]:
        points = []
        corners = (
            (selected_half_width - selected_radius, selected_half_height - selected_radius, 0.0),
            (
                -selected_half_width + selected_radius,
                selected_half_height - selected_radius,
                math.pi / 2.0,
            ),
            (
                -selected_half_width + selected_radius,
                -selected_half_height + selected_radius,
                math.pi,
            ),
            (
                selected_half_width - selected_radius,
                -selected_half_height + selected_radius,
                3.0 * math.pi / 2.0,
            ),
        )
        for center_x, center_z, start_angle in corners:
            for index in range(corner_segments):
                angle = start_angle + index / corner_segments * math.pi / 2.0
                points.append(
                    (
                        center_x + selected_radius * math.cos(angle),
                        center_z + selected_radius * math.sin(angle),
                    )
                )
        return points

    outer = perimeter_points(half_width, half_height, radius)
    inner = perimeter_points(
        half_width - ring_width,
        half_height - ring_width,
        radius - ring_width,
    )
    vertices: list[tuple[float, float, float]] = []
    for outer_point, inner_point in zip(outer, inner, strict=True):
        vertices.extend(
            (
                (outer_point[0], front_y, outer_point[1]),
                (outer_point[0], back_y, outer_point[1]),
                (inner_point[0], front_y, inner_point[1]),
                (inner_point[0], back_y, inner_point[1]),
            )
        )
    faces: list[tuple[int, ...]] = []
    for index in range(len(outer)):
        current = index * 4
        following = ((index + 1) % len(outer)) * 4
        faces.extend(
            (
                (current, following, following + 1, current + 1),
                (current + 2, current + 3, following + 3, following + 2),
                (current, current + 2, following + 2, following),
                (current + 1, following + 1, following + 3, current + 3),
            )
        )
    mesh = bpy.data.meshes.new(name)
    mesh.from_pydata(vertices, [], faces)
    mesh.validate(verbose=False)
    mesh.update()
    graph = bmesh.new()
    graph.from_mesh(mesh)
    bmesh.ops.recalc_face_normals(graph, faces=list(graph.faces))
    graph.to_mesh(mesh)
    graph.free()
    return mesh


def _rtx_contact_bank_mesh(
    name: str,
    *,
    minimum_x: float,
    maximum_x: float,
    minimum_z: float,
    maximum_z: float,
    contact_pitch: float,
    contact_width: float,
    front_y: float,
    back_y: float,
) -> tuple[bpy.types.Mesh, int]:
    span = maximum_z - minimum_z
    contact_count = max(1, int(round(span / contact_pitch)))
    vertices: list[tuple[float, float, float]] = []
    faces: list[tuple[int, ...]] = []
    for index in range(contact_count):
        center_z = minimum_z + (index + 0.5) / contact_count * span
        half_width = min(contact_width / 2.0, span / contact_count * 0.45)
        points = (
            (minimum_x, center_z - half_width),
            (maximum_x, center_z - half_width),
            (maximum_x, center_z + half_width),
            (minimum_x, center_z + half_width),
        )
        start = len(vertices)
        vertices.extend((x, front_y, z) for x, z in points)
        vertices.extend((x, back_y, z) for x, z in points)
        faces.extend(
            (
                (start, start + 1, start + 2, start + 3),
                (start + 7, start + 6, start + 5, start + 4),
                (start, start + 4, start + 5, start + 1),
                (start + 1, start + 5, start + 6, start + 2),
                (start + 2, start + 6, start + 7, start + 3),
                (start + 3, start + 7, start + 4, start),
            )
        )
    mesh = bpy.data.meshes.new(name)
    mesh.from_pydata(vertices, [], faces)
    mesh.validate(verbose=False)
    mesh.update()
    graph = bmesh.new()
    graph.from_mesh(mesh)
    bmesh.ops.recalc_face_normals(graph, faces=list(graph.faces))
    graph.to_mesh(mesh)
    graph.free()
    return mesh, contact_count


def _rtx_axis_aligned_boxes_mesh(
    name: str,
    boxes: list[tuple[float, float, float, float, float, float]],
) -> bpy.types.Mesh:
    vertices: list[tuple[float, float, float]] = []
    faces: list[tuple[int, ...]] = []
    for minimum_x, minimum_y, minimum_z, maximum_x, maximum_y, maximum_z in boxes:
        if not (maximum_x > minimum_x and maximum_y > minimum_y and maximum_z > minimum_z):
            raise ValueError("axis-aligned box bounds must be strictly increasing")
        start = len(vertices)
        vertices.extend(
            (
                (minimum_x, minimum_y, minimum_z),
                (maximum_x, minimum_y, minimum_z),
                (maximum_x, maximum_y, minimum_z),
                (minimum_x, maximum_y, minimum_z),
                (minimum_x, minimum_y, maximum_z),
                (maximum_x, minimum_y, maximum_z),
                (maximum_x, maximum_y, maximum_z),
                (minimum_x, maximum_y, maximum_z),
            )
        )
        faces.extend(
            (
                (start, start + 3, start + 2, start + 1),
                (start + 4, start + 5, start + 6, start + 7),
                (start, start + 1, start + 5, start + 4),
                (start + 1, start + 2, start + 6, start + 5),
                (start + 2, start + 3, start + 7, start + 6),
                (start + 3, start, start + 4, start + 7),
            )
        )
    mesh = bpy.data.meshes.new(name)
    mesh.from_pydata(vertices, [], faces)
    mesh.validate(verbose=False)
    mesh.update()
    graph = bmesh.new()
    graph.from_mesh(mesh)
    bmesh.ops.recalc_face_normals(graph, faces=list(graph.faces))
    graph.to_mesh(mesh)
    graph.free()
    return mesh


def _rtx_yz_rect_ring_mesh(
    name: str,
    *,
    outer_minimum_y: float,
    outer_maximum_y: float,
    outer_minimum_z: float,
    outer_maximum_z: float,
    ring_width: float,
    front_x: float,
    back_x: float,
) -> bpy.types.Mesh:
    outer = (
        (outer_minimum_y, outer_minimum_z),
        (outer_maximum_y, outer_minimum_z),
        (outer_maximum_y, outer_maximum_z),
        (outer_minimum_y, outer_maximum_z),
    )
    inner = (
        (outer_minimum_y + ring_width, outer_minimum_z + ring_width),
        (outer_maximum_y - ring_width, outer_minimum_z + ring_width),
        (outer_maximum_y - ring_width, outer_maximum_z - ring_width),
        (outer_minimum_y + ring_width, outer_maximum_z - ring_width),
    )
    vertices: list[tuple[float, float, float]] = []
    for outer_point, inner_point in zip(outer, inner, strict=True):
        vertices.extend(
            (
                (front_x, outer_point[0], outer_point[1]),
                (back_x, outer_point[0], outer_point[1]),
                (front_x, inner_point[0], inner_point[1]),
                (back_x, inner_point[0], inner_point[1]),
            )
        )
    faces: list[tuple[int, ...]] = []
    for index in range(4):
        current = index * 4
        following = ((index + 1) % 4) * 4
        faces.extend(
            (
                (current, following, following + 1, current + 1),
                (current + 2, current + 3, following + 3, following + 2),
                (current, current + 2, following + 2, following),
                (current + 1, following + 1, following + 3, current + 3),
            )
        )
    mesh = bpy.data.meshes.new(name)
    mesh.from_pydata(vertices, [], faces)
    mesh.validate(verbose=False)
    mesh.update()
    graph = bmesh.new()
    graph.from_mesh(mesh)
    bmesh.ops.recalc_face_normals(graph, faces=list(graph.faces))
    graph.to_mesh(mesh)
    graph.free()
    return mesh


def _dgx_xz_rounded_prism_mesh(
    name: str,
    *,
    center_x: float,
    center_z: float,
    half_width: float,
    half_height: float,
    radius: float,
    front_y: float,
    back_y: float,
    corner_segments: int = 10,
) -> bpy.types.Mesh:
    """Build a watertight rounded-rectangle prism facing the DGX front/rear axis."""
    if half_width <= 0.0 or half_height <= 0.0:
        raise ValueError("rounded prism half extents must be positive")
    radius = min(radius, half_width, half_height)
    points: list[tuple[float, float]] = []
    corners = (
        (center_x + half_width - radius, center_z + half_height - radius, 0.0),
        (
            center_x - half_width + radius,
            center_z + half_height - radius,
            math.pi / 2.0,
        ),
        (
            center_x - half_width + radius,
            center_z - half_height + radius,
            math.pi,
        ),
        (
            center_x + half_width - radius,
            center_z - half_height + radius,
            3.0 * math.pi / 2.0,
        ),
    )
    for corner_x, corner_z, start_angle in corners:
        for index in range(corner_segments):
            angle = start_angle + index / corner_segments * math.pi / 2.0
            points.append(
                (
                    corner_x + radius * math.cos(angle),
                    corner_z + radius * math.sin(angle),
                )
            )
    return _rtx_xz_prism_mesh(
        name,
        points=points,
        front_y=front_y,
        back_y=back_y,
    )


def _dgx_assign_material(obj, material) -> None:
    if obj is None or obj.type != "MESH":
        raise ValueError("DGX material target is missing or is not a mesh")
    obj.data.materials.clear()
    obj.data.materials.append(material)


def _dgx_procedural_foam_material(
    name: str,
    *,
    color: tuple[float, float, float, float],
    metallic: float,
    roughness: float,
    coordinate_scale: float,
    thickness_noise_scale: float,
    minimum_edge_distance: float,
    maximum_edge_distance: float,
    seed: int,
) -> tuple[bpy.types.Material, dict[str, object]]:
    """Create a render-only cellular alpha field with rounded, varying struts."""
    material = bpy.data.materials.get(name) or bpy.data.materials.new(name)
    material.diffuse_color = color
    material.use_nodes = True
    material.surface_render_method = "DITHERED"
    if hasattr(material, "use_transparency_overlap"):
        material.use_transparency_overlap = False
    material["bvmcp_authority"] = (
        "render-only fine cellular proxy; physical aperture authority is the paired hidden-render "
        "watertight web mesh"
    )
    tree = material.node_tree
    if tree is None:
        raise ValueError("DGX procedural foam material has no node tree")
    tree.nodes.clear()
    nodes = tree.nodes
    links = tree.links

    output = nodes.new("ShaderNodeOutputMaterial")
    output.location = (1020.0, 0.0)
    mix = nodes.new("ShaderNodeMixShader")
    mix.location = (800.0, 0.0)
    transparent = nodes.new("ShaderNodeBsdfTransparent")
    transparent.location = (570.0, -110.0)
    principled = nodes.new("ShaderNodeBsdfPrincipled")
    principled.location = (570.0, 120.0)
    principled.inputs["Base Color"].default_value = color
    principled.inputs["Metallic"].default_value = metallic
    principled.inputs["Roughness"].default_value = roughness

    geometry = nodes.new("ShaderNodeNewGeometry")
    geometry.location = (-1050.0, 80.0)
    separate = nodes.new("ShaderNodeSeparateXYZ")
    separate.location = (-850.0, 80.0)
    combine = nodes.new("ShaderNodeCombineXYZ")
    combine.location = (-650.0, 80.0)
    combine.inputs["Z"].default_value = float(seed % 1009) / 37.0
    links.new(geometry.outputs["Position"], separate.inputs["Vector"])
    links.new(separate.outputs["X"], combine.inputs["X"])
    links.new(separate.outputs["Z"], combine.inputs["Y"])

    voronoi = nodes.new("ShaderNodeTexVoronoi")
    voronoi.location = (-430.0, 190.0)
    voronoi.voronoi_dimensions = "3D"
    voronoi.feature = "DISTANCE_TO_EDGE"
    voronoi.distance = "EUCLIDEAN"
    voronoi.inputs["Scale"].default_value = coordinate_scale
    voronoi.inputs["Randomness"].default_value = 1.0
    links.new(combine.outputs["Vector"], voronoi.inputs["Vector"])

    noise = nodes.new("ShaderNodeTexNoise")
    noise.location = (-430.0, -140.0)
    noise.noise_dimensions = "3D"
    noise.inputs["Scale"].default_value = thickness_noise_scale
    noise.inputs["Detail"].default_value = 3.0
    noise.inputs["Roughness"].default_value = 0.68
    links.new(combine.outputs["Vector"], noise.inputs["Vector"])
    thickness = nodes.new("ShaderNodeMapRange")
    thickness.location = (-180.0, -130.0)
    thickness.inputs["From Min"].default_value = 0.0
    thickness.inputs["From Max"].default_value = 1.0
    thickness.inputs["To Min"].default_value = minimum_edge_distance
    thickness.inputs["To Max"].default_value = maximum_edge_distance
    links.new(noise.outputs["Fac"], thickness.inputs["Value"])

    strut_mask = nodes.new("ShaderNodeMath")
    strut_mask.location = (80.0, 20.0)
    strut_mask.operation = "LESS_THAN"
    links.new(voronoi.outputs["Distance"], strut_mask.inputs[0])
    links.new(thickness.outputs["Result"], strut_mask.inputs[1])
    rounded_height = nodes.new("ShaderNodeMath")
    rounded_height.location = (80.0, 210.0)
    rounded_height.operation = "SUBTRACT"
    links.new(thickness.outputs["Result"], rounded_height.inputs[0])
    links.new(voronoi.outputs["Distance"], rounded_height.inputs[1])
    bump = nodes.new("ShaderNodeBump")
    bump.location = (340.0, 210.0)
    bump.inputs["Strength"].default_value = 0.42
    bump.inputs["Distance"].default_value = 0.000075
    links.new(rounded_height.outputs["Value"], bump.inputs["Height"])
    links.new(bump.outputs["Normal"], principled.inputs["Normal"])

    links.new(strut_mask.outputs["Value"], mix.inputs[0])
    links.new(transparent.outputs["BSDF"], mix.inputs[1])
    links.new(principled.outputs["BSDF"], mix.inputs[2])
    links.new(mix.outputs["Shader"], output.inputs["Surface"])
    return material, {
        "seed": seed,
        "coordinate_scale": coordinate_scale,
        "thickness_noise_scale": thickness_noise_scale,
        "edge_distance_range": [minimum_edge_distance, maximum_edge_distance],
        "color": list(color),
        "metallic": metallic,
        "roughness": roughness,
        "surface_render_method": material.surface_render_method,
        "authority": material["bvmcp_authority"],
    }


def _dgx_transparent_material(name: str) -> bpy.types.Material:
    """Create a deterministic fully transparent material for proxy edge faces."""
    material = bpy.data.materials.get(name) or bpy.data.materials.new(name)
    material.use_nodes = True
    material.surface_render_method = "DITHERED"
    tree = material.node_tree
    if tree is None:
        raise ValueError("DGX transparent material has no node tree")
    tree.nodes.clear()
    output = tree.nodes.new("ShaderNodeOutputMaterial")
    transparent = tree.nodes.new("ShaderNodeBsdfTransparent")
    tree.links.new(transparent.outputs["BSDF"], output.inputs["Surface"])
    return material


def _dgx_cellfield_proxy_mesh(
    name: str,
    *,
    scale_to_mm: float,
    front_y_mm: float,
    back_y_mm: float,
    exclusions_mm: list[tuple[float, float, float, float, float]],
) -> bpy.types.Mesh:
    """Build watertight proxy tiles around rectangular bounds of product openings."""
    minimum_x_mm = -73.1
    maximum_x_mm = 73.1
    minimum_z_mm = 2.7
    maximum_z_mm = 47.8
    x_breaks = {minimum_x_mm, maximum_x_mm}
    z_breaks = {minimum_z_mm, maximum_z_mm}
    for center_x_mm, center_z_mm, half_width_mm, half_height_mm, _ in exclusions_mm:
        x_breaks.update(
            {
                max(minimum_x_mm, center_x_mm - half_width_mm),
                min(maximum_x_mm, center_x_mm + half_width_mm),
            }
        )
        z_breaks.update(
            {
                max(minimum_z_mm, center_z_mm - half_height_mm),
                min(maximum_z_mm, center_z_mm + half_height_mm),
            }
        )
    sorted_x = sorted(x_breaks)
    sorted_z = sorted(z_breaks)
    minimum_y_mm, maximum_y_mm = sorted((front_y_mm, back_y_mm))
    boxes = []
    for x_index in range(len(sorted_x) - 1):
        for z_index in range(len(sorted_z) - 1):
            epsilon_mm = 0.0005
            minimum_cell_x = sorted_x[x_index] + (epsilon_mm if x_index else 0.0)
            maximum_cell_x = sorted_x[x_index + 1] - (
                epsilon_mm if x_index < len(sorted_x) - 2 else 0.0
            )
            minimum_cell_z = sorted_z[z_index] + (epsilon_mm if z_index else 0.0)
            maximum_cell_z = sorted_z[z_index + 1] - (
                epsilon_mm if z_index < len(sorted_z) - 2 else 0.0
            )
            center_x = (minimum_cell_x + maximum_cell_x) * 0.5
            center_z = (minimum_cell_z + maximum_cell_z) * 0.5
            if any(
                abs(center_x - exclusion_x) <= half_width
                and abs(center_z - exclusion_z) <= half_height
                for exclusion_x, exclusion_z, half_width, half_height, _ in exclusions_mm
            ):
                continue
            boxes.append(
                (
                    minimum_cell_x / scale_to_mm,
                    minimum_y_mm / scale_to_mm,
                    minimum_cell_z / scale_to_mm,
                    maximum_cell_x / scale_to_mm,
                    maximum_y_mm / scale_to_mm,
                    maximum_cell_z / scale_to_mm,
                )
            )
    if not boxes:
        raise ValueError("DGX cellfield proxy exclusions removed the complete panel")
    return _rtx_axis_aligned_boxes_mesh(name, boxes)


def _dgx_voronoi_foam_web_lod_mesh(
    name: str,
    *,
    scale_to_mm: float,
    front_y_mm: float,
    back_y_mm: float,
    seed: int,
    exclusions_mm: list[tuple[float, float, float, float, float]],
    pitch_mm: float,
    site_retention: float,
    edge_width_range_mm: tuple[float, float],
    node_radius_mm: float,
) -> tuple[bpy.types.Mesh, dict[str, object]]:
    """Build a deterministic irregular open-cell render LOD from Voronoi cell walls."""
    if pitch_mm <= 0.0:
        raise ValueError("DGX Voronoi pitch must be positive")
    if not 0.0 < site_retention <= 1.0:
        raise ValueError("DGX Voronoi site retention must be in (0, 1]")
    if (
        edge_width_range_mm[0] <= 0.0
        or edge_width_range_mm[1] < edge_width_range_mm[0]
        or node_radius_mm <= 0.0
    ):
        raise ValueError("DGX Voronoi strut dimensions must be positive and ordered")
    to_world = 1.0 / scale_to_mm
    randomizer = random.Random(seed)
    minimum_x_mm = -73.1
    maximum_x_mm = 73.1
    minimum_z_mm = 2.7
    maximum_z_mm = 47.8
    row_count = int((maximum_z_mm - minimum_z_mm) / pitch_mm) + 1
    column_count = int((maximum_x_mm - minimum_x_mm) / pitch_mm) + 1

    def inside_exclusion(x_mm: float, z_mm: float, *, margin_mm: float = 0.0) -> bool:
        for center_x_mm, center_z_mm, half_width_mm, half_height_mm, radius_mm in exclusions_mm:
            qx = abs(x_mm - center_x_mm) - (half_width_mm - radius_mm)
            qz = abs(z_mm - center_z_mm) - (half_height_mm - radius_mm)
            signed_distance = (
                math.hypot(max(qx, 0.0), max(qz, 0.0)) + min(max(qx, qz), 0.0) - radius_mm
            )
            if signed_distance <= margin_mm:
                return True
        return False

    # A jittered square lattice is used only to distribute cell sites. The visible
    # network is made from the sites' Voronoi boundaries, so it has irregular
    # polygonal pores rather than the visibly repeated triangular graph used by v4.
    site_margin = 3
    jitter_mm = pitch_mm * 0.43
    sites: dict[tuple[int, int], tuple[float, float]] = {}
    for row in range(-site_margin, row_count + site_margin):
        base_z_mm = minimum_z_mm + row * pitch_mm
        for column in range(-site_margin, column_count + site_margin):
            base_x_mm = minimum_x_mm + column * pitch_mm
            x_mm = base_x_mm + randomizer.uniform(-jitter_mm, jitter_mm)
            z_mm = base_z_mm + randomizer.uniform(-jitter_mm, jitter_mm)
            if randomizer.random() <= site_retention:
                sites[(row, column)] = (x_mm, z_mm)

    def clip_to_site_half_plane(
        polygon: list[tuple[float, float]],
        site: tuple[float, float],
        neighbour: tuple[float, float],
    ) -> list[tuple[float, float]]:
        normal_x = neighbour[0] - site[0]
        normal_z = neighbour[1] - site[1]
        threshold = (
            neighbour[0] * neighbour[0]
            + neighbour[1] * neighbour[1]
            - site[0] * site[0]
            - site[1] * site[1]
        ) * 0.5

        def signed(point: tuple[float, float]) -> float:
            return point[0] * normal_x + point[1] * normal_z - threshold

        clipped: list[tuple[float, float]] = []
        previous = polygon[-1]
        previous_distance = signed(previous)
        previous_inside = previous_distance <= 1e-9
        for current in polygon:
            current_distance = signed(current)
            current_inside = current_distance <= 1e-9
            if current_inside != previous_inside:
                denominator = previous_distance - current_distance
                if abs(denominator) > 1e-12:
                    interpolation = previous_distance / denominator
                    clipped.append(
                        (
                            previous[0] + interpolation * (current[0] - previous[0]),
                            previous[1] + interpolation * (current[1] - previous[1]),
                        )
                    )
            if current_inside:
                clipped.append(current)
            previous = current
            previous_distance = current_distance
            previous_inside = current_inside
        return clipped

    quantization_per_mm = 1000.0

    def quantize(point: tuple[float, float]) -> tuple[int, int]:
        return (
            int(round(point[0] * quantization_per_mm)),
            int(round(point[1] * quantization_per_mm)),
        )

    network_edges: set[tuple[tuple[int, int], tuple[int, int]]] = set()
    retained_cell_count = 0
    for (row, column), site in sorted(sites.items()):
        polygon = [
            (minimum_x_mm, minimum_z_mm),
            (maximum_x_mm, minimum_z_mm),
            (maximum_x_mm, maximum_z_mm),
            (minimum_x_mm, maximum_z_mm),
        ]
        for neighbour_row in range(row - 4, row + 5):
            for neighbour_column in range(column - 4, column + 5):
                if (neighbour_row, neighbour_column) == (row, column):
                    continue
                neighbour = sites.get((neighbour_row, neighbour_column))
                if neighbour is None:
                    continue
                polygon = clip_to_site_half_plane(polygon, site, neighbour)
                if len(polygon) < 3:
                    break
            if len(polygon) < 3:
                break
        if len(polygon) < 3:
            continue
        retained_cell_count += 1
        for index, start in enumerate(polygon):
            end = polygon[(index + 1) % len(polygon)]
            length_mm = math.hypot(end[0] - start[0], end[1] - start[1])
            if length_mm < 0.24:
                continue
            # Remove complete wall segments that intrude into product openings.
            if any(
                inside_exclusion(
                    start[0] + fraction * (end[0] - start[0]),
                    start[1] + fraction * (end[1] - start[1]),
                    margin_mm=0.12,
                )
                for fraction in (0.0, 0.25, 0.5, 0.75, 1.0)
            ):
                continue
            start_key = quantize(start)
            end_key = quantize(end)
            if start_key == end_key:
                continue
            network_edges.add(tuple(sorted((start_key, end_key))))

    network_nodes = sorted({node for edge in network_edges for node in edge})
    edge_lengths_mm = [
        math.hypot(
            (edge[1][0] - edge[0][0]) / quantization_per_mm,
            (edge[1][1] - edge[0][1]) / quantization_per_mm,
        )
        for edge in sorted(network_edges)
    ]
    if not network_edges or not network_nodes:
        raise ValueError("DGX Voronoi foam generation produced an empty network")

    vertices: list[tuple[float, float, float]] = []
    faces: list[tuple[int, ...]] = []

    def add_prism(points_mm: list[tuple[float, float]]) -> None:
        start_index = len(vertices)
        vertices.extend(
            (x_mm * to_world, front_y_mm * to_world, z_mm * to_world) for x_mm, z_mm in points_mm
        )
        vertices.extend(
            (x_mm * to_world, back_y_mm * to_world, z_mm * to_world) for x_mm, z_mm in points_mm
        )
        count = len(points_mm)
        faces.append(tuple(start_index + index for index in range(count)))
        faces.append(tuple(start_index + count + index for index in reversed(range(count))))
        for index in range(count):
            following = (index + 1) % count
            faces.append(
                (
                    start_index + index,
                    start_index + following,
                    start_index + count + following,
                    start_index + count + index,
                )
            )

    edge_widths_mm = []
    for edge in sorted(network_edges):
        start = tuple(value / quantization_per_mm for value in edge[0])
        end = tuple(value / quantization_per_mm for value in edge[1])
        width_mm = randomizer.uniform(*edge_width_range_mm)
        edge_widths_mm.append(width_mm)
        add_prism(_rtx_strip_points(start, end, width_mm))

    for node in network_nodes:
        x_mm, z_mm = (value / quantization_per_mm for value in node)
        points_mm = []
        for index in range(8):
            angle = 2.0 * math.pi * index / 8.0
            radius_mm = node_radius_mm * randomizer.uniform(0.88, 1.12)
            points_mm.append(
                (
                    x_mm + radius_mm * math.cos(angle),
                    z_mm + radius_mm * math.sin(angle),
                )
            )
        add_prism(points_mm)

    mesh = bpy.data.meshes.new(name)
    mesh.from_pydata(vertices, [], faces)
    mesh.validate(verbose=False)
    mesh.update()
    graph = bmesh.new()
    graph.from_mesh(mesh)
    bmesh.ops.recalc_face_normals(graph, faces=list(graph.faces))
    graph.to_mesh(mesh)
    graph.free()
    return mesh, {
        "seed": seed,
        "network_model": "clipped jittered-site Voronoi cell boundaries",
        "pitch_mm": pitch_mm,
        "site_jitter_mm": jitter_mm,
        "site_retention": site_retention,
        "retained_cell_count": retained_cell_count,
        "node_count": len(network_nodes),
        "edge_count": len(network_edges),
        "edge_length_mm": {
            "minimum": min(edge_lengths_mm),
            "maximum": max(edge_lengths_mm),
            "mean": sum(edge_lengths_mm) / len(edge_lengths_mm),
        },
        "edge_width_mm": {
            "minimum": min(edge_widths_mm),
            "maximum": max(edge_widths_mm),
            "mean": sum(edge_widths_mm) / len(edge_widths_mm),
        },
        "depth_mm": abs(front_y_mm - back_y_mm),
        "exclusions_mm": [list(item) for item in exclusions_mm],
    }


def refine_dgx_spark_visual_candidate(
    root: Path, parameters: dict[str, object]
) -> dict[str, object]:
    """Correct DGX material response and rear-I/O detail without changing its body envelope."""
    source_revision = str(parameters.get("source_revision", "")).strip()
    if not source_revision:
        raise ValueError("DGX Spark visual refinement requires a source revision")
    body = bpy.data.objects.get("dgx-spark")
    front_foam = bpy.data.objects.get("dgx-spark-foam")
    rear_foam = bpy.data.objects.get("dgx-spark-foam-rear")
    io_plate = bpy.data.objects.get("dgx-spark-rear-ioplate")
    if any(obj is None or obj.type != "MESH" for obj in (body, front_foam, rear_foam, io_plate)):
        raise ValueError("scene is not the expected DGX Spark benchmark candidate")
    scale_to_mm = float(bpy.context.scene.unit_settings.scale_length) * 1000.0
    if scale_to_mm <= 0.0:
        raise ValueError("scene unit scale must be positive")
    to_world = 1.0 / scale_to_mm
    body_before = object_world_bounds(body)
    body_dimensions_mm = [value * scale_to_mm for value in body_before["dimensions"]]
    targets_mm = [150.0, 150.0, 50.5]
    dimension_checks = {
        axis: {
            "actual_mm": body_dimensions_mm[index],
            "target_mm": targets_mm[index],
            "absolute_delta_mm": abs(body_dimensions_mm[index] - targets_mm[index]),
            "tolerance_mm": 0.05,
            "passed": abs(body_dimensions_mm[index] - targets_mm[index]) <= 0.05,
        }
        for index, axis in enumerate(("x", "y", "z"))
    }
    failed_dimensions = [axis for axis, check in dimension_checks.items() if not check["passed"]]
    if failed_dimensions:
        raise ValueError(
            "DGX body envelope fails source dimensions: " + ", ".join(failed_dimensions)
        )

    material_specs = {
        "spark-gold": ((0.145, 0.084, 0.033, 1.0), 0.64, 0.43),
        "spark-gold.001": ((0.095, 0.052, 0.020, 1.0), 0.62, 0.40),
        "spark-gold.002": ((0.145, 0.084, 0.033, 1.0), 0.60, 0.46),
        "spark-bezel-satin": ((0.0002, 0.0002, 0.0002, 1.0), 0.0, 0.72),
        "spark-bezel-satin.001": ((0.145, 0.084, 0.033, 1.0), 0.64, 0.36),
        "spark-pill-wall": ((0.018, 0.012, 0.005, 1.0), 0.12, 0.68),
        "spark-pill-wall3d": ((0.120, 0.078, 0.044, 1.0), 0.54, 0.39),
        "spark-pill-wall3d.001": ((0.095, 0.055, 0.018, 1.0), 0.48, 0.40),
        "spark-tub": ((0.095, 0.052, 0.017, 1.0), 0.0, 0.66),
        "spark-tub.001": ((0.095, 0.052, 0.017, 1.0), 0.0, 0.66),
        "spark-foam3d": ((0.052, 0.032, 0.012, 1.0), 0.24, 0.64),
        "spark-foam3d.001": ((0.052, 0.032, 0.012, 1.0), 0.24, 0.64),
        "spark-foam-recess": ((0.006, 0.004, 0.002, 1.0), 0.08, 0.82),
        "spark-basecover-mat": ((0.135, 0.076, 0.029, 1.0), 0.30, 0.65),
        "spark-intake-mat": ((0.004, 0.004, 0.005, 1.0), 0.04, 0.76),
        "spark-top-vent": ((0.110, 0.074, 0.045, 1.0), 0.48, 0.46),
        "spark-nv-green": ((0.085, 0.315, 0.010, 1.0), 0.0, 0.44),
    }
    material_assignments = {}
    for material_name, (color, metallic, roughness) in material_specs.items():
        material = bpy.data.materials.get(material_name)
        if material is None:
            continue
        _material(
            material_name,
            color,
            metallic,
            roughness,
            specular_ior_level=(
                0.025
                if material_name == "spark-bezel-satin"
                else 0.04
                if material_name in {"spark-tub", "spark-tub.001"}
                else None
            ),
        )
        material_assignments[material_name] = {
            "base_color": list(color),
            "metallic": metallic,
            "roughness": roughness,
        }

    foam_web_modifiers = []
    for foam in (front_foam, rear_foam):
        modifier_name = "BVMCP_DGX_ThinFoamWeb_V3"
        modifier = foam.modifiers.get(modifier_name) or foam.modifiers.new(
            modifier_name, "DISPLACE"
        )
        modifier.direction = "NORMAL"
        modifier.strength = -0.12 * to_world
        modifier.mid_level = 0.0
        foam_web_modifiers.append(
            {
                "object": foam.name,
                "modifier": modifier.name,
                "normal_offset_mm": -0.12,
                "applied": False,
                "authority": "evaluated render/ray geometry; immutable source mesh retained",
            }
        )

    inset_geometry_rebuild = {}
    for object_name in ("tub", "tub.001"):
        obj = bpy.data.objects.get(object_name)
        if obj is None or obj.type != "MESH":
            continue
        previous_mesh = obj.data
        previous_materials = [
            material for material in previous_mesh.materials if material is not None
        ]
        previous_bounds = object_world_bounds(obj)
        minimum = previous_bounds["minimum"]
        maximum = previous_bounds["maximum"]
        center_x = (minimum[0] + maximum[0]) * 0.5
        center_z = (minimum[2] + maximum[2]) * 0.5
        half_width = (maximum[0] - minimum[0]) * 0.5
        half_height = (maximum[2] - minimum[2]) * 0.5
        replacement_mesh = _dgx_xz_rounded_prism_mesh(
            f"{object_name}-flat-inset-v15-mesh",
            center_x=center_x,
            center_z=center_z,
            half_width=half_width,
            half_height=half_height,
            radius=min(half_width, half_height),
            front_y=minimum[1],
            back_y=maximum[1],
            corner_segments=16,
        )
        obj.data = replacement_mesh
        for material in previous_materials:
            replacement_mesh.materials.append(material)
        obj.matrix_world = Matrix.Identity(4)
        if previous_mesh.users == 0:
            bpy.data.meshes.remove(previous_mesh)
        bpy.context.view_layer.update()
        rebuilt_bounds = object_world_bounds(obj)
        inset_geometry_rebuild[object_name] = {
            "bounds_before": previous_bounds,
            "bounds_after": rebuilt_bounds,
            "materials": [material.name for material in replacement_mesh.materials],
            "topology": _mesh_topology(obj),
            "reason": (
                "replace the inherited convex capsule with a watertight flat-front rounded "
                "prism at the exact existing bounds so the recessed champagne plate does not "
                "acquire a false central highlight"
            ),
        }

    removed_prior_generated = []
    for obj in list(bpy.context.scene.objects):
        if obj.name.startswith(
            (
                "bvmcp-dgx-v1-",
                "bvmcp-dgx-web-v4-",
                "bvmcp-dgx-web-v5-",
                "bvmcp-dgx-web-v6-",
                "bvmcp-dgx-web-v7-",
                "bvmcp-dgx-cellfield-v7-",
                "bvmcp-dgx-web-v8-",
                "bvmcp-dgx-cellfield-v8-",
                "bvmcp-dgx-web-v9-",
                "bvmcp-dgx-cellfield-v9-",
                "bvmcp-dgx-web-v10-",
                "bvmcp-dgx-cellfield-v10-",
                "bvmcp-dgx-web-v11-",
                "bvmcp-dgx-cellfield-v11-",
                "bvmcp-dgx-web-v12-",
                "bvmcp-dgx-cellfield-v12-",
                "bvmcp-dgx-web-v13-",
                "bvmcp-dgx-cellfield-v13-",
                "bvmcp-dgx-web-v14-",
                "bvmcp-dgx-cellfield-v14-",
                "bvmcp-dgx-web-v15-",
                "bvmcp-dgx-cellfield-v15-",
                "bvmcp-dgx-web-v16-",
                "bvmcp-dgx-cellfield-v16-",
            )
        ):
            removed_prior_generated.append(obj.name)
            mesh = obj.data if obj.type == "MESH" else None
            bpy.data.objects.remove(obj, do_unlink=True)
            if mesh is not None and mesh.users == 0:
                bpy.data.meshes.remove(mesh)
    removed_prior_meshes = []
    for mesh in list(bpy.data.meshes):
        if mesh.users == 0 and mesh.name.startswith(
            (
                "bvmcp-dgx-v1-",
                "bvmcp-dgx-web-v4-",
                "bvmcp-dgx-web-v5-",
                "bvmcp-dgx-web-v6-",
                "bvmcp-dgx-web-v7-",
                "bvmcp-dgx-cellfield-v7-",
                "bvmcp-dgx-web-v8-",
                "bvmcp-dgx-cellfield-v8-",
                "bvmcp-dgx-web-v9-",
                "bvmcp-dgx-cellfield-v9-",
                "bvmcp-dgx-web-v10-",
                "bvmcp-dgx-cellfield-v10-",
                "bvmcp-dgx-web-v11-",
                "bvmcp-dgx-cellfield-v11-",
                "bvmcp-dgx-web-v12-",
                "bvmcp-dgx-cellfield-v12-",
                "bvmcp-dgx-web-v13-",
                "bvmcp-dgx-cellfield-v13-",
                "bvmcp-dgx-web-v14-",
                "bvmcp-dgx-cellfield-v14-",
                "bvmcp-dgx-web-v15-",
                "bvmcp-dgx-cellfield-v15-",
                "bvmcp-dgx-web-v16-",
                "bvmcp-dgx-cellfield-v16-",
            )
        ):
            removed_prior_meshes.append(mesh.name)
            bpy.data.meshes.remove(mesh)
    removed_prior_materials = []
    for material_name in (
        "BVMCP_DGX_Fine_Web_LOD_V4",
        "BVMCP_DGX_Voronoi_Web_LOD_V5",
        "BVMCP_DGX_Foam_Surface_V6",
        "BVMCP_DGX_Foam_Mid_V6",
        "BVMCP_DGX_Foam_Deep_V6",
        "BVMCP_DGX_Physical_Web_V7",
        "BVMCP_DGX_Cellfield_Surface_V7",
        "BVMCP_DGX_Cellfield_Mid_V7",
        "BVMCP_DGX_Cellfield_Deep_V7",
        "BVMCP_DGX_Physical_Web_V8",
        "BVMCP_DGX_Cellfield_Surface_V8",
        "BVMCP_DGX_Cellfield_Mid_V8",
        "BVMCP_DGX_Cellfield_Deep_V8",
        "BVMCP_DGX_Cellfield_Edge_Transparent_V8",
        "BVMCP_DGX_Physical_Web_V9",
        "BVMCP_DGX_Cellfield_Surface_V9",
        "BVMCP_DGX_Cellfield_Mid_V9",
        "BVMCP_DGX_Cellfield_Deep_V9",
        "BVMCP_DGX_Cellfield_Edge_Transparent_V9",
        "BVMCP_DGX_Physical_Web_V10",
        "BVMCP_DGX_Cellfield_Surface_V10",
        "BVMCP_DGX_Cellfield_Mid_V10",
        "BVMCP_DGX_Cellfield_Deep_V10",
        "BVMCP_DGX_Cellfield_Edge_Transparent_V10",
        "BVMCP_DGX_Physical_Web_V11",
        "BVMCP_DGX_Cellfield_Surface_V11",
        "BVMCP_DGX_Cellfield_Mid_V11",
        "BVMCP_DGX_Cellfield_Deep_V11",
        "BVMCP_DGX_Cellfield_Edge_Transparent_V11",
        "BVMCP_DGX_Physical_Web_V12",
        "BVMCP_DGX_Cellfield_Surface_V12",
        "BVMCP_DGX_Cellfield_Mid_V12",
        "BVMCP_DGX_Cellfield_Deep_V12",
        "BVMCP_DGX_Cellfield_Edge_Transparent_V12",
        "BVMCP_DGX_Physical_Web_V13",
        "BVMCP_DGX_Cellfield_Surface_V13",
        "BVMCP_DGX_Cellfield_Mid_V13",
        "BVMCP_DGX_Cellfield_Deep_V13",
        "BVMCP_DGX_Cellfield_Edge_Transparent_V13",
        "BVMCP_DGX_Physical_Web_V14",
        "BVMCP_DGX_Cellfield_Surface_V14",
        "BVMCP_DGX_Cellfield_Mid_V14",
        "BVMCP_DGX_Cellfield_Deep_V14",
        "BVMCP_DGX_Cellfield_Edge_Transparent_V14",
        "BVMCP_DGX_Physical_Web_V15",
        "BVMCP_DGX_Cellfield_Surface_V15",
        "BVMCP_DGX_Cellfield_Mid_V15",
        "BVMCP_DGX_Cellfield_Deep_V15",
        "BVMCP_DGX_Cellfield_Edge_Transparent_V15",
        "BVMCP_DGX_Physical_Web_V16",
        "BVMCP_DGX_Cellfield_Surface_V16",
        "BVMCP_DGX_Cellfield_Mid_V16",
        "BVMCP_DGX_Cellfield_Deep_V16",
        "BVMCP_DGX_Cellfield_Edge_Transparent_V16",
    ):
        material = bpy.data.materials.get(material_name)
        if material is not None and material.users == 0:
            removed_prior_materials.append(material.name)
            bpy.data.materials.remove(material)
    removed_trademark_placeholders = []
    for object_name in ("spark-nv-eye", "spark-nv-word"):
        obj = bpy.data.objects.get(object_name)
        if obj is not None:
            removed_trademark_placeholders.append(obj.name)
            bpy.data.objects.remove(obj, do_unlink=True)
    obsolete_patterns = (
        "spark-power-button-face*",
        "spark-rear-port-face*",
        "spark-usbc-tongue*",
        "spark-hdmi-*",
        "spark-rj45-*",
        "spark-cage-lip*",
    )
    removed_obsolete = []
    for obj in list(bpy.context.scene.objects):
        if any(fnmatchcase(obj.name, pattern) for pattern in obsolete_patterns):
            removed_obsolete.append(obj.name)
            bpy.data.objects.remove(obj, do_unlink=True)

    plate_adjustment = {"applied": False, "z_scale": 1.0}
    if not bool(io_plate.get("bvmcp_v8_plate_height_corrected", False)):
        minimum_z = min(vertex.co.z for vertex in io_plate.data.vertices)
        maximum_z = max(vertex.co.z for vertex in io_plate.data.vertices)
        center_z = (minimum_z + maximum_z) * 0.5
        z_scale = 0.91
        for vertex in io_plate.data.vertices:
            vertex.co.z = center_z + (vertex.co.z - center_z) * z_scale
        io_plate.data.update()
        io_plate["bvmcp_v8_plate_height_corrected"] = True
        plate_adjustment = {"applied": True, "z_scale": z_scale}
    plate_material = _material("BVMCP_DGX_IO_Plate_V1", (0.080, 0.057, 0.039, 1.0), 0.56, 0.43)
    _dgx_assign_material(io_plate, plate_material)
    port_dark = _material(
        "BVMCP_DGX_Port_Dark_V1",
        (0.0004, 0.0004, 0.00035, 1.0),
        0.02,
        0.72,
        specular_ior_level=0.04,
    )
    port_metal = _material("BVMCP_DGX_Port_Metal_V1", (0.075, 0.068, 0.056, 1.0), 0.82, 0.32)
    port_tongue = _material("BVMCP_DGX_Port_Tongue_V1", (0.125, 0.095, 0.058, 1.0), 0.46, 0.42)
    contact_gold = _material("BVMCP_DGX_Contact_Gold_V1", (0.36, 0.22, 0.055, 1.0), 0.92, 0.24)
    cage_rail = _material(
        "BVMCP_DGX_Cage_Rail_V1",
        (0.004, 0.0045, 0.005, 1.0),
        0.38,
        0.56,
        specular_ior_level=0.08,
    )
    power_material = _material("BVMCP_DGX_Power_Button_V1", (0.095, 0.052, 0.020, 1.0), 0.50, 0.52)

    generated_objects = []
    generated_topology = {}
    foam_visual_lod = {}
    front_webs = []
    rear_webs = []
    for side, seed, exclusions_mm in (
        (
            "front",
            509104,
            [
                (-56.45, 25.25, 18.1, 21.0, 6.8),
                (56.45, 25.25, 16.6, 21.0, 6.8),
            ],
        ),
        (
            "rear",
            509105,
            [(0.0, 15.5, 64.2, 9.0, 7.8)],
        ),
    ):
        direction = -1.0 if side == "front" else 1.0
        physical_name = f"bvmcp-dgx-web-v16-{side}-physical"
        physical_mesh, physical_report = _dgx_voronoi_foam_web_lod_mesh(
            f"{physical_name}-mesh",
            scale_to_mm=scale_to_mm,
            front_y_mm=direction * 75.78,
            back_y_mm=direction * 74.76,
            seed=seed,
            exclusions_mm=exclusions_mm,
            pitch_mm=1.31,
            site_retention=0.88,
            edge_width_range_mm=(0.10, 0.18),
            node_radius_mm=0.115,
        )
        physical = bpy.data.objects.new(physical_name, physical_mesh)
        bpy.context.scene.collection.objects.link(physical)
        physical.data.materials.append(
            _material(
                "BVMCP_DGX_Physical_Web_V16",
                (0.035, 0.020, 0.007, 1.0),
                0.18,
                0.76,
            )
        )
        physical.hide_render = True
        physical["bvmcp_authority"] = "watertight physical aperture and ray-cast authority"
        generated_objects.append(physical)
        if side == "front":
            front_webs.append(physical)
        else:
            rear_webs.append(physical)

        visual_reports = {}
        for layer_index, (
            layer,
            depth_mm,
            pitch_mm,
            color,
            metallic,
            roughness,
            minimum_edge_distance,
            maximum_edge_distance,
        ) in enumerate(
            (
                (
                    "surface",
                    75.82,
                    1.10,
                    (0.055, 0.038, 0.022, 1.0),
                    0.68,
                    0.42,
                    0.078,
                    0.148,
                ),
                (
                    "mid",
                    75.46,
                    1.28,
                    (0.012, 0.008, 0.004, 1.0),
                    0.24,
                    0.70,
                    0.060,
                    0.118,
                ),
                (
                    "deep",
                    75.10,
                    1.45,
                    (0.0025, 0.0018, 0.0011, 1.0),
                    0.06,
                    0.86,
                    0.050,
                    0.098,
                ),
            )
        ):
            proxy_name = f"bvmcp-dgx-cellfield-v16-{side}-{layer}"
            proxy_mesh = _dgx_cellfield_proxy_mesh(
                f"{proxy_name}-mesh",
                scale_to_mm=scale_to_mm,
                front_y_mm=direction * depth_mm,
                back_y_mm=direction * (depth_mm - 0.025),
                exclusions_mm=exclusions_mm,
            )
            proxy = bpy.data.objects.new(proxy_name, proxy_mesh)
            bpy.context.scene.collection.objects.link(proxy)
            procedural_material, procedural_report = _dgx_procedural_foam_material(
                f"BVMCP_DGX_Cellfield_{layer.title()}_V16",
                color=color,
                metallic=metallic,
                roughness=roughness,
                coordinate_scale=scale_to_mm / pitch_mm,
                thickness_noise_scale=scale_to_mm / 4.8,
                minimum_edge_distance=minimum_edge_distance,
                maximum_edge_distance=maximum_edge_distance,
                seed=seed + layer_index * 1009,
            )
            proxy.data.materials.append(procedural_material)
            proxy.data.materials.append(
                _dgx_transparent_material("BVMCP_DGX_Cellfield_Edge_Transparent_V16")
            )
            for polygon in proxy.data.polygons:
                if abs(float(polygon.normal.y)) < 0.9:
                    polygon.material_index = 1
            proxy.hide_viewport = True
            proxy["bvmcp_authority"] = procedural_report["authority"]
            generated_objects.append(proxy)
            visual_reports[layer] = {
                **procedural_report,
                "nominal_pitch_mm": pitch_mm,
                "depth_from_surface_mm": 75.82 - depth_mm,
                "proxy_thickness_mm": 0.025,
                "geometry_exclusions_mm": [list(item) for item in exclusions_mm],
            }
        foam_visual_lod[side] = {
            "physical_authority": physical_report,
            "render_proxies": visual_reports,
        }
    if len(front_webs) != 1 or len(rear_webs) != 1:
        raise ValueError("DGX foam visual LOD generation failed")
    front_foam.hide_render = True
    front_foam.hide_viewport = True
    rear_foam.hide_render = True
    rear_foam.hide_viewport = True

    def add_rounded_prism(
        name: str,
        *,
        center_x_mm: float,
        center_z_mm: float,
        width_mm: float,
        height_mm: float,
        radius_mm: float,
        back_y_mm: float,
        front_y_mm: float,
        material,
    ):
        mesh = _dgx_xz_rounded_prism_mesh(
            f"{name}-mesh",
            center_x=center_x_mm * to_world,
            center_z=center_z_mm * to_world,
            half_width=width_mm * 0.5 * to_world,
            half_height=height_mm * 0.5 * to_world,
            radius=radius_mm * to_world,
            front_y=front_y_mm * to_world,
            back_y=back_y_mm * to_world,
        )
        obj = bpy.data.objects.new(name, mesh)
        bpy.context.scene.collection.objects.link(obj)
        obj.data.materials.append(material)
        generated_objects.append(obj)
        return obj

    add_rounded_prism(
        "bvmcp-dgx-v1-power-button",
        center_x_mm=60.0,
        center_z_mm=15.5,
        width_mm=2.8,
        height_mm=7.2,
        radius_mm=1.3,
        back_y_mm=76.55,
        front_y_mm=77.12,
        material=power_material,
    )
    usb_centers_mm = (51.5, 44.0, 36.5, 29.0)
    for index, center_x_mm in enumerate(usb_centers_mm):
        ring_name = f"bvmcp-dgx-v1-usbc-{index}-ring"
        ring_mesh = _rtx_rounded_rect_ring_mesh(
            f"{ring_name}-mesh",
            half_width=1.58 * to_world,
            half_height=4.58 * to_world,
            radius=1.52 * to_world,
            ring_width=0.34 * to_world,
            front_y=77.14 * to_world,
            back_y=76.54 * to_world,
        )
        ring = bpy.data.objects.new(ring_name, ring_mesh)
        bpy.context.scene.collection.objects.link(ring)
        ring.location.x = center_x_mm * to_world
        ring.location.z = 15.5 * to_world
        ring.data.materials.append(port_metal)
        generated_objects.append(ring)
        add_rounded_prism(
            f"bvmcp-dgx-v1-usbc-{index}-cavity",
            center_x_mm=center_x_mm,
            center_z_mm=15.5,
            width_mm=2.46,
            height_mm=8.15,
            radius_mm=1.18,
            back_y_mm=76.58,
            front_y_mm=77.18,
            material=port_dark,
        )
        add_rounded_prism(
            f"bvmcp-dgx-v1-usbc-{index}-tongue",
            center_x_mm=center_x_mm,
            center_z_mm=15.5,
            width_mm=0.38,
            height_mm=6.35,
            radius_mm=0.18,
            back_y_mm=77.18,
            front_y_mm=77.27,
            material=port_tongue,
        )

    hdmi_outer_points_mm = (
        (4.8, 18.75),
        (20.2, 18.75),
        (18.7, 12.25),
        (6.3, 12.25),
    )
    hdmi_inner_points_mm = (
        (5.35, 18.25),
        (19.65, 18.25),
        (18.25, 12.78),
        (6.75, 12.78),
    )
    for name, points_mm, back_y_mm, front_y_mm, material in (
        (
            "bvmcp-dgx-v1-hdmi-shield",
            hdmi_outer_points_mm,
            76.52,
            77.10,
            port_metal,
        ),
        (
            "bvmcp-dgx-v1-hdmi-cavity",
            hdmi_inner_points_mm,
            77.10,
            77.20,
            port_dark,
        ),
    ):
        mesh = _rtx_xz_prism_mesh(
            f"{name}-mesh",
            points=[(x * to_world, z * to_world) for x, z in points_mm],
            front_y=front_y_mm * to_world,
            back_y=back_y_mm * to_world,
        )
        obj = bpy.data.objects.new(name, mesh)
        bpy.context.scene.collection.objects.link(obj)
        obj.data.materials.append(material)
        generated_objects.append(obj)

    rj_ring_name = "bvmcp-dgx-v1-rj45-ring"
    rj_ring_mesh = _rtx_rounded_rect_ring_mesh(
        f"{rj_ring_name}-mesh",
        half_width=6.3 * to_world,
        half_height=5.6 * to_world,
        radius=1.10 * to_world,
        ring_width=0.48 * to_world,
        front_y=77.14 * to_world,
        back_y=76.52 * to_world,
    )
    rj_ring = bpy.data.objects.new(rj_ring_name, rj_ring_mesh)
    bpy.context.scene.collection.objects.link(rj_ring)
    rj_ring.location.x = -8.5 * to_world
    rj_ring.location.z = 15.5 * to_world
    rj_ring.data.materials.append(port_metal)
    generated_objects.append(rj_ring)
    add_rounded_prism(
        "bvmcp-dgx-v1-rj45-cavity",
        center_x_mm=-8.5,
        center_z_mm=15.5,
        width_mm=11.55,
        height_mm=10.15,
        radius_mm=0.65,
        back_y_mm=76.56,
        front_y_mm=77.19,
        material=port_dark,
    )
    add_rounded_prism(
        "bvmcp-dgx-v1-rj45-key",
        center_x_mm=-8.5,
        center_z_mm=20.0,
        width_mm=4.1,
        height_mm=1.8,
        radius_mm=0.35,
        back_y_mm=76.60,
        front_y_mm=77.22,
        material=port_dark,
    )
    rj_contact_boxes_mm = []
    for index in range(8):
        center_x_mm = -12.15 + index * 1.04
        rj_contact_boxes_mm.append(
            (
                center_x_mm - 0.22,
                77.19,
                11.2,
                center_x_mm + 0.22,
                77.30,
                13.9,
            )
        )
    contact_name = "bvmcp-dgx-v1-rj45-contacts"
    contact_mesh = _rtx_axis_aligned_boxes_mesh(
        f"{contact_name}-mesh",
        [tuple(value * to_world for value in box) for box in rj_contact_boxes_mm],
    )
    contacts = bpy.data.objects.new(contact_name, contact_mesh)
    bpy.context.scene.collection.objects.link(contacts)
    contacts.data.materials.append(contact_gold)
    generated_objects.append(contacts)

    cage_name = "bvmcp-dgx-v1-qsfp-shared-ring"
    cage_mesh = _rtx_rounded_rect_ring_mesh(
        f"{cage_name}-mesh",
        half_width=18.9 * to_world,
        half_height=6.1 * to_world,
        radius=1.35 * to_world,
        ring_width=0.52 * to_world,
        front_y=77.16 * to_world,
        back_y=76.50 * to_world,
    )
    cage = bpy.data.objects.new(cage_name, cage_mesh)
    bpy.context.scene.collection.objects.link(cage)
    cage.location.x = -41.0 * to_world
    cage.location.z = 15.5 * to_world
    cage.data.materials.append(port_metal)
    generated_objects.append(cage)
    add_rounded_prism(
        "bvmcp-dgx-v1-qsfp-divider",
        center_x_mm=-41.0,
        center_z_mm=15.5,
        width_mm=0.65,
        height_mm=11.0,
        radius_mm=0.18,
        back_y_mm=76.52,
        front_y_mm=77.24,
        material=port_metal,
    )
    for cage_index, center_x_mm in enumerate((-31.25, -50.75)):
        add_rounded_prism(
            f"bvmcp-dgx-v1-qsfp-{cage_index}-cavity",
            center_x_mm=center_x_mm,
            center_z_mm=15.5,
            width_mm=17.25,
            height_mm=10.8,
            radius_mm=0.55,
            back_y_mm=76.54,
            front_y_mm=77.20,
            material=port_dark,
        )
        rail_boxes_mm = []
        for center_z_mm in (12.2, 15.5, 18.8):
            rail_boxes_mm.append(
                (
                    center_x_mm - 7.6,
                    77.20,
                    center_z_mm - 0.30,
                    center_x_mm + 7.6,
                    77.31,
                    center_z_mm + 0.30,
                )
            )
        rails_name = f"bvmcp-dgx-v1-qsfp-{cage_index}-rails"
        rails_mesh = _rtx_axis_aligned_boxes_mesh(
            f"{rails_name}-mesh",
            [tuple(value * to_world for value in box) for box in rail_boxes_mm],
        )
        rails = bpy.data.objects.new(rails_name, rails_mesh)
        bpy.context.scene.collection.objects.link(rails)
        rails.data.materials.append(cage_rail)
        generated_objects.append(rails)

    for obj in generated_objects:
        topology = _mesh_topology(obj)
        if topology["non_manifold_edges"] != 0:
            raise ValueError(f"DGX generated topology failed for {obj.name}: {topology}")
        generated_topology[obj.name] = topology

    bpy.context.view_layer.update()
    depsgraph = bpy.context.evaluated_depsgraph_get()
    foam_ray_hits: dict[str, int] = {}
    for x_mm in range(-35, 36, 5):
        for z_mm in range(4, 48, 3):
            hit = bpy.context.scene.ray_cast(
                depsgraph,
                Vector((x_mm * to_world, -0.2, z_mm * to_world)),
                Vector((0.0, 1.0, 0.0)),
            )
            hit_name = hit[4].name if hit[0] else "MISS"
            foam_ray_hits[hit_name] = foam_ray_hits.get(hit_name, 0) + 1
    front_web_names = {web.name for web in front_webs}
    foam_strut_hits = sum(count for name, count in foam_ray_hits.items() if name in front_web_names)
    foam_open_hits = sum(count for name, count in foam_ray_hits.items() if name == body.name)
    if foam_strut_hits == 0 or foam_open_hits == 0:
        raise ValueError(
            "DGX front foam validation requires both physical strut and open-pore ray hits"
        )

    bpy.context.view_layer.update()
    body_after = object_world_bounds(body)
    body_after_dimensions_mm = [value * scale_to_mm for value in body_after["dimensions"]]
    for index, axis in enumerate(("x", "y", "z")):
        if abs(body_after_dimensions_mm[index] - targets_mm[index]) > 0.05:
            raise ValueError(f"DGX visual refinement regressed the body {axis} envelope")
    visible_meshes = [
        obj for obj in bpy.context.scene.objects if obj.type == "MESH" and not obj.hide_render
    ]
    visible_bounds = _combined_object_bounds(visible_meshes)
    bpy.context.scene["bvmcp_benchmark"] = "dgx_spark"
    bpy.context.scene["bvmcp_benchmark_revision"] = "visual-v16-front-foam-response"
    bpy.context.scene["bvmcp_source_revision"] = source_revision
    bpy.context.scene["bvmcp_candidate_accepted"] = False
    bpy.context.scene["bvmcp_reference_conflict"] = (
        "cl_side-profile is catalogued as top but depicts the underside/base-cover view; "
        "the governed source states that the true top is blank champagne"
    )
    checkpoint = save_checkpoint(root, parameters)
    return {
        **checkpoint,
        "benchmark": "dgx_spark",
        "revision": "visual-v16-front-foam-response",
        "source_revision": source_revision,
        "body_bounds_before": body_before,
        "body_bounds_after": body_after,
        "body_dimensions_mm": dict(zip(("x", "y", "z"), body_after_dimensions_mm, strict=True)),
        "dimension_checks": dimension_checks,
        "visible_scene_bounds": visible_bounds,
        "material_assignments": material_assignments,
        "inset_geometry_rebuild": inset_geometry_rebuild,
        "rear_io_plate_adjustment": plate_adjustment,
        "foam_web_modifiers": foam_web_modifiers,
        "foam_visual_lod": foam_visual_lod,
        "source_physical_foam_retained_hidden": [front_foam.name, rear_foam.name],
        "removed_trademark_placeholders": sorted(removed_trademark_placeholders),
        "removed_obsolete_rear_io": sorted(removed_obsolete),
        "removed_prior_generated": sorted(removed_prior_generated),
        "removed_prior_meshes": sorted(removed_prior_meshes),
        "removed_prior_materials": sorted(removed_prior_materials),
        "generated_rear_io": [obj.name for obj in generated_objects],
        "generated_topology": generated_topology,
        "foam_ray_validation": {
            "first_hit_counts": foam_ray_hits,
            "strut_hits": foam_strut_hits,
            "open_pore_hits": foam_open_hits,
            "physical_openness_observed": True,
        },
        "reference_conflicts": [
            {
                "reference_role": "top-teardown",
                "finding": "image depicts underside/base cover rather than the true top",
                "resolution": "retain blank true top; use image only as underside evidence",
            },
            {
                "reference_role": "bottom-review",
                "finding": (
                    "photographs show a champagne base cover while source prose calls it "
                    "dark plastic"
                ),
                "resolution": "visual candidate follows the two direct photographic observations",
            },
        ],
        "accepted": False,
        "reason": (
            "Automated DGX visual candidate remains pending reference residuals, feature/material "
            "coverage, repeatability, and named human acceptance."
        ),
    }


def refine_dgx_spark_base_foot_candidate(
    root: Path, parameters: dict[str, object]
) -> dict[str, object]:
    """Rebuild only the evidence-visible DGX recessed foot below the body envelope."""
    source_revision = str(parameters.get("source_revision", "")).strip()
    if not source_revision:
        raise ValueError("DGX Spark base-foot refinement requires a source revision")
    if bpy.context.scene.get("bvmcp_benchmark") != "dgx_spark":
        raise ValueError("scene is not a governed DGX Spark candidate")

    body = bpy.data.objects.get("dgx-spark")
    base = bpy.data.objects.get("spark-basecover")
    if body is None or body.type != "MESH" or base is None or base.type != "MESH":
        raise ValueError("DGX base-foot refinement requires dgx-spark and spark-basecover meshes")
    scale_to_mm = float(bpy.context.scene.unit_settings.scale_length) * 1000.0
    if scale_to_mm <= 0.0:
        raise ValueError("scene unit scale must be positive")

    body_before = object_world_bounds(body)
    body_dimensions_mm = [value * scale_to_mm for value in body_before["dimensions"]]
    body_targets_mm = [150.0, 150.0, 50.5]
    body_checks = {
        axis: {
            "actual_mm": body_dimensions_mm[index],
            "target_mm": body_targets_mm[index],
            "absolute_delta_mm": abs(body_dimensions_mm[index] - body_targets_mm[index]),
            "tolerance_mm": 0.05,
            "passed": abs(body_dimensions_mm[index] - body_targets_mm[index]) <= 0.05,
        }
        for index, axis in enumerate(("x", "y", "z"))
    }
    failed_body_axes = [axis for axis, check in body_checks.items() if not check["passed"]]
    if failed_body_axes:
        raise ValueError(
            "DGX body envelope fails source dimensions: " + ", ".join(failed_body_axes)
        )

    base_before = object_world_bounds(base)
    old_minimum = base_before["minimum"]
    old_maximum = base_before["maximum"]
    target_minimum_mm = (-56.5, -61.0, -3.25)
    target_maximum_mm = (56.5, 61.0, 0.75)
    target_minimum = tuple(value / scale_to_mm for value in target_minimum_mm)
    target_maximum = tuple(value / scale_to_mm for value in target_maximum_mm)
    inverse_world = base.matrix_world.inverted()

    def remap(
        value: float,
        source_minimum: float,
        source_maximum: float,
        target_index: int,
    ) -> float:
        span = source_maximum - source_minimum
        if span <= 0.0:
            raise ValueError("DGX base-foot source mesh has a degenerate axis")
        fraction = (value - source_minimum) / span
        return target_minimum[target_index] + fraction * (
            target_maximum[target_index] - target_minimum[target_index]
        )

    for vertex in base.data.vertices:
        world = base.matrix_world @ vertex.co
        rebuilt_world = Vector(
            tuple(
                remap(world[index], old_minimum[index], old_maximum[index], index)
                for index in range(3)
            )
        )
        vertex.co = inverse_world @ rebuilt_world
    base.data.update()
    bpy.context.view_layer.update()

    base_after = object_world_bounds(base)
    base_after_dimensions_mm = [value * scale_to_mm for value in base_after["dimensions"]]
    base_target_dimensions_mm = [
        target_maximum_mm[index] - target_minimum_mm[index] for index in range(3)
    ]
    base_checks = {
        axis: {
            "actual_mm": base_after_dimensions_mm[index],
            "target_mm": base_target_dimensions_mm[index],
            "absolute_delta_mm": abs(
                base_after_dimensions_mm[index] - base_target_dimensions_mm[index]
            ),
            "tolerance_mm": 0.05,
            "passed": abs(base_after_dimensions_mm[index] - base_target_dimensions_mm[index])
            <= 0.05,
        }
        for index, axis in enumerate(("x", "y", "z"))
    }
    failed_base_axes = [axis for axis, check in base_checks.items() if not check["passed"]]
    if failed_base_axes:
        raise ValueError(
            "DGX base-foot reconstruction failed target axes: " + ", ".join(failed_base_axes)
        )
    topology = _mesh_topology(base)
    if topology["connected_components"] != 1 or topology["non_manifold_edges"] != 0:
        raise ValueError(f"DGX base-foot topology failed: {topology}")

    body_after = object_world_bounds(body)
    body_after_dimensions_mm = [value * scale_to_mm for value in body_after["dimensions"]]
    for index, axis in enumerate(("x", "y", "z")):
        if abs(body_after_dimensions_mm[index] - body_targets_mm[index]) > 0.05:
            raise ValueError(f"DGX base-foot refinement regressed the body {axis} envelope")

    bpy.context.scene["bvmcp_benchmark_revision"] = "visual-v17-recessed-base-foot"
    bpy.context.scene["bvmcp_source_revision"] = source_revision
    bpy.context.scene["bvmcp_candidate_accepted"] = False
    checkpoint = save_checkpoint(root, parameters)
    return {
        **checkpoint,
        "benchmark": "dgx_spark",
        "revision": "visual-v17-recessed-base-foot",
        "source_revision": source_revision,
        "body_bounds_before": body_before,
        "body_bounds_after": body_after,
        "body_dimensions_mm": dict(zip(("x", "y", "z"), body_after_dimensions_mm, strict=True)),
        "body_dimension_checks": body_checks,
        "base_foot_reconstruction": {
            "object": base.name,
            "bounds_before": base_before,
            "bounds_after": base_after,
            "target_minimum_mm": list(target_minimum_mm),
            "target_maximum_mm": list(target_maximum_mm),
            "dimensions_mm": dict(zip(("x", "y", "z"), base_after_dimensions_mm, strict=True)),
            "dimension_checks": base_checks,
            "topology": topology,
            "evidence": {
                "reference_role": "rear-real-product",
                "body_mask_width_px": 1433,
                "foot_mask_width_px_at_upper_edge": 1077,
                "foot_visible_height_px": 31,
                "derived_foot_width_mm": 113.0,
                "derived_visible_height_mm": 3.25,
                "corroboration": ["bottom-review", "side-vertical-review"],
            },
        },
        "accepted": False,
        "reason": (
            "Base-foot v17 is restricted below the measured body envelope and remains pending "
            "fresh multi-view residuals and named human acceptance."
        ),
    }


def refine_rtx_5090_fe_visual_candidate(
    root: Path, parameters: dict[str, object]
) -> dict[str, object]:
    """Replace the historical near-solid fan rotors with governed seven-blade geometry."""
    source_revision = str(parameters.get("source_revision", "")).strip()
    if not source_revision:
        raise ValueError("RTX visual candidate refinement requires a source revision")
    if bpy.context.scene.get("bvmcp_benchmark") != "rtx_5090_fe":
        raise ValueError("scene is not a governed RTX 5090 FE candidate")
    scale_to_mm = float(bpy.context.scene.unit_settings.scale_length) * 1000.0
    rotor_objects = [
        obj
        for obj in bpy.context.scene.objects
        if obj.type == "MESH" and fnmatchcase(obj.name, "fan-rotor*")
    ]
    if len(rotor_objects) != 2:
        raise ValueError("RTX visual refinement requires exactly two fan rotors")
    fan_material = _material(
        "BVMCP_RTX_SevenBlade_Satin_V8",
        (0.010, 0.011, 0.014, 1.0),
        0.04,
        0.46,
    )
    replacements = []
    for rotor in sorted(rotor_objects, key=lambda item: item.name):
        before = object_world_bounds(rotor)
        center_x = (before["minimum"][0] + before["maximum"][0]) / 2.0
        center_z = (before["minimum"][2] + before["maximum"][2]) / 2.0
        old_mesh = rotor.data
        rotor.data = _rtx_fan_blade_mesh(
            f"{rotor.name}-strict-v4",
            center_x=center_x,
            center_z=center_z,
            scale_to_mm=scale_to_mm,
        )
        rotor.matrix_world = Matrix.Identity(4)
        rotor.data.materials.append(fan_material)
        if old_mesh.users == 0:
            bpy.data.meshes.remove(old_mesh)
        bpy.ops.object.select_all(action="DESELECT")
        rotor.select_set(True)
        bpy.context.view_layer.objects.active = rotor
        bevel = rotor.modifiers.new("BVMCP_RTX_BladeEdge_V8", "BEVEL")
        bevel.width = 0.35 / scale_to_mm
        bevel.segments = 3
        bevel.limit_method = "ANGLE"
        bpy.ops.object.modifier_apply(modifier=bevel.name)
        for polygon in rotor.data.polygons:
            polygon.use_smooth = True
        bpy.context.view_layer.update()
        topology = _mesh_topology(rotor)
        if topology["connected_components"] != 7 or topology["non_manifold_edges"] != 0:
            raise ValueError(f"replacement fan topology failed for {rotor.name}: {topology}")
        replacements.append(
            {
                "object": rotor.name,
                "before_bounds": before,
                "after_bounds": object_world_bounds(rotor),
                "topology": topology,
            }
        )

    well_objects = [
        obj
        for obj in bpy.context.scene.objects
        if obj.type == "MESH" and fnmatchcase(obj.name, "fan-well*")
    ]
    if len(well_objects) != 2:
        raise ValueError("RTX visual refinement requires exactly two fan wells")
    sorted_rotors = sorted(rotor_objects, key=lambda item: object_world_bounds(item)["minimum"][2])
    sorted_wells = sorted(well_objects, key=lambda item: object_world_bounds(item)["minimum"][2])
    cavity_adjustments = []
    for rotor, well in zip(sorted_rotors, sorted_wells, strict=True):
        rotor_bounds = object_world_bounds(rotor)
        before = object_world_bounds(well)
        target_front_y = rotor_bounds["maximum"][1] + 1.2 / scale_to_mm
        shift_y = target_front_y - before["minimum"][1]
        well.location.y += shift_y
        bpy.context.view_layer.update()
        cavity_adjustments.append(
            {
                "object": well.name,
                "paired_rotor": rotor.name,
                "before_bounds": before,
                "after_bounds": object_world_bounds(well),
                "clearance_behind_blade_mm": 1.2,
                "shift_y_mm": shift_y * scale_to_mm,
            }
        )
    for well in sorted_wells:
        well.hide_render = True
        well.hide_viewport = True

    cavity_back_objects = [
        obj
        for obj in bpy.context.scene.objects
        if obj.type == "MESH" and fnmatchcase(obj.name, "fe-recessback*")
    ]
    if len(cavity_back_objects) != 2:
        raise ValueError("RTX visual refinement requires exactly two cavity back objects")
    cavity_baffles = []
    for obj, center_z_mm in zip(
        sorted(
            cavity_back_objects,
            key=lambda item: object_world_bounds(item)["minimum"][2],
        ),
        (-80.5, 79.5),
        strict=True,
    ):
        before = object_world_bounds(obj)
        obj.hide_render = False
        obj.hide_viewport = False
        obj.location = (0.0, 4.0 / scale_to_mm, center_z_mm / scale_to_mm)
        obj.rotation_euler = (0.0, 0.0, 0.0)
        obj.dimensions = (121.0 / scale_to_mm, 1.0 / scale_to_mm, 138.0 / scale_to_mm)
        bpy.context.view_layer.update()
        bpy.ops.object.select_all(action="DESELECT")
        obj.select_set(True)
        bpy.context.view_layer.objects.active = obj
        bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
        cavity_baffles.append(
            {
                "object": obj.name,
                "before_bounds": before,
                "after_bounds": object_world_bounds(obj),
                "dimensions_mm": [121.0, 1.0, 138.0],
            }
        )

    obsolete_rear_trim = [
        obj
        for obj in bpy.context.scene.objects
        if fnmatchcase(obj.name, "fe-rear-infinity*")
        or fnmatchcase(obj.name, "fe-rear-x-node*")
        or fnmatchcase(obj.name, "fe-rear-bowtie-v*")
        or fnmatchcase(obj.name, "fe-rear-perimeter-v*")
    ]
    for obj in obsolete_rear_trim:
        obj.hide_render = True
        obj.hide_viewport = True
    obsolete_window_lips = [
        obj for obj in bpy.context.scene.objects if fnmatchcase(obj.name, "fe-rear-window-lip*")
    ]
    for obj in obsolete_window_lips:
        obj.hide_render = True
        obj.hide_viewport = True
    rear_panel_material = _material(
        "BVMCP_RTX_Rear_Bowtie_V12",
        (0.028, 0.030, 0.035, 1.0),
        0.65,
        0.52,
    )
    rear_trim_material = _material(
        "BVMCP_RTX_Rear_Trim_V12",
        (0.050, 0.053, 0.058, 1.0),
        0.82,
        0.38,
    )
    rear_panel_objects = []
    rear_panel_specs = (
        ("fe-rear-bowtie-v12-top", [(65.0, -60.0), (65.0, 60.0), (0.0, 0.0)]),
        (
            "fe-rear-bowtie-v12-bottom",
            [(-65.0, 60.0), (-65.0, -60.0), (0.0, 0.0)],
        ),
    )
    for name, points_mm in rear_panel_specs:
        mesh = _rtx_xz_prism_mesh(
            f"{name}-mesh",
            points=[(x / scale_to_mm, z / scale_to_mm) for x, z in points_mm],
            front_y=19.0 / scale_to_mm,
            back_y=18.0 / scale_to_mm,
        )
        obj = bpy.data.objects.new(name, mesh)
        bpy.context.scene.collection.objects.link(obj)
        obj.data.materials.append(rear_panel_material)
        rear_panel_objects.append(obj)

    rear_trim_objects = []
    rear_trim_endpoints_mm = (
        ((0.0, 0.0), (65.0, -60.0)),
        ((0.0, 0.0), (65.0, 60.0)),
        ((0.0, 0.0), (-65.0, -60.0)),
        ((0.0, 0.0), (-65.0, 60.0)),
    )
    for index, (start_mm, end_mm) in enumerate(rear_trim_endpoints_mm):
        points_mm = _rtx_strip_points(start_mm, end_mm, 2.4)
        name = f"fe-rear-infinity-v12-{index}"
        mesh = _rtx_xz_prism_mesh(
            f"{name}-mesh",
            points=[(x / scale_to_mm, z / scale_to_mm) for x, z in points_mm],
            front_y=19.8 / scale_to_mm,
            back_y=18.8 / scale_to_mm,
        )
        obj = bpy.data.objects.new(name, mesh)
        bpy.context.scene.collection.objects.link(obj)
        obj.data.materials.append(rear_trim_material)
        rear_trim_objects.append(obj)

    rear_perimeter_name = "fe-rear-perimeter-v12"
    rear_perimeter_mesh = _rtx_rounded_rect_ring_mesh(
        f"{rear_perimeter_name}-mesh",
        half_width=66.25 / scale_to_mm,
        half_height=150.0 / scale_to_mm,
        radius=18.0 / scale_to_mm,
        ring_width=2.4 / scale_to_mm,
        front_y=19.8 / scale_to_mm,
        back_y=18.8 / scale_to_mm,
    )
    rear_perimeter = bpy.data.objects.new(rear_perimeter_name, rear_perimeter_mesh)
    bpy.context.scene.collection.objects.link(rear_perimeter)
    rear_perimeter.data.materials.append(rear_trim_material)

    rear_bowtie_topology = {}
    for obj in rear_panel_objects + rear_trim_objects + [rear_perimeter]:
        topology = _mesh_topology(obj)
        if topology["connected_components"] != 1 or topology["non_manifold_edges"] != 0:
            raise ValueError(f"rear bowtie topology failed for {obj.name}: {topology}")
        rear_bowtie_topology[obj.name] = topology

    obsolete_gold_objects = [
        obj for obj in bpy.context.scene.objects if fnmatchcase(obj.name, "fe-goldfinger*")
    ]
    for obj in obsolete_gold_objects:
        obj.hide_render = True
        obj.hide_viewport = True
    extender_material = _material(
        "BVMCP_RTX_Extender_Substrate_V13",
        (0.015, 0.018, 0.016, 1.0),
        0.20,
        0.55,
    )
    extender_objects = [
        obj
        for obj in bpy.context.scene.objects
        if obj.type == "MESH"
        and (fnmatchcase(obj.name, "fe-extboard*") or fnmatchcase(obj.name, "fe-extpcb*"))
    ]
    for obj in extender_objects:
        obj.data.materials.clear()
        obj.data.materials.append(extender_material)
    gold_material = _material(
        "BVMCP_RTX_PCIe_Gold_V13",
        (0.42, 0.27, 0.075, 1.0),
        1.0,
        0.26,
    )
    contact_bank_objects = []
    contact_bank_counts = {}
    bank_ranges_mm = ((-112.0, -102.0, "short"), (-95.0, -9.0, "long"))
    for side, front_y_mm, back_y_mm in (
        ("front", 11.8, 11.4),
        ("rear", 19.4, 19.0),
    ):
        for minimum_z_mm, maximum_z_mm, bank_label in bank_ranges_mm:
            name = f"fe-goldfinger-v13-{side}-{bank_label}"
            mesh, contact_count = _rtx_contact_bank_mesh(
                f"{name}-mesh",
                minimum_x=70.2 / scale_to_mm,
                maximum_x=75.2 / scale_to_mm,
                minimum_z=minimum_z_mm / scale_to_mm,
                maximum_z=maximum_z_mm / scale_to_mm,
                contact_pitch=2.0 / scale_to_mm,
                contact_width=1.2 / scale_to_mm,
                front_y=front_y_mm / scale_to_mm,
                back_y=back_y_mm / scale_to_mm,
            )
            obj = bpy.data.objects.new(name, mesh)
            bpy.context.scene.collection.objects.link(obj)
            obj.data.materials.append(gold_material)
            topology = _mesh_topology(obj)
            if (
                topology["connected_components"] != contact_count
                or topology["non_manifold_edges"] != 0
            ):
                raise ValueError(f"PCIe contact topology failed for {name}: {topology}")
            contact_bank_objects.append(obj)
            contact_bank_counts[name] = contact_count

    edge_wall_material = _material(
        "BVMCP_RTX_Edge_Wall_V14",
        (0.018, 0.019, 0.021, 1.0),
        0.70,
        0.48,
    )
    edge_cover_material = _material(
        "BVMCP_RTX_Edge_Cover_V14",
        (0.012, 0.013, 0.015, 1.0),
        0.55,
        0.55,
    )
    power_header_material = _material(
        "BVMCP_RTX_Power_Header_V14",
        (0.006, 0.007, 0.009, 1.0),
        0.05,
        0.50,
    )
    edge_material_assignments = {}
    for object_name, material in (
        ("fe-edge-wall", edge_wall_material),
        ("fe-edge-cover", edge_cover_material),
        ("fe-pwr-pins", power_header_material),
    ):
        obj = bpy.data.objects.get(object_name)
        if obj is None or obj.type != "MESH":
            raise ValueError(f"RTX edge material target is missing: {object_name}")
        obj.data.materials.clear()
        obj.data.materials.append(material)
        edge_material_assignments[object_name] = material.name

    obsolete_power_objects = [
        obj
        for obj in bpy.context.scene.objects
        if fnmatchcase(obj.name, "fe-pwr-bay*") or fnmatchcase(obj.name, "fe-pwr-pins*")
    ]
    obsolete_power_names = [obj.name for obj in obsolete_power_objects]
    removed_prior_power_objects = []
    for obj in obsolete_power_objects:
        if "-v15" in obj.name:
            removed_prior_power_objects.append(obj.name)
            bpy.data.objects.remove(obj, do_unlink=True)
        else:
            obj.hide_render = True
            obj.hide_viewport = True
    power_frame_material = _material(
        "BVMCP_RTX_Power_Frame_V15",
        (0.018, 0.020, 0.024, 1.0),
        0.62,
        0.48,
    )
    power_recess_material = _material(
        "BVMCP_RTX_Power_Recess_V15",
        (0.0025, 0.0030, 0.0040, 1.0),
        0.02,
        0.70,
    )
    power_header_v15_material = _material(
        "BVMCP_RTX_Power_Header_V15",
        (0.008, 0.009, 0.011, 1.0),
        0.04,
        0.52,
    )
    power_cell_material = _material(
        "BVMCP_RTX_Power_Cell_V15",
        (0.001, 0.0012, 0.0015, 1.0),
        0.0,
        0.82,
    )
    power_frame_name = "fe-pwr-bay-frame-v16"
    power_frame_mesh = _rtx_yz_rect_ring_mesh(
        f"{power_frame_name}-mesh",
        outer_minimum_y=-12.0 / scale_to_mm,
        outer_maximum_y=12.0 / scale_to_mm,
        outer_minimum_z=40.0 / scale_to_mm,
        outer_maximum_z=64.0 / scale_to_mm,
        ring_width=3.0 / scale_to_mm,
        front_x=-68.5 / scale_to_mm,
        back_x=-66.7 / scale_to_mm,
    )
    power_frame = bpy.data.objects.new(power_frame_name, power_frame_mesh)
    bpy.context.scene.collection.objects.link(power_frame)
    power_frame.data.materials.append(power_frame_material)
    power_objects = [power_frame]
    for name, boxes_mm, material in (
        (
            "fe-pwr-bay-recess-v16",
            [(-66.6, -9.0, 43.0, -66.1, 9.0, 61.0)],
            power_recess_material,
        ),
        (
            "fe-pwr-pins-v16-header",
            [(-68.0, -9.0, 48.0, -67.3, 9.0, 56.0)],
            power_header_v15_material,
        ),
    ):
        mesh = _rtx_axis_aligned_boxes_mesh(
            f"{name}-mesh",
            [tuple(value / scale_to_mm for value in box) for box in boxes_mm],
        )
        obj = bpy.data.objects.new(name, mesh)
        bpy.context.scene.collection.objects.link(obj)
        obj.data.materials.append(material)
        power_objects.append(obj)
    power_cells_mm = []
    for row in range(8):
        center_y_mm = -7.7 + row * 2.2
        for column in range(2):
            center_z_mm = 50.0 + column * 4.0
            power_cells_mm.append(
                (
                    -68.15,
                    center_y_mm - 0.7,
                    center_z_mm - 1.1,
                    -67.95,
                    center_y_mm + 0.7,
                    center_z_mm + 1.1,
                )
            )
    power_cells_name = "fe-pwr-pins-v16-cells"
    power_cells_mesh = _rtx_axis_aligned_boxes_mesh(
        f"{power_cells_name}-mesh",
        [tuple(value / scale_to_mm for value in box) for box in power_cells_mm],
    )
    power_cells = bpy.data.objects.new(power_cells_name, power_cells_mesh)
    bpy.context.scene.collection.objects.link(power_cells)
    power_cells.data.materials.append(power_cell_material)
    power_objects.append(power_cells)
    power_topology = {}
    for obj in power_objects:
        topology = _mesh_topology(obj)
        if topology["non_manifold_edges"] != 0:
            raise ValueError(f"power bay topology failed for {obj.name}: {topology}")
        power_topology[obj.name] = topology

    shrouds = [
        obj for obj in bpy.context.scene.objects if obj.type == "MESH" and obj.name == "fe-shroud"
    ]
    if len(shrouds) != 1:
        raise ValueError("RTX visual refinement requires exactly one front shroud")
    shroud = shrouds[0]
    shroud_before = object_world_bounds(shroud)
    removed_broken_modifiers = []
    for modifier in list(shroud.modifiers):
        if modifier.type == "BOOLEAN" and modifier.object is None:
            removed_broken_modifiers.append(modifier.name)
            shroud.modifiers.remove(modifier)
    aperture_centers = []
    for index, rotor in enumerate(sorted_rotors):
        rotor_bounds = object_world_bounds(rotor)
        center_x = (rotor_bounds["minimum"][0] + rotor_bounds["maximum"][0]) / 2.0
        center_z = (rotor_bounds["minimum"][2] + rotor_bounds["maximum"][2]) / 2.0
        bpy.ops.mesh.primitive_cylinder_add(
            vertices=96,
            radius=58.5 / scale_to_mm,
            depth=80.0 / scale_to_mm,
            location=(
                center_x,
                (shroud_before["minimum"][1] + shroud_before["maximum"][1]) / 2.0,
                center_z,
            ),
            rotation=(math.radians(90.0), 0.0, 0.0),
        )
        cutter = bpy.context.active_object
        cutter.name = f"BVMCP_RTX_FanAperture_Cutter_{index}"
        bpy.ops.object.select_all(action="DESELECT")
        shroud.select_set(True)
        bpy.context.view_layer.objects.active = shroud
        modifier = shroud.modifiers.new(f"BVMCP_FanAperture_{index}", "BOOLEAN")
        modifier.operation = "DIFFERENCE"
        modifier.solver = "EXACT"
        modifier.object = cutter
        bpy.ops.object.modifier_apply(modifier=modifier.name)
        bpy.data.objects.remove(cutter, do_unlink=True)
        aperture_centers.append({"x": center_x, "z": center_z})

    liner_objects = [
        obj
        for obj in bpy.context.scene.objects
        if obj.type == "MESH" and fnmatchcase(obj.name, "fe-linr*")
    ]
    if len(liner_objects) != 2:
        raise ValueError("RTX visual refinement requires exactly two fan liners")
    liner_material = _material(
        "BVMCP_RTX_Open_Fan_Liner_V6",
        (0.009, 0.010, 0.012, 1.0),
        0.05,
        0.68,
    )
    liner_replacements = []
    for liner, rotor in zip(
        sorted(liner_objects, key=lambda item: object_world_bounds(item)["minimum"][2]),
        sorted_rotors,
        strict=True,
    ):
        before = object_world_bounds(liner)
        rotor_bounds = object_world_bounds(rotor)
        center_x = (rotor_bounds["minimum"][0] + rotor_bounds["maximum"][0]) / 2.0
        center_z = (rotor_bounds["minimum"][2] + rotor_bounds["maximum"][2]) / 2.0
        old_mesh = liner.data
        liner.data = _rtx_fan_liner_mesh(
            f"{liner.name}-strict-v6",
            center_x=center_x,
            center_z=center_z,
            front_y=shroud_before["minimum"][1] + 0.2 / scale_to_mm,
            back_y=shroud_before["maximum"][1],
            inner_radius=57.6 / scale_to_mm,
            outer_radius=59.0 / scale_to_mm,
        )
        liner.matrix_world = Matrix.Identity(4)
        liner.data.materials.append(liner_material)
        if old_mesh.users == 0:
            bpy.data.meshes.remove(old_mesh)
        for polygon in liner.data.polygons:
            polygon.use_smooth = True
        bpy.context.view_layer.update()
        topology = _mesh_topology(liner)
        if topology["connected_components"] != 1 or topology["non_manifold_edges"] != 0:
            raise ValueError(f"replacement fan liner topology failed for {liner.name}: {topology}")
        liner_replacements.append(
            {
                "object": liner.name,
                "before_bounds": before,
                "after_bounds": object_world_bounds(liner),
                "topology": topology,
            }
        )

    front_fin_objects = [
        obj
        for obj in bpy.context.scene.objects
        if obj.type == "MESH" and fnmatchcase(obj.name, "fan-hsfin*")
    ]
    if len(front_fin_objects) != 62:
        raise ValueError("RTX visual refinement requires exactly 62 front fan fins")
    front_fin_before = _combined_object_bounds(front_fin_objects)
    front_fin_groups = (
        [obj for obj in front_fin_objects if obj.location.z < 0.0],
        [obj for obj in front_fin_objects if obj.location.z >= 0.0],
    )
    front_fin_fields = []
    for group in front_fin_groups:
        if len(group) != 31:
            raise ValueError("each RTX front fan fin field must contain 31 fins")
        center_z = sum(obj.location.z for obj in group) / len(group)
        center_x = sum(obj.location.x for obj in group) / len(group)
        center_y = sum(obj.location.y for obj in group) / len(group)
        radius = 53.0 / scale_to_mm
        for index, obj in enumerate(sorted(group, key=lambda item: item.name)):
            offset_z = (2.0 * (index + 0.5) / len(group) - 1.0) * radius
            half_width = math.sqrt(max(radius * radius - offset_z * offset_z, 0.0))
            obj.location = (center_x, center_y, center_z + offset_z)
            obj.rotation_euler = (0.0, 0.0, 0.0)
            obj.dimensions = (2.0 * half_width, 21.0 / scale_to_mm, 0.45 / scale_to_mm)
            bpy.context.view_layer.update()
            bpy.ops.object.select_all(action="DESELECT")
            obj.select_set(True)
            bpy.context.view_layer.objects.active = obj
            bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
        front_fin_fields.append(
            {
                "center_x": center_x,
                "center_z": center_z,
                "fin_count": len(group),
                "circular_field_radius_mm": 53.0,
                "fin_thickness_mm": 0.45,
                "fin_depth_mm": 21.0,
            }
        )

    rear_fin_objects = [
        obj
        for obj in bpy.context.scene.objects
        if obj.type == "MESH" and fnmatchcase(obj.name, "fe-flowfin*")
    ]
    if len(rear_fin_objects) != 86:
        raise ValueError("RTX visual refinement requires exactly 86 rear flow-through fins")
    rear_fin_before = _combined_object_bounds(rear_fin_objects)
    rear_fin_groups = (
        [obj for obj in rear_fin_objects if obj.location.z < 0.0],
        [obj for obj in rear_fin_objects if obj.location.z >= 0.0],
    )
    rear_windows_mm = ((-148.5, -12.5), (10.5, 148.5))
    rear_fin_fields = []
    for group, (minimum_z_mm, maximum_z_mm) in zip(rear_fin_groups, rear_windows_mm, strict=True):
        if len(group) != 43:
            raise ValueError("each RTX rear fin field must contain 43 fins")
        center_x = sum(obj.location.x for obj in group) / len(group)
        center_y = sum(obj.location.y for obj in group) / len(group)
        for index, obj in enumerate(sorted(group, key=lambda item: item.name)):
            z_mm = minimum_z_mm + ((index + 0.5) / len(group) * (maximum_z_mm - minimum_z_mm))
            obj.location = (center_x, center_y, z_mm / scale_to_mm)
            obj.rotation_euler = (0.0, 0.0, 0.0)
            obj.dimensions = (121.0 / scale_to_mm, 12.5 / scale_to_mm, 0.42 / scale_to_mm)
            bpy.context.view_layer.update()
            bpy.ops.object.select_all(action="DESELECT")
            obj.select_set(True)
            bpy.context.view_layer.objects.active = obj
            bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
        rear_fin_fields.append(
            {
                "minimum_z_mm": minimum_z_mm,
                "maximum_z_mm": maximum_z_mm,
                "fin_count": len(group),
                "fin_span_x_mm": 121.0,
                "fin_thickness_mm": 0.42,
                "fin_depth_mm": 12.5,
            }
        )

    bpy.context.view_layer.update()
    depsgraph = bpy.context.evaluated_depsgraph_get()
    visibility_hits: dict[str, int] = {}
    for rotor in sorted_rotors:
        rotor_bounds = object_world_bounds(rotor)
        center_x = (rotor_bounds["minimum"][0] + rotor_bounds["maximum"][0]) / 2.0
        center_z = (rotor_bounds["minimum"][2] + rotor_bounds["maximum"][2]) / 2.0
        for x_mm in range(-50, 51, 5):
            for z_mm in range(-50, 51, 5):
                radius_mm = math.hypot(x_mm, z_mm)
                if not 18.0 <= radius_mm <= 52.0:
                    continue
                hit = bpy.context.scene.ray_cast(
                    depsgraph,
                    Vector(
                        (
                            center_x + x_mm / scale_to_mm,
                            -1.0,
                            center_z + z_mm / scale_to_mm,
                        )
                    ),
                    Vector((0.0, 1.0, 0.0)),
                )
                name = hit[4].name if hit[0] else "MISS"
                visibility_hits[name] = visibility_hits.get(name, 0) + 1
    blocked_hits = sum(
        count
        for name, count in visibility_hits.items()
        if name == shroud.name or fnmatchcase(name, "fe-linr*")
    )
    rotor_hits = sum(
        count for name, count in visibility_hits.items() if fnmatchcase(name, "fan-rotor*")
    )
    if blocked_hits:
        raise ValueError(
            f"front fan aperture remains occluded by shroud or liner ({blocked_hits} rays)"
        )
    if rotor_hits == 0:
        raise ValueError("front fan visibility verification found no rotor hits")

    center_trim = _material(
        "BVMCP_RTX_CenterTrim_V8",
        (0.020, 0.022, 0.026, 1.0),
        0.76,
        0.44,
    )
    trim_objects = []
    for obj in bpy.context.scene.objects:
        if obj.type == "MESH" and fnmatchcase(obj.name, "fe-front-infinity*"):
            obj.data.materials.clear()
            obj.data.materials.append(center_trim)
            trim_objects.append(obj.name)

    bpy.context.view_layer.update()
    body_objects = [
        obj
        for obj in bpy.context.scene.objects
        if obj.type == "MESH"
        and not obj.hide_render
        and _name_matches(obj.name, RTX_5090_FE_BODY_PATTERNS)
    ]
    body_bounds = _combined_object_bounds(body_objects)
    dimensions_mm = [value * scale_to_mm for value in body_bounds["dimensions"]]
    targets_mm = [137.0, 40.0, 304.0]
    checks = {
        axis: {
            "actual_mm": dimensions_mm[index],
            "target_mm": targets_mm[index],
            "passed": abs(dimensions_mm[index] - targets_mm[index]) <= 0.25,
        }
        for index, axis in enumerate(("x", "y", "z"))
    }
    failed = [axis for axis, check in checks.items() if not check["passed"]]
    if failed:
        raise ValueError("visual RTX candidate regressed axes: " + ", ".join(failed))
    bpy.context.scene["bvmcp_benchmark_revision"] = "strict-v16-audit-clean-power-frame"
    bpy.context.scene["bvmcp_source_revision"] = source_revision
    bpy.context.scene["bvmcp_candidate_accepted"] = False
    checkpoint = save_checkpoint(root, parameters)
    return {
        **checkpoint,
        "benchmark": "rtx_5090_fe",
        "revision": "strict-v16-audit-clean-power-frame",
        "source_revision": source_revision,
        "fan_geometry": {
            "blade_count": 7,
            "span_stations": 9,
            "root_angular_chord_degrees": 30.0,
            "maximum_angular_chord_degrees": 34.0,
            "tip_angular_chord_degrees": 26.0,
            "root_to_tip_sweep_degrees": 31.0,
            "minimum_inter_blade_gap_degrees": 360.0 / 7.0 - 34.0,
            "edge_bevel_mm": 0.35,
        },
        "fan_replacements": replacements,
        "fan_cavity_adjustments": cavity_adjustments,
        "fan_cavity_baffles": {
            "hidden_historical_wells": [obj.name for obj in sorted_wells],
            "planar_baffles": cavity_baffles,
        },
        "rear_bowtie_reconstruction": {
            "hidden_obsolete_objects": [obj.name for obj in obsolete_rear_trim],
            "hidden_window_lips": [obj.name for obj in obsolete_window_lips],
            "panel_objects": [obj.name for obj in rear_panel_objects],
            "trim_objects": [obj.name for obj in rear_trim_objects],
            "perimeter_object": rear_perimeter.name,
            "topology": rear_bowtie_topology,
            "panel_outer_endpoints_mm": {"x": [-65.0, 65.0], "z": [-60.0, 60.0]},
            "trim_width_mm": 2.4,
            "perimeter_outer_dimensions_mm": [132.5, 300.0],
        },
        "pcie_contact_reconstruction": {
            "hidden_continuous_objects": [obj.name for obj in obsolete_gold_objects],
            "contact_bank_objects": [obj.name for obj in contact_bank_objects],
            "contact_counts": contact_bank_counts,
            "extender_objects_reassigned": [obj.name for obj in extender_objects],
            "contact_pitch_mm": 2.0,
            "contact_width_mm": 1.2,
        },
        "top_edge_material_revision": {
            "assignments": edge_material_assignments,
            "geometry_unchanged": True,
        },
        "power_bay_reconstruction": {
            "superseded_objects": obsolete_power_names,
            "removed_prior_generated_objects": removed_prior_power_objects,
            "generated_objects": [obj.name for obj in power_objects],
            "cell_layout": [8, 2],
            "topology": power_topology,
            "outer_dimensions_mm": [24.0, 24.0],
            "recess_depth_mm": 1.9,
        },
        "fan_apertures": {
            "radius_mm": 58.5,
            "centers": aperture_centers,
            "removed_broken_modifiers": removed_broken_modifiers,
            "shroud_before_bounds": shroud_before,
            "shroud_after_bounds": object_world_bounds(shroud),
        },
        "fan_liner_replacements": liner_replacements,
        "front_visibility_verification": {
            "sample_hits": visibility_hits,
            "rotor_hits": rotor_hits,
            "blocked_shroud_or_liner_hits": blocked_hits,
        },
        "fin_field_corrections": {
            "front_before_bounds": front_fin_before,
            "front_after_bounds": _combined_object_bounds(front_fin_objects),
            "front_fields": front_fin_fields,
            "rear_before_bounds": rear_fin_before,
            "rear_after_bounds": _combined_object_bounds(rear_fin_objects),
            "rear_fields": rear_fin_fields,
            "reference_orientation": "fin edges vertical in horizontal-card review renders",
        },
        "center_trim_objects": sorted(trim_objects),
        "body_bounds": body_bounds,
        "dimensions_mm": dict(zip(("x", "y", "z"), dimensions_mm, strict=True)),
        "dimension_checks": checks,
        "accepted": False,
        "reason": (
            "Visual candidate requires fresh residuals, material review, and human acceptance."
        ),
    }


def refine_rtx_5090_fe_front_frame_candidate(
    root: Path, parameters: dict[str, object]
) -> dict[str, object]:
    """Reconstruct the evidence-visible RTX front X rails without changing its envelope."""
    source_revision = str(parameters.get("source_revision", "")).strip()
    if not source_revision:
        raise ValueError("RTX front-frame refinement requires a source revision")
    if bpy.context.scene.get("bvmcp_benchmark") != "rtx_5090_fe":
        raise ValueError("scene is not a governed RTX 5090 FE candidate")

    scale_to_mm = float(bpy.context.scene.unit_settings.scale_length) * 1000.0
    superseded_patterns = (
        "fe-front-infinity*",
        "fe-front-x-outer-v*",
        "fe-front-x-inset-v*",
        "fe-front-perimeter-v*",
    )
    superseded = [
        obj for obj in bpy.context.scene.objects if _name_matches(obj.name, superseded_patterns)
    ]
    for obj in superseded:
        obj.hide_render = True
        obj.hide_viewport = True

    outer_material = _material(
        "BVMCP_RTX_Front_X_Outer_V17",
        (0.026, 0.029, 0.034, 1.0),
        0.78,
        0.40,
    )
    inset_material = _material(
        "BVMCP_RTX_Front_X_Inset_V17",
        (0.010, 0.012, 0.016, 1.0),
        0.42,
        0.38,
    )
    front_endpoints_mm = (
        ((0.0, 0.0), (65.0, -60.0)),
        ((0.0, 0.0), (65.0, 60.0)),
        ((0.0, 0.0), (-65.0, -60.0)),
        ((0.0, 0.0), (-65.0, 60.0)),
    )
    outer_objects = []
    inset_objects = []
    for index, (start_mm, end_mm) in enumerate(front_endpoints_mm):
        outer_name = f"fe-front-x-outer-v17-{index}"
        outer_mesh = _rtx_xz_prism_mesh(
            f"{outer_name}-mesh",
            points=[
                (x / scale_to_mm, z / scale_to_mm)
                for x, z in _rtx_strip_points(start_mm, end_mm, 5.2)
            ],
            front_y=-20.0 / scale_to_mm,
            back_y=-18.4 / scale_to_mm,
        )
        outer = bpy.data.objects.new(outer_name, outer_mesh)
        bpy.context.scene.collection.objects.link(outer)
        outer.data.materials.append(outer_material)
        outer_objects.append(outer)

        inset_name = f"fe-front-x-inset-v17-{index}"
        inset_mesh = _rtx_xz_prism_mesh(
            f"{inset_name}-mesh",
            points=[
                (x / scale_to_mm, z / scale_to_mm)
                for x, z in _rtx_strip_points(start_mm, end_mm, 1.8)
            ],
            front_y=-20.01 / scale_to_mm,
            back_y=-19.99 / scale_to_mm,
        )
        inset = bpy.data.objects.new(inset_name, inset_mesh)
        bpy.context.scene.collection.objects.link(inset)
        inset.data.materials.append(inset_material)
        inset_objects.append(inset)

    perimeter_name = "fe-front-perimeter-v17"
    perimeter_mesh = _rtx_rounded_rect_ring_mesh(
        f"{perimeter_name}-mesh",
        half_width=68.0 / scale_to_mm,
        half_height=151.5 / scale_to_mm,
        radius=18.0 / scale_to_mm,
        ring_width=2.6 / scale_to_mm,
        front_y=-20.0 / scale_to_mm,
        back_y=-18.4 / scale_to_mm,
    )
    perimeter = bpy.data.objects.new(perimeter_name, perimeter_mesh)
    bpy.context.scene.collection.objects.link(perimeter)
    perimeter.data.materials.append(outer_material)

    topology = {}
    for obj in outer_objects + inset_objects + [perimeter]:
        result = _mesh_topology(obj)
        if result["connected_components"] != 1 or result["non_manifold_edges"] != 0:
            raise ValueError(f"RTX front-frame topology failed for {obj.name}: {result}")
        topology[obj.name] = result

    bpy.context.view_layer.update()
    body_objects = [
        obj
        for obj in bpy.context.scene.objects
        if obj.type == "MESH"
        and not obj.hide_render
        and _name_matches(obj.name, RTX_5090_FE_BODY_PATTERNS)
    ]
    body_bounds = _combined_object_bounds(body_objects)
    dimensions_mm = [value * scale_to_mm for value in body_bounds["dimensions"]]
    targets_mm = [137.0, 40.0, 304.0]
    checks = {
        axis: {
            "actual_mm": dimensions_mm[index],
            "target_mm": targets_mm[index],
            "passed": abs(dimensions_mm[index] - targets_mm[index]) <= 0.25,
        }
        for index, axis in enumerate(("x", "y", "z"))
    }
    failed = [axis for axis, check in checks.items() if not check["passed"]]
    if failed:
        raise ValueError("front-frame RTX candidate regressed axes: " + ", ".join(failed))

    bpy.context.scene["bvmcp_benchmark_revision"] = "strict-v17-front-x-frame"
    bpy.context.scene["bvmcp_source_revision"] = source_revision
    bpy.context.scene["bvmcp_candidate_accepted"] = False
    checkpoint = save_checkpoint(root, parameters)
    return {
        **checkpoint,
        "benchmark": "rtx_5090_fe",
        "revision": "strict-v17-front-x-frame",
        "source_revision": source_revision,
        "front_frame_reconstruction": {
            "superseded_objects": sorted(obj.name for obj in superseded),
            "outer_rail_objects": [obj.name for obj in outer_objects],
            "inset_rail_objects": [obj.name for obj in inset_objects],
            "perimeter_object": perimeter.name,
            "outer_rail_width_mm": 5.2,
            "inset_rail_width_mm": 1.8,
            "outer_endpoints_mm": {"x": [-65.0, 65.0], "z": [-60.0, 60.0]},
            "perimeter_outer_dimensions_mm": [136.0, 303.0],
            "topology": topology,
        },
        "body_bounds": body_bounds,
        "dimensions_mm": dict(zip(("x", "y", "z"), dimensions_mm, strict=True)),
        "dimension_checks": checks,
        "accepted": False,
        "reason": (
            "Front-frame candidate requires fresh residuals, material review, and human acceptance."
        ),
    }


def import_asset(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    source = confined(root, str(parameters["source_path"]), must_exist=True)
    extension = source.suffix.lower()
    for item in list(bpy.data.objects):
        bpy.data.objects.remove(item, do_unlink=True)
    before = set(bpy.data.objects)
    if extension in {".glb", ".gltf"}:
        bpy.ops.import_scene.gltf(filepath=str(source), import_pack_images=True)
    elif extension == ".obj":
        bpy.ops.wm.obj_import(filepath=str(source))
    elif extension == ".ply":
        bpy.ops.wm.ply_import(filepath=str(source))
    elif extension == ".stl":
        bpy.ops.wm.stl_import(filepath=str(source))
    else:
        raise ValueError("asset import supports only GLB, glTF, OBJ, PLY, and STL")
    imported = sorted(item.name for item in set(bpy.data.objects) - before)
    if not imported:
        raise ValueError("asset import produced no Blender objects")
    checkpoint = save_checkpoint(root, parameters)
    return {
        **checkpoint,
        "source_path": str(source.relative_to(root)),
        "imported_objects": imported,
    }


def create_component(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    component = parameters.get("component")
    if not isinstance(component, dict):
        raise ValueError("create_component requires one component record")
    return generate_components(root, {**parameters, "components": [component]})


def update_component(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    component = parameters.get("component")
    if not isinstance(component, dict) or not str(component.get("id", "")):
        raise ValueError("update_component requires one identified component record")
    component_id = str(component["id"])
    replaced = sorted(
        item.name
        for item in bpy.context.scene.objects
        if item.get("bvmcp_component_id") == component_id
    )
    if not replaced:
        raise ValueError(f"component does not exist in the scene: {component_id}")
    for name in replaced:
        bpy.data.objects.remove(bpy.data.objects[name], do_unlink=True)
    result = generate_components(root, {**parameters, "components": [component]})
    return {**result, "replaced_objects": replaced}


def apply_constraints(_root: Path, parameters: dict[str, object]) -> dict[str, object]:
    components = parameters.get("components")
    if not isinstance(components, list) or not components:
        raise ValueError("apply_constraints requires component records")
    checks = [
        check
        for component in components
        if isinstance(component, dict)
        for check in _validate_component_constraints(component)
    ]
    return {
        "constraint_checks": checks,
        "failed_constraints": [item for item in checks if item.get("passed") is False],
        "passed": not any(item.get("passed") is False for item in checks),
    }


def _create_camera_from_specification(specification: dict[str, object]):
    name = str(specification.get("name", f"BVMCP_Camera_{len(bpy.data.cameras) + 1:03d}"))
    if not name or len(name) > 128:
        raise ValueError("camera name is invalid")
    camera_data = bpy.data.cameras.new(name)
    camera = bpy.data.objects.new(name, camera_data)
    bpy.context.scene.collection.objects.link(camera)
    matrix = specification.get("world_from_camera")
    if matrix is not None:
        if (
            not isinstance(matrix, list)
            or len(matrix) != 4
            or not all(isinstance(row, list) and len(row) == 4 for row in matrix)
        ):
            raise ValueError("world_from_camera must be a 4x4 matrix")
        camera.matrix_world = Matrix(matrix)
    else:
        position = specification.get("position", [1.0, -1.0, 1.0])
        target = specification.get("target", [0.0, 0.0, 0.0])
        if not isinstance(position, list) or len(position) != 3:
            raise ValueError("camera position must have three values")
        if not isinstance(target, list) or len(target) != 3:
            raise ValueError("camera target must have three values")
        camera.location = [float(value) for value in position]
        point_camera(
            camera,
            Vector([float(value) for value in target]),
            float(specification.get("camera_roll_degrees", 0.0)),
        )
    camera_data.type = "PERSP"
    if "horizontal_fov_degrees" in specification:
        camera_data.angle = math.radians(float(specification["horizontal_fov_degrees"]))
        camera_data.lens_unit = "FOV"
    else:
        camera_data.lens = float(specification.get("lens_mm", 50.0))
        camera_data.sensor_width = float(specification.get("sensor_width_mm", 36.0))
    camera["bvmcp_reference_id"] = str(specification.get("reference_id", ""))
    return camera


def create_camera(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    specification = parameters.get("camera")
    if not isinstance(specification, dict):
        raise ValueError("create_camera requires one camera specification")
    camera = _create_camera_from_specification(specification)
    bpy.context.scene.camera = camera
    result: dict[str, object] = {
        "camera": camera.name,
        "world_from_camera": [list(row) for row in camera.matrix_world],
    }
    if parameters.get("output_path"):
        result["checkpoint"] = save_checkpoint(root, parameters)
    return result


def apply_camera_solution(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    cameras = parameters.get("cameras")
    if not isinstance(cameras, list) or not 1 <= len(cameras) <= 128:
        raise ValueError("apply_camera_solution requires between one and 128 cameras")
    created = [
        _create_camera_from_specification(item) for item in cameras if isinstance(item, dict)
    ]
    if len(created) != len(cameras):
        raise ValueError("camera solution entries must be JSON objects")
    bpy.context.scene.camera = created[0]
    result: dict[str, object] = {"cameras": [item.name for item in created]}
    if parameters.get("output_path"):
        result["checkpoint"] = save_checkpoint(root, parameters)
    return result


ASSET_PREPARATION_TARGET_FIELDS = {
    "name",
    "repair",
    "repair_merge_distance",
    "retopology_ratio",
    "uv",
    "material",
    "texture_bake",
    "texture_resolution",
    "rig",
    "animation",
    "lod_ratios",
    "collision",
}
ASSET_PREPARATION_MATERIAL_FIELDS = {
    "name",
    "material_class",
    "base_color",
    "metallic",
    "roughness",
    "transmission",
    "alpha",
    "emission_color",
    "emission_strength",
}


def _activate_object(obj) -> None:
    if bpy.context.object is not None and bpy.context.object.mode != "OBJECT":
        bpy.ops.object.mode_set(mode="OBJECT")
    bpy.ops.object.select_all(action="DESELECT")
    obj.hide_set(False)
    obj.select_set(True)
    bpy.context.view_layer.objects.active = obj


def _safe_asset_token(value: str) -> str:
    token = "".join(character if character.isalnum() else "_" for character in value)
    token = token.strip("_")
    if not token:
        raise ValueError("asset preparation name has no safe filename characters")
    return token[:96]


def _mesh_stage_facts(obj) -> dict[str, object]:
    topology = mesh_topology(obj.data)
    bounds = object_world_bounds(obj)
    return {
        "vertices": len(obj.data.vertices),
        "edges": len(obj.data.edges),
        "polygons": len(obj.data.polygons),
        "uv_layers": [layer.name for layer in obj.data.uv_layers],
        "material_slots": [
            slot.material.name if slot.material else None for slot in obj.material_slots
        ],
        "world_bounds": bounds,
        "topology": topology,
    }


def _maximum_bounds_delta(
    before: dict[str, list[float]], after: dict[str, list[float]]
) -> float:
    return max(
        abs(float(after[group][axis]) - float(before[group][axis]))
        for group in ("minimum", "maximum", "dimensions")
        for axis in range(3)
    )


def _repair_asset_mesh(obj, merge_distance: float) -> dict[str, object]:
    if not 0.0 < merge_distance <= 1e-4:
        raise ValueError("repair_merge_distance must be in (0, 1e-4]")
    before = _mesh_stage_facts(obj)
    graph = bmesh.new()
    graph.from_mesh(obj.data)
    before_degenerate = sum(face.calc_area() <= 1e-12 for face in graph.faces)
    before_non_manifold = sum(not edge.is_manifold for edge in graph.edges)
    bmesh.ops.remove_doubles(graph, verts=list(graph.verts), dist=merge_distance)
    bmesh.ops.dissolve_degenerate(
        graph,
        dist=max(merge_distance, 1e-12),
        edges=list(graph.edges),
    )
    if graph.faces:
        bmesh.ops.recalc_face_normals(graph, faces=list(graph.faces))
    after_degenerate = sum(face.calc_area() <= 1e-12 for face in graph.faces)
    after_non_manifold = sum(not edge.is_manifold for edge in graph.edges)
    graph.to_mesh(obj.data)
    graph.free()
    obj.data.update()
    after = _mesh_stage_facts(obj)
    if after_degenerate:
        raise ValueError(
            f"mesh repair left {after_degenerate} degenerate faces on {obj.name}"
        )
    return {
        "operation": "remove_doubles_dissolve_degenerate_recalculate_normals",
        "merge_distance": merge_distance,
        "before": before,
        "after": after,
        "degenerate_faces_before": before_degenerate,
        "degenerate_faces_after": after_degenerate,
        "non_manifold_edges_before": before_non_manifold,
        "non_manifold_edges_after": after_non_manifold,
        "maximum_world_bounds_delta": _maximum_bounds_delta(
            before["world_bounds"], after["world_bounds"]
        ),
    }


def _retopology_candidate(obj, ratio: float) -> dict[str, object]:
    if not 0.05 <= ratio <= 1.0:
        raise ValueError("retopology_ratio must be between 0.05 and 1.0")
    before = _mesh_stage_facts(obj)
    if ratio < 0.999999 and len(obj.data.polygons) >= 8:
        modifier = obj.modifiers.new("BVMCP_RetopologyDecimate", "DECIMATE")
        modifier.decimate_type = "COLLAPSE"
        modifier.ratio = ratio
        _activate_object(obj)
        bpy.ops.object.modifier_apply(modifier=modifier.name)
    after = _mesh_stage_facts(obj)
    if not after["polygons"]:
        raise ValueError(f"retopology removed every polygon from {obj.name}")
    return {
        "operation": "bounded_decimate_retopology_candidate",
        "requested_ratio": ratio,
        "realized_polygon_ratio": after["polygons"] / max(1, before["polygons"]),
        "before": before,
        "after": after,
        "maximum_world_bounds_delta": _maximum_bounds_delta(
            before["world_bounds"], after["world_bounds"]
        ),
        "deformation_ready_claim": False,
        "limitation": (
            "This is an executable topology-reduction candidate, not hand-authored "
            "all-quad deformation topology."
        ),
    }


def _unwrap_asset_mesh(obj) -> dict[str, object]:
    if not obj.data.polygons:
        raise ValueError(f"UV generation requires polygons: {obj.name}")
    layer = obj.data.uv_layers.get("BVMCP_UV0") or obj.data.uv_layers.new(name="BVMCP_UV0")
    obj.data.uv_layers.active = layer
    _activate_object(obj)
    bpy.ops.object.mode_set(mode="EDIT")
    bpy.ops.mesh.select_all(action="SELECT")
    result = bpy.ops.uv.smart_project(
        angle_limit=math.radians(66.0),
        island_margin=0.02,
        area_weight=0.0,
        correct_aspect=True,
        scale_to_bounds=True,
    )
    bpy.ops.object.mode_set(mode="OBJECT")
    if "FINISHED" not in result:
        raise RuntimeError(f"Blender smart-project UV generation failed for {obj.name}")
    active = obj.data.uv_layers.active
    coordinates = [tuple(float(value) for value in item.uv) for item in active.data]
    if not coordinates or not all(math.isfinite(value) for uv in coordinates for value in uv):
        raise ValueError(f"UV generation produced invalid coordinates for {obj.name}")
    minimum = [min(uv[axis] for uv in coordinates) for axis in range(2)]
    maximum = [max(uv[axis] for uv in coordinates) for axis in range(2)]
    return {
        "operation": "smart_project",
        "layer": active.name,
        "loop_count": len(coordinates),
        "finite": True,
        "minimum_uv": minimum,
        "maximum_uv": maximum,
        "within_unit_square": all(
            -1e-6 <= value <= 1.0 + 1e-6 for uv in coordinates for value in uv
        ),
    }


def _rgba(value: object, *, field: str) -> tuple[float, float, float, float]:
    if not isinstance(value, list) or len(value) not in {3, 4}:
        raise ValueError(f"{field} must be an RGB or RGBA list")
    channels = [float(channel) for channel in value]
    if len(channels) == 3:
        channels.append(1.0)
    if not all(0.0 <= channel <= 1.0 for channel in channels):
        raise ValueError(f"{field} channels must be between zero and one")
    return tuple(channels)


def _bounded_material_value(
    specification: dict[str, object], field: str, default: float
) -> float:
    value = float(specification.get(field, default))
    if not 0.0 <= value <= 1.0:
        raise ValueError(f"material {field} must be between zero and one")
    return value


def _create_asset_pbr_material(
    obj,
    specification: dict[str, object],
) -> tuple[object, dict[str, object]]:
    unknown = set(specification) - ASSET_PREPARATION_MATERIAL_FIELDS
    if unknown:
        raise ValueError(f"unsupported PBR material fields: {sorted(unknown)}")
    name = str(specification.get("name", f"BVMCP_{obj.name}_PBR"))
    if not name or len(name) > 128:
        raise ValueError("PBR material name is invalid")
    material_class = str(specification.get("material_class", "unspecified"))
    base_color = _rgba(
        specification.get("base_color", [0.5, 0.5, 0.5, 1.0]),
        field="base_color",
    )
    emission_color = _rgba(
        specification.get("emission_color", [0.0, 0.0, 0.0, 1.0]),
        field="emission_color",
    )
    metallic = _bounded_material_value(specification, "metallic", 0.0)
    roughness = _bounded_material_value(specification, "roughness", 0.5)
    transmission = _bounded_material_value(specification, "transmission", 0.0)
    alpha = _bounded_material_value(specification, "alpha", base_color[3])
    emission_strength = float(specification.get("emission_strength", 0.0))
    if not 0.0 <= emission_strength <= 1000.0:
        raise ValueError("material emission_strength must be between zero and 1000")
    material = bpy.data.materials.get(name) or bpy.data.materials.new(name)
    material.use_nodes = True
    tree = material.node_tree
    if tree is None:
        raise RuntimeError("Blender did not create a material node tree")
    principled = tree.nodes.get("Principled BSDF")
    if principled is None:
        principled = tree.nodes.new("ShaderNodeBsdfPrincipled")
        output = tree.nodes.get("Material Output") or tree.nodes.new("ShaderNodeOutputMaterial")
        tree.links.new(principled.outputs["BSDF"], output.inputs["Surface"])
    principled.inputs["Base Color"].default_value = base_color
    principled.inputs["Metallic"].default_value = metallic
    principled.inputs["Roughness"].default_value = roughness
    principled.inputs["Alpha"].default_value = alpha
    if "Transmission Weight" in principled.inputs:
        principled.inputs["Transmission Weight"].default_value = transmission
    if "Emission Color" in principled.inputs:
        principled.inputs["Emission Color"].default_value = emission_color
    elif "Emission" in principled.inputs:
        principled.inputs["Emission"].default_value = emission_color
    if "Emission Strength" in principled.inputs:
        principled.inputs["Emission Strength"].default_value = emission_strength
    material.diffuse_color = base_color
    material["bvmcp_material_class"] = material_class
    material["bvmcp_pbr_authority"] = "explicit_specification"
    obj.data.materials.clear()
    obj.data.materials.append(material)
    return material, {
        "operation": "principled_bsdf_material",
        "material": material.name,
        "material_class": material_class,
        "base_color": list(base_color),
        "metallic": metallic,
        "roughness": roughness,
        "transmission": transmission,
        "alpha": alpha,
        "emission_color": list(emission_color),
        "emission_strength": emission_strength,
        "node_type": principled.bl_idname,
    }


def _bake_asset_base_color(
    root: Path,
    obj,
    material,
    resolution: int,
) -> dict[str, object]:
    if not 32 <= resolution <= 4096 or resolution & (resolution - 1):
        raise ValueError("texture_resolution must be a power of two from 32 through 4096")
    if not obj.data.uv_layers.active:
        raise ValueError(f"texture baking requires an active UV layer: {obj.name}")
    texture_directory = root / "textures" / "generated"
    texture_directory.mkdir(parents=True, exist_ok=True)
    destination = texture_directory / f"{_safe_asset_token(obj.name)}_base_color.png"
    image = bpy.data.images.new(
        f"BVMCP_{obj.name}_BaseColor",
        width=resolution,
        height=resolution,
        alpha=True,
        float_buffer=False,
    )
    image.generated_color = (0.0, 0.0, 0.0, 1.0)
    tree = material.node_tree
    if tree is None:
        raise RuntimeError("texture bake material has no node tree")
    image_node = tree.nodes.new("ShaderNodeTexImage")
    image_node.name = "BVMCP_BakedBaseColor"
    image_node.label = "BVMCP Baked Base Color"
    image_node.image = image
    for node in tree.nodes:
        node.select = False
    image_node.select = True
    tree.nodes.active = image_node
    _activate_object(obj)
    scene = bpy.context.scene
    original_engine = scene.render.engine
    scene.render.engine = "CYCLES"
    scene.cycles.device = "CPU"
    original_samples = scene.cycles.samples
    scene.cycles.samples = 1
    try:
        result = bpy.ops.object.bake(
            type="DIFFUSE",
            pass_filter={"COLOR"},
            margin=max(2, resolution // 64),
            use_clear=True,
        )
    finally:
        scene.cycles.samples = original_samples
        scene.render.engine = original_engine
    if "FINISHED" not in result:
        raise RuntimeError(f"Blender texture bake failed for {obj.name}")
    image.filepath_raw = str(destination)
    image.file_format = "PNG"
    image.save()
    if not destination.is_file() or not destination.stat().st_size:
        raise RuntimeError(f"texture bake did not produce a PNG for {obj.name}")
    image.pack()
    principled = tree.nodes.get("Principled BSDF")
    if principled is not None:
        tree.links.new(image_node.outputs["Color"], principled.inputs["Base Color"])
    material["bvmcp_baked_base_color"] = str(destination.relative_to(root))
    return {
        "operation": "blender_diffuse_color_bake",
        "image": image.name,
        "path": str(destination.relative_to(root)),
        "sha256": hashlib.sha256(destination.read_bytes()).hexdigest(),
        "bytes": destination.stat().st_size,
        "resolution": [resolution, resolution],
        "uv_layer": obj.data.uv_layers.active.name,
        "packed_in_blend": bool(image.packed_file),
        "network_used": False,
    }


def _create_character_lite_rig(obj, frame_start: int, frame_end: int) -> dict[str, object]:
    if frame_start < 0 or frame_end <= frame_start or frame_end - frame_start > 10_000:
        raise ValueError("rig frame range is invalid or unbounded")
    if not obj.data.vertices:
        raise ValueError("character-lite rig requires a non-empty mesh")
    z_values = [float(vertex.co.z) for vertex in obj.data.vertices]
    minimum_z = min(z_values)
    maximum_z = max(z_values)
    if maximum_z - minimum_z <= 1e-6:
        raise ValueError("character-lite rig requires vertical mesh extent")
    midpoint = (minimum_z + maximum_z) / 2.0
    armature_data = bpy.data.armatures.new(f"{obj.name}_RigData")
    armature = bpy.data.objects.new(f"{obj.name}_Rig", armature_data)
    bpy.context.scene.collection.objects.link(armature)
    armature.matrix_world = obj.matrix_world.copy()
    armature.show_in_front = True
    _activate_object(armature)
    bpy.ops.object.mode_set(mode="EDIT")
    root_bone = armature_data.edit_bones.new("root")
    root_bone.head = (0.0, 0.0, minimum_z)
    root_bone.tail = (0.0, 0.0, midpoint)
    upper_bone = armature_data.edit_bones.new("upper")
    upper_bone.head = root_bone.tail
    upper_bone.tail = (0.0, 0.0, maximum_z)
    upper_bone.parent = root_bone
    upper_bone.use_connect = True
    bpy.ops.object.mode_set(mode="OBJECT")
    root_group = obj.vertex_groups.new(name="root")
    upper_group = obj.vertex_groups.new(name="upper")
    lower_indices = [
        vertex.index for vertex in obj.data.vertices if float(vertex.co.z) <= midpoint
    ]
    upper_indices = [
        vertex.index for vertex in obj.data.vertices if float(vertex.co.z) > midpoint
    ]
    if lower_indices:
        root_group.add(lower_indices, 1.0, "REPLACE")
    if upper_indices:
        upper_group.add(upper_indices, 1.0, "REPLACE")
    modifier = obj.modifiers.new("BVMCP_Armature", "ARMATURE")
    modifier.object = armature
    world_matrix = obj.matrix_world.copy()
    obj.parent = armature
    obj.matrix_world = world_matrix
    scene = bpy.context.scene
    scene.frame_start = min(scene.frame_start, frame_start)
    scene.frame_end = max(scene.frame_end, frame_end)
    upper_pose = armature.pose.bones["upper"]
    upper_pose.rotation_mode = "XYZ"
    middle_frame = frame_start + (frame_end - frame_start) // 2
    for frame, angle in (
        (frame_start, 0.0),
        (middle_frame, math.radians(18.0)),
        (frame_end, 0.0),
    ):
        upper_pose.rotation_euler[1] = angle
        upper_pose.keyframe_insert(data_path="rotation_euler", index=1, frame=frame)
    if armature.animation_data and armature.animation_data.action:
        armature.animation_data.action.name = f"{obj.name}_CharacterLite_Action"
    armature["bvmcp_rig_kind"] = "character_lite_two_bone"
    obj["bvmcp_character_lite_rig"] = armature.name
    return {
        "operation": "character_lite_two_bone_rig",
        "armature": armature.name,
        "bones": ["root", "upper"],
        "vertex_groups": ["root", "upper"],
        "weighted_vertices": {
            "root": len(lower_indices),
            "upper": len(upper_indices),
        },
        "animation": {
            "kind": "pose_bone",
            "frame_start": frame_start,
            "frame_end": frame_end,
            "keyframes": [frame_start, middle_frame, frame_end],
            "action": (
                armature.animation_data.action.name
                if armature.animation_data and armature.animation_data.action
                else None
            ),
        },
    }


def _create_object_animation(
    obj, frame_start: int, frame_end: int, rotation_degrees: float
) -> dict[str, object]:
    if frame_start < 0 or frame_end <= frame_start or frame_end - frame_start > 10_000:
        raise ValueError("object animation frame range is invalid or unbounded")
    if not -360.0 <= rotation_degrees <= 360.0:
        raise ValueError("object animation rotation_degrees is out of bounds")
    scene = bpy.context.scene
    scene.frame_start = min(scene.frame_start, frame_start)
    scene.frame_end = max(scene.frame_end, frame_end)
    original = float(obj.rotation_euler.z)
    middle_frame = frame_start + (frame_end - frame_start) // 2
    for frame, angle in (
        (frame_start, original),
        (middle_frame, original + math.radians(rotation_degrees)),
        (frame_end, original),
    ):
        obj.rotation_euler.z = angle
        obj.keyframe_insert(data_path="rotation_euler", index=2, frame=frame)
    if obj.animation_data and obj.animation_data.action:
        obj.animation_data.action.name = f"{obj.name}_Object_Action"
    obj["bvmcp_object_animation"] = "bounded_rotation"
    return {
        "operation": "bounded_object_rotation",
        "object": obj.name,
        "frame_start": frame_start,
        "frame_end": frame_end,
        "keyframes": [frame_start, middle_frame, frame_end],
        "rotation_degrees": rotation_degrees,
        "action": (
            obj.animation_data.action.name
            if obj.animation_data and obj.animation_data.action
            else None
        ),
    }


def _create_collision_hull(obj, collection) -> dict[str, object]:
    source_bounds = object_world_bounds(obj)
    collision = obj.copy()
    collision.data = obj.data.copy()
    collision.animation_data_clear()
    collision.name = f"UCX_{obj.name}_00"
    collection.objects.link(collision)
    world_matrix = collision.matrix_world.copy()
    collision.parent = None
    collision.matrix_world = world_matrix
    for modifier in list(collision.modifiers):
        collision.modifiers.remove(modifier)
    collision.data.materials.clear()
    _activate_object(collision)
    bpy.ops.object.mode_set(mode="EDIT")
    bpy.ops.mesh.select_all(action="SELECT")
    result = bpy.ops.mesh.convex_hull(
        delete_unused=True,
        use_existing_faces=False,
        make_holes=False,
        join_triangles=True,
    )
    bpy.ops.object.mode_set(mode="OBJECT")
    if "FINISHED" not in result or not collision.data.polygons:
        raise RuntimeError(f"convex collision generation failed for {obj.name}")
    collision.hide_render = True
    collision.display_type = "WIRE"
    collision["bvmcp_collision_source"] = obj.name
    collision["bvmcp_collision_kind"] = "convex_hull"
    collision_bounds = object_world_bounds(collision)
    return {
        "operation": "convex_hull",
        "source": obj.name,
        "object": collision.name,
        "vertices": len(collision.data.vertices),
        "polygons": len(collision.data.polygons),
        "source_world_bounds": source_bounds,
        "collision_world_bounds": collision_bounds,
        "maximum_world_bounds_delta": _maximum_bounds_delta(
            source_bounds, collision_bounds
        ),
        "renderable": False,
    }


def _create_asset_lods(obj, ratios: list[object], collection) -> list[dict[str, object]]:
    if len(ratios) > 4:
        raise ValueError("at most four LOD ratios may be requested per object")
    generated = []
    previous = 1.0
    for raw_ratio in ratios:
        ratio = float(raw_ratio)
        if not 0.01 <= ratio < previous:
            raise ValueError("LOD ratios must be strictly descending values in [0.01, 1)")
        previous = ratio
        item = obj.copy()
        item.data = obj.data.copy()
        item.animation_data_clear()
        item.name = f"{obj.name}_LOD_{ratio:.3f}"
        collection.objects.link(item)
        modifier = item.modifiers.new("BVMCP_LOD_Decimate", "DECIMATE")
        modifier.decimate_type = "COLLAPSE"
        modifier.ratio = ratio
        _activate_object(item)
        bpy.ops.object.modifier_move_to_index(modifier=modifier.name, index=0)
        bpy.ops.object.modifier_apply(modifier=modifier.name)
        item.hide_render = True
        item["bvmcp_lod_source"] = obj.name
        item["bvmcp_lod_ratio"] = ratio
        generated.append(
            {
                "operation": "decimate_lod",
                "source": obj.name,
                "object": item.name,
                "requested_ratio": ratio,
                "vertices": len(item.data.vertices),
                "polygons": len(item.data.polygons),
                "renderable": False,
            }
        )
    return generated


def _target_animation_specification(value: object) -> dict[str, object]:
    if value is None or value is False:
        return {}
    if value is True:
        return {
            "kind": "object",
            "frame_start": 1,
            "frame_end": 48,
            "rotation_degrees": 30.0,
        }
    if not isinstance(value, dict):
        raise ValueError("animation must be false, true, or an object")
    allowed = {"kind", "frame_start", "frame_end", "rotation_degrees"}
    unknown = set(value) - allowed
    if unknown:
        raise ValueError(f"unsupported animation fields: {sorted(unknown)}")
    kind = str(value.get("kind", "object"))
    if kind != "object":
        raise ValueError("asset preparation animation supports only kind=object")
    return {
        "kind": kind,
        "frame_start": int(value.get("frame_start", 1)),
        "frame_end": int(value.get("frame_end", 48)),
        "rotation_degrees": float(value.get("rotation_degrees", 30.0)),
    }


def _target_rig_specification(value: object) -> dict[str, object]:
    if value is None or value is False:
        return {}
    if value is True:
        return {"kind": "character_lite", "frame_start": 1, "frame_end": 48}
    if not isinstance(value, dict):
        raise ValueError("rig must be false, true, or an object")
    allowed = {"kind", "frame_start", "frame_end"}
    unknown = set(value) - allowed
    if unknown:
        raise ValueError(f"unsupported rig fields: {sorted(unknown)}")
    kind = str(value.get("kind", "character_lite"))
    if kind != "character_lite":
        raise ValueError("asset preparation rig supports only kind=character_lite")
    return {
        "kind": kind,
        "frame_start": int(value.get("frame_start", 1)),
        "frame_end": int(value.get("frame_end", 48)),
    }


def prepare_asset(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    """Execute a bounded, non-authoritative production-preparation transaction."""
    allowed_parameters = {"output_path", "glb_output_path", "targets"}
    unknown_parameters = set(parameters) - allowed_parameters
    if unknown_parameters:
        raise ValueError(
            f"unsupported asset preparation parameters: {sorted(unknown_parameters)}"
        )
    targets = parameters.get("targets")
    if not isinstance(targets, list) or not 1 <= len(targets) <= 32:
        raise ValueError("asset preparation requires between one and 32 target records")
    output = confined(root, str(parameters["output_path"]))
    glb_output = confined(root, str(parameters["glb_output_path"]))
    if output.suffix.lower() != ".blend":
        raise ValueError("asset preparation output_path must use .blend")
    if glb_output.suffix.lower() != ".glb":
        raise ValueError("asset preparation glb_output_path must use .glb")
    output.parent.mkdir(parents=True, exist_ok=True)
    glb_output.parent.mkdir(parents=True, exist_ok=True)
    names = []
    for target in targets:
        if not isinstance(target, dict):
            raise ValueError("asset preparation target records must be JSON objects")
        unknown = set(target) - ASSET_PREPARATION_TARGET_FIELDS
        if unknown:
            raise ValueError(f"unsupported asset target fields: {sorted(unknown)}")
        name = str(target.get("name", ""))
        if not name or name in names:
            raise ValueError("asset preparation target names must be unique and non-empty")
        names.append(name)
    missing = [
        name
        for name in names
        if bpy.data.objects.get(name) is None or bpy.data.objects[name].type != "MESH"
    ]
    if missing:
        raise ValueError(f"asset preparation mesh targets were not found: {missing}")

    prepared_collection = bpy.data.collections.get("BVMCP_PREPARED")
    if prepared_collection is None:
        prepared_collection = bpy.data.collections.new("BVMCP_PREPARED")
        bpy.context.scene.collection.children.link(prepared_collection)
    lod_collection = bpy.data.collections.get("BVMCP_LOD")
    if lod_collection is None:
        lod_collection = bpy.data.collections.new("BVMCP_LOD")
        bpy.context.scene.collection.children.link(lod_collection)
    collision_collection = bpy.data.collections.get("BVMCP_COLLISION")
    if collision_collection is None:
        collision_collection = bpy.data.collections.new("BVMCP_COLLISION")
        bpy.context.scene.collection.children.link(collision_collection)

    reports = []
    capability_receipts: dict[str, list[str]] = {}
    for target in targets:
        source = bpy.data.objects[str(target["name"])]
        prepared = source.copy()
        prepared.data = source.data.copy()
        prepared.animation_data_clear()
        prepared.name = f"{source.name}_Prepared"
        prepared_collection.objects.link(prepared)
        prepared["bvmcp_preparation_source"] = source.name
        prepared["bvmcp_component_id"] = (
            source.get("bvmcp_component_id") or source.name
        )
        source.hide_render = True
        source["bvmcp_superseded_for_render_by"] = prepared.name
        stages: dict[str, object] = {}

        if bool(target.get("repair", False)):
            stages["mesh_repair"] = _repair_asset_mesh(
                prepared,
                float(target.get("repair_merge_distance", 1e-6)),
            )
            capability_receipts.setdefault("mesh_repair", []).append(prepared.name)
        if "retopology_ratio" in target:
            stages["retopology"] = _retopology_candidate(
                prepared, float(target["retopology_ratio"])
            )
            capability_receipts.setdefault("retopology", []).append(prepared.name)
        if bool(target.get("uv", False)) or bool(target.get("texture_bake", False)):
            stages["uv_generation"] = _unwrap_asset_mesh(prepared)
            capability_receipts.setdefault("uv_generation", []).append(prepared.name)
        material_specification = target.get("material")
        if material_specification is not None:
            if not isinstance(material_specification, dict):
                raise ValueError("asset material must be a JSON object")
            material, material_report = _create_asset_pbr_material(
                prepared, material_specification
            )
            stages["pbr_material_generation"] = material_report
            capability_receipts.setdefault("pbr_material_generation", []).append(
                prepared.name
            )
        else:
            material = prepared.active_material
        if bool(target.get("texture_bake", False)):
            if material is None:
                raise ValueError(f"texture baking requires a material: {prepared.name}")
            stages["texture_baking"] = _bake_asset_base_color(
                root,
                prepared,
                material,
                int(target.get("texture_resolution", 256)),
            )
            capability_receipts.setdefault("texture_projection_and_baking", []).append(
                prepared.name
            )
        animation = _target_animation_specification(target.get("animation"))
        if animation:
            stages["object_animation"] = _create_object_animation(
                prepared,
                int(animation["frame_start"]),
                int(animation["frame_end"]),
                float(animation["rotation_degrees"]),
            )
            capability_receipts.setdefault("object_animation", []).append(prepared.name)
        rig = _target_rig_specification(target.get("rig"))
        if rig:
            stages["rigging"] = _create_character_lite_rig(
                prepared,
                int(rig["frame_start"]),
                int(rig["frame_end"]),
            )
            capability_receipts.setdefault("rigging", []).append(prepared.name)
            capability_receipts.setdefault("character_lite_animation", []).append(
                prepared.name
            )
        lods = target.get("lod_ratios", [])
        if not isinstance(lods, list):
            raise ValueError("lod_ratios must be a list")
        if lods:
            stages["lod_generation"] = _create_asset_lods(
                prepared, lods, lod_collection
            )
            capability_receipts.setdefault("lod_generation", []).append(prepared.name)
        if bool(target.get("collision", False)):
            stages["collision_generation"] = _create_collision_hull(
                prepared, collision_collection
            )
            capability_receipts.setdefault("collision_generation", []).append(
                prepared.name
            )
        reports.append(
            {
                "source": source.name,
                "prepared": prepared.name,
                "stages": stages,
                "final": _mesh_stage_facts(prepared),
            }
        )

    bpy.context.scene["bvmcp_asset_preparation"] = "candidate_v1"
    bpy.context.scene["bvmcp_candidate_accepted"] = False
    bpy.context.scene["bvmcp_preparation_capabilities"] = json.dumps(
        sorted(capability_receipts)
    )
    checkpoint = save_checkpoint(root, {"output_path": str(output)})
    bpy.ops.export_scene.gltf(
        filepath=str(glb_output),
        export_format="GLB",
        export_apply=True,
        use_renderable=True,
        export_yup=True,
        export_cameras=False,
        export_lights=False,
        export_animations=True,
    )
    if not glb_output.is_file() or not glb_output.stat().st_size:
        raise RuntimeError("asset preparation GLB export produced no bytes")
    return {
        **checkpoint,
        "glb_path": str(glb_output.relative_to(root)),
        "glb_sha256": hashlib.sha256(glb_output.read_bytes()).hexdigest(),
        "glb_size": glb_output.stat().st_size,
        "targets": reports,
        "capability_receipts": capability_receipts,
        "required_prepared_nodes": [report["prepared"] for report in reports],
        "candidate_only": True,
        "accepted": False,
        "network_used": False,
        "authority": (
            "Exact Blender execution receipts prove the requested production operations; "
            "visual, deformation, and application-specific acceptance remain separate gates."
        ),
    }


def _apply_fixture_scale(obj) -> None:
    _activate_object(obj)
    bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)


def generate_asset_preparation_benchmark(
    root: Path, parameters: dict[str, object]
) -> dict[str, object]:
    """Create an owned deterministic fixture spanning the production-preparation stages."""
    allowed = {"output_path"}
    unknown = set(parameters) - allowed
    if unknown:
        raise ValueError(
            f"unsupported asset preparation benchmark parameters: {sorted(unknown)}"
        )
    output = confined(root, str(parameters["output_path"]))
    if output.suffix.lower() != ".blend":
        raise ValueError("asset preparation benchmark output must use .blend")
    output.parent.mkdir(parents=True, exist_ok=True)
    for item in list(bpy.data.objects):
        bpy.data.objects.remove(item, do_unlink=True)
    scene = bpy.context.scene
    scene.unit_settings.system = "METRIC"
    scene.unit_settings.scale_length = 0.001
    scene.unit_settings.length_unit = "MILLIMETERS"
    scene["bvmcp_benchmark_id"] = "asset-preparation-v1"
    scene["bvmcp_rights_state"] = "SYNTHETIC_OWNED_CC0"
    scene["bvmcp_candidate_accepted"] = False

    fixture_material = _material(
        "Benchmark_Source_Neutral", (0.3, 0.34, 0.4, 1.0), 0.1, 0.45
    )
    bpy.ops.mesh.primitive_cube_add(location=(-150.0, 0.0, 20.0))
    product = bpy.context.active_object
    product.name = "Benchmark_HardSurface"
    product.scale = (60.0, 35.0, 20.0)
    _apply_fixture_scale(product)
    product.data.materials.append(fixture_material)
    bevel = product.modifiers.new("Benchmark_Product_Bevel", "BEVEL")
    bevel.width = 5.0
    bevel.segments = 5
    _activate_object(product)
    bpy.ops.object.modifier_apply(modifier=bevel.name)

    bpy.ops.mesh.primitive_uv_sphere_add(
        segments=48,
        ring_count=24,
        location=(-20.0, 0.0, 42.0),
    )
    curved = bpy.context.active_object
    curved.name = "Benchmark_CurvedConsumer"
    curved.scale = (42.0, 28.0, 42.0)
    _apply_fixture_scale(curved)
    curved.data.materials.append(fixture_material)

    bpy.ops.mesh.primitive_ico_sphere_add(
        subdivisions=4,
        radius=36.0,
        location=(90.0, 0.0, 42.0),
    )
    organic = bpy.context.active_object
    organic.name = "Benchmark_Organic"
    for vertex in organic.data.vertices:
        factor = 1.0 + 0.12 * math.sin(vertex.co.z * 0.17) * math.cos(vertex.co.x * 0.13)
        vertex.co.x *= factor
        vertex.co.y *= 0.8 + 0.08 * math.sin(vertex.co.z * 0.11)
        vertex.co.z *= 1.25
    organic.data.update()
    organic.data.materials.append(fixture_material)

    bpy.ops.mesh.primitive_uv_sphere_add(
        segments=40,
        ring_count=24,
        location=(190.0, 0.0, 45.0),
    )
    character = bpy.context.active_object
    character.name = "Benchmark_CharacterLite"
    character.scale = (20.0, 15.0, 45.0)
    _apply_fixture_scale(character)
    character.data.materials.append(fixture_material)

    bpy.ops.mesh.primitive_cylinder_add(
        vertices=48,
        radius=34.0,
        depth=65.0,
        location=(285.0, 0.0, 34.0),
    )
    textured = bpy.context.active_object
    textured.name = "Benchmark_Textured"
    textured.data.materials.append(fixture_material)

    damaged_mesh = bpy.data.meshes.new("Benchmark_Damaged_Mesh")
    damaged_mesh.from_pydata(
        [
            (-20.0, -20.0, 0.0),
            (20.0, -20.0, 0.0),
            (20.0, 20.0, 0.0),
            (-20.0, 20.0, 0.0),
            (-20.0, -20.0, 40.0),
            (20.0, -20.0, 40.0),
            (20.0, 20.0, 40.0),
            (-20.0, 20.0, 40.0),
            (-20.0, -20.0, 0.0),
        ],
        [],
        [
            (0, 3, 2, 1),
            (4, 5, 6, 7),
            (0, 1, 5, 4),
            (1, 2, 6, 5),
            (2, 3, 7, 6),
            (3, 0, 4, 7),
            (0, 8, 1),
        ],
    )
    damaged_mesh.update()
    damaged = bpy.data.objects.new("Benchmark_Damaged", damaged_mesh)
    bpy.context.scene.collection.objects.link(damaged)
    damaged.location = (375.0, 0.0, 0.0)
    damaged.data.materials.append(fixture_material)

    for obj in (product, curved, organic, character, textured, damaged):
        obj["bvmcp_component_id"] = obj.name
        obj["bvmcp_fixture_authority"] = "procedural_ground_truth"

    targets = [
        {
            "name": product.name,
            "retopology_ratio": 0.72,
            "uv": True,
            "material": {
                "name": "Benchmark_Anodized_PBR",
                "material_class": "reflective_anodized_metal",
                "base_color": [0.04, 0.05, 0.065, 1.0],
                "metallic": 0.92,
                "roughness": 0.24,
            },
            "animation": {
                "kind": "object",
                "frame_start": 1,
                "frame_end": 48,
                "rotation_degrees": 28.0,
            },
            "lod_ratios": [0.5, 0.2],
            "collision": True,
        },
        {
            "name": curved.name,
            "retopology_ratio": 0.55,
            "uv": True,
            "material": {
                "name": "Benchmark_Translucent_PBR",
                "material_class": "frosted_translucent_polymer",
                "base_color": [0.65, 0.76, 0.82, 0.72],
                "metallic": 0.0,
                "roughness": 0.38,
                "transmission": 0.62,
                "alpha": 0.72,
            },
            "lod_ratios": [0.5],
            "collision": True,
        },
        {
            "name": organic.name,
            "retopology_ratio": 0.45,
            "uv": True,
            "material": {
                "name": "Benchmark_Organic_PBR",
                "material_class": "matte_organic_surface",
                "base_color": [0.18, 0.32, 0.16, 1.0],
                "metallic": 0.0,
                "roughness": 0.72,
            },
            "lod_ratios": [0.5, 0.25],
            "collision": True,
        },
        {
            "name": character.name,
            "retopology_ratio": 0.5,
            "uv": True,
            "material": {
                "name": "Benchmark_Character_PBR",
                "material_class": "stylized_character",
                "base_color": [0.36, 0.14, 0.5, 1.0],
                "metallic": 0.0,
                "roughness": 0.62,
            },
            "rig": {
                "kind": "character_lite",
                "frame_start": 1,
                "frame_end": 48,
            },
            "lod_ratios": [0.5],
            "collision": True,
        },
        {
            "name": textured.name,
            "uv": True,
            "material": {
                "name": "Benchmark_Baked_PBR",
                "material_class": "painted_metal",
                "base_color": [0.72, 0.22, 0.08, 1.0],
                "metallic": 0.35,
                "roughness": 0.46,
            },
            "texture_bake": True,
            "texture_resolution": 128,
            "lod_ratios": [0.5],
        },
        {
            "name": damaged.name,
            "repair": True,
            "repair_merge_distance": 1e-5,
            "uv": True,
            "material": {
                "name": "Benchmark_Repaired_PBR",
                "material_class": "repaired_test_surface",
                "base_color": [0.18, 0.24, 0.3, 1.0],
                "metallic": 0.1,
                "roughness": 0.55,
            },
        },
    ]
    checkpoint = save_checkpoint(root, {"output_path": str(output)})
    return {
        **checkpoint,
        "benchmark_id": "asset-preparation-v1",
        "rights_state": "SYNTHETIC_OWNED_CC0",
        "objects": [obj.name for obj in (product, curved, organic, character, textured, damaged)],
        "targets": targets,
        "expected_capabilities": [
            "retopology",
            "uv_generation",
            "pbr_material_generation",
            "texture_projection_and_baking",
            "rigging",
            "object_animation",
            "character_lite_animation",
            "lod_generation",
            "collision_generation",
            "mesh_repair",
        ],
        "network_used": False,
    }


def _appearance_camera_state(
    name: str,
    position: tuple[float, float, float],
    target: tuple[float, float, float],
    *,
    width: int,
    height: int,
    horizontal_fov_degrees: float,
) -> dict[str, object]:
    camera_data = bpy.data.cameras.new(f"{name}_Data")
    camera = bpy.data.objects.new(name, camera_data)
    bpy.context.scene.collection.objects.link(camera)
    camera.location = position
    point_camera(camera, Vector(target))
    camera_data.sensor_fit = "HORIZONTAL"
    camera_data.sensor_width = 36.0
    fx = width / (2.0 * math.tan(math.radians(horizontal_fov_degrees) / 2.0))
    camera_data.lens = fx / width * camera_data.sensor_width
    matrix = [
        [float(camera.matrix_world[row][column]) for column in range(4)]
        for row in range(4)
    ]
    bpy.data.objects.remove(camera, do_unlink=True)
    bpy.data.cameras.remove(camera_data)
    state: dict[str, object] = {
        "reference_id": name,
        "model": "PINHOLE",
        "width": width,
        "height": height,
        "intrinsics": {
            "fx": fx,
            "fy": fx,
            "cx": width / 2.0,
            "cy": height / 2.0,
        },
        "world_from_camera": matrix,
        "extrinsics": {"world_from_camera": matrix},
        "registration_class": "metric_camera_solution",
        "evidence_class": "MEASURED",
        "confidence": 1.0,
        "distortion_model": {
            "type": "PINHOLE",
            "parameters": {},
            "render_policy": "undistorted_input",
        },
        "sensor_model": {
            "type": "virtual_pinhole",
            "sensor_width_mm": 36.0,
            "pixel_aspect_x": 1.0,
            "pixel_aspect_y": 1.0,
        },
        "crop": {
            "x": 0,
            "y": 0,
            "width": width,
            "height": height,
            "source": "full_frame",
        },
        "resolution": {"width": width, "height": height},
        "clipping": {"near": 0.1, "far": 5000.0},
        "coordinate_transform": {
            "matrix": matrix,
            "matrix_semantics": "world_from_camera",
            "world_handedness": "right",
            "world_up_axis": "Z",
            "camera_forward_axis": "-Z",
            "camera_up_axis": "Y",
        },
        "camera_source_identity": {
            "reference_id": name,
            "artifact_digest": None,
            "original_name": f"{name}.synthetic",
        },
        "solve_method": {
            "backend": "procedural_ground_truth",
            "registration_class": "metric_camera_solution",
        },
        "approval_state": "approved_procedural_ground_truth",
    }
    state["immutable_sha256"] = hashlib.sha256(
        json.dumps(state, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()
    return state


def _appearance_area_light(
    name: str,
    location: tuple[float, float, float],
    target: tuple[float, float, float],
    *,
    energy: float,
    color: tuple[float, float, float],
    size: float,
):
    data = bpy.data.lights.new(name, "AREA")
    data.energy = energy
    data.color = color
    data.shape = "DISK"
    data.size = size
    light = bpy.data.objects.new(name, data)
    bpy.context.scene.collection.objects.link(light)
    light.location = location
    point_camera(light, Vector(target))
    light["bvmcp_lighting_authority"] = "procedural_ground_truth"
    return light


def generate_appearance_benchmark(
    root: Path, parameters: dict[str, object]
) -> dict[str, object]:
    """Create a deterministic reflective, translucent, and emissive appearance fixture."""
    if set(parameters) != {"output_path"}:
        raise ValueError("appearance benchmark requires only output_path")
    output = confined(root, str(parameters["output_path"]))
    if output.suffix.lower() != ".blend":
        raise ValueError("appearance benchmark output must use .blend")
    output.parent.mkdir(parents=True, exist_ok=True)
    for item in list(bpy.data.objects):
        bpy.data.objects.remove(item, do_unlink=True)
    for material in list(bpy.data.materials):
        bpy.data.materials.remove(material)
    scene = bpy.context.scene
    scene.unit_settings.system = "METRIC"
    scene.unit_settings.scale_length = 0.001
    scene.unit_settings.length_unit = "MILLIMETERS"
    scene.render.engine = "BLENDER_EEVEE_NEXT"
    scene.render.resolution_x = 320
    scene.render.resolution_y = 256
    scene.render.resolution_percentage = 100
    scene.render.dither_intensity = 0.0
    scene.view_settings.view_transform = "AgX"
    scene.view_settings.look = "AgX - Medium High Contrast"
    scene.view_settings.exposure = -0.35
    scene["bvmcp_benchmark_id"] = "appearance-authority-v1"
    scene["bvmcp_rights_state"] = "SYNTHETIC_OWNED_CC0"
    scene["bvmcp_lighting_hypothesis"] = "procedural_ground_truth"
    scene["bvmcp_candidate_accepted"] = False
    scene.world = bpy.data.worlds.new("Appearance_Benchmark_World")
    scene.world.use_nodes = True
    background = scene.world.node_tree.nodes.get("Background")
    background.inputs["Color"].default_value = (0.012, 0.016, 0.028, 1.0)
    background.inputs["Strength"].default_value = 0.09

    bpy.ops.mesh.primitive_cube_add(location=(0.0, 0.0, 24.0))
    shell = bpy.context.active_object
    shell.name = "Appearance_AnodizedShell"
    shell.scale = (92.0, 58.0, 24.0)
    _apply_fixture_scale(shell)
    bevel = shell.modifiers.new("Appearance_Shell_Bevel", "BEVEL")
    bevel.width = 12.0
    bevel.segments = 10
    _activate_object(shell)
    bpy.ops.object.modifier_apply(modifier=bevel.name)
    shell_material, shell_report = _create_asset_pbr_material(
        shell,
        {
            "name": "Appearance_AnodizedMetal",
            "material_class": "anodized_metal",
            "base_color": [0.025, 0.035, 0.055, 1.0],
            "metallic": 0.92,
            "roughness": 0.23,
        },
    )

    bpy.ops.mesh.primitive_uv_sphere_add(
        segments=64,
        ring_count=32,
        location=(0.0, -18.0, 88.0),
    )
    core = bpy.context.active_object
    core.name = "Appearance_FrostedCore"
    core.scale = (42.0, 32.0, 70.0)
    _apply_fixture_scale(core)
    core_material, core_report = _create_asset_pbr_material(
        core,
        {
            "name": "Appearance_FrostedGlass",
            "material_class": "translucent",
            "base_color": [0.55, 0.72, 0.88, 0.68],
            "metallic": 0.0,
            "roughness": 0.32,
            "transmission": 0.76,
            "alpha": 0.68,
        },
    )
    principled = core_material.node_tree.nodes.get("Principled BSDF")
    if principled and "IOR" in principled.inputs:
        principled.inputs["IOR"].default_value = 1.46

    bpy.ops.mesh.primitive_cylinder_add(
        vertices=64,
        radius=28.0,
        depth=5.0,
        location=(0.0, -52.0, 92.0),
        rotation=(math.pi / 2.0, 0.0, 0.0),
    )
    disk = bpy.context.active_object
    disk.name = "Appearance_EmissiveDisk"
    disk_material, disk_report = _create_asset_pbr_material(
        disk,
        {
            "name": "Appearance_Emissive",
            "material_class": "emissive",
            "base_color": [0.08, 0.01, 0.002, 1.0],
            "metallic": 0.0,
            "roughness": 0.4,
            "emission_color": [1.0, 0.055, 0.005, 1.0],
            "emission_strength": 9.0,
        },
    )

    bpy.ops.mesh.primitive_torus_add(
        major_radius=72.0,
        minor_radius=5.0,
        major_segments=96,
        minor_segments=16,
        location=(0.0, 0.0, 88.0),
        rotation=(math.pi / 2.0, 0.0, 0.0),
    )
    membrane = bpy.context.active_object
    membrane.name = "Appearance_AcousticMembrane"
    membrane_material, membrane_report = _create_asset_pbr_material(
        membrane,
        {
            "name": "Appearance_Textile",
            "material_class": "textile",
            "base_color": [0.035, 0.025, 0.055, 1.0],
            "metallic": 0.0,
            "roughness": 0.88,
        },
    )
    del shell_material, disk_material, membrane_material
    for obj in (shell, core, disk, membrane):
        obj["bvmcp_component_id"] = obj.name
        obj["bvmcp_appearance_authority"] = "procedural_ground_truth"

    target = (0.0, 0.0, 70.0)
    lights = [
        _appearance_area_light(
            "Appearance_Key",
            (-170.0, -230.0, 250.0),
            target,
            energy=850.0,
            color=(1.0, 0.78, 0.62),
            size=95.0,
        ),
        _appearance_area_light(
            "Appearance_Fill",
            (190.0, -150.0, 140.0),
            target,
            energy=520.0,
            color=(0.52, 0.68, 1.0),
            size=120.0,
        ),
        _appearance_area_light(
            "Appearance_Rim",
            (0.0, 170.0, 230.0),
            target,
            energy=1100.0,
            color=(0.7, 0.82, 1.0),
            size=80.0,
        ),
    ]
    cameras = [
        {
            "id": "public-front",
            "visibility": "public",
            "camera_state": _appearance_camera_state(
                "public-front",
                (250.0, -360.0, 190.0),
                target,
                width=320,
                height=256,
                horizontal_fov_degrees=48.0,
            ),
        },
        {
            "id": "public-side",
            "visibility": "public",
            "camera_state": _appearance_camera_state(
                "public-side",
                (-300.0, -250.0, 150.0),
                target,
                width=320,
                height=256,
                horizontal_fov_degrees=52.0,
            ),
        },
        {
            "id": "public-high",
            "visibility": "public",
            "camera_state": _appearance_camera_state(
                "public-high",
                (80.0, -300.0, 330.0),
                target,
                width=320,
                height=256,
                horizontal_fov_degrees=46.0,
            ),
        },
        {
            "id": "holdout-rear-quarter",
            "visibility": "holdout",
            "camera_state": _appearance_camera_state(
                "holdout-rear-quarter",
                (285.0, 260.0, 175.0),
                target,
                width=320,
                height=256,
                horizontal_fov_degrees=50.0,
            ),
        },
    ]
    checkpoint = save_checkpoint(root, {"output_path": str(output)})
    return {
        **checkpoint,
        "benchmark_id": "appearance-authority-v1",
        "rights_state": "SYNTHETIC_OWNED_CC0",
        "cameras": cameras,
        "materials": {
            "Appearance_AnodizedMetal": {
                **shell_report,
                "ior": 1.5,
            },
            "Appearance_FrostedGlass": {
                **core_report,
                "ior": 1.46,
            },
            "Appearance_Emissive": {
                **disk_report,
                "ior": 1.5,
            },
            "Appearance_Textile": {
                **membrane_report,
                "ior": 1.5,
            },
        },
        "lighting": {
            "hypothesis_class": "procedural_ground_truth",
            "lights": [_light_inspection(light) for light in lights],
            "environment": {
                "kind": "constant_world",
                "color": _json_socket_value(
                    background.inputs["Color"].default_value
                ),
                "strength": float(background.inputs["Strength"].default_value),
                "hdr_supplied": False,
            },
            "uncertainty": {
                "classification": "none_procedural_ground_truth",
            },
        },
        "required_separate_objects": [
            "Appearance_AnodizedShell",
            "Appearance_FrostedCore",
            "Appearance_EmissiveDisk",
            "Appearance_AcousticMembrane",
        ],
        "network_used": False,
    }


def generate_lod(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    ratio = float(parameters.get("ratio", 0.5))
    if not 0.01 <= ratio <= 1.0:
        raise ValueError("LOD ratio must be between 0.01 and 1.0")
    requested = parameters.get("objects")
    if requested is not None and not isinstance(requested, list):
        raise ValueError("LOD object filter must be a list")
    requested_names = {str(name) for name in requested or []}
    sources = [
        item
        for item in bpy.context.scene.objects
        if item.type == "MESH"
        and not item.hide_render
        and (not requested_names or item.name in requested_names)
    ]
    if not sources or len(sources) > 128:
        raise ValueError("LOD generation requires between one and 128 mesh objects")
    collection = bpy.data.collections.get("BVMCP_LOD")
    if collection is None:
        collection = bpy.data.collections.new("BVMCP_LOD")
        bpy.context.scene.collection.children.link(collection)
    generated = []
    for source in sources:
        item = source.copy()
        item.data = source.data.copy()
        item.name = f"{source.name}_LOD_{ratio:.3f}"
        collection.objects.link(item)
        modifier = item.modifiers.new("BVMCP_Decimate", "DECIMATE")
        modifier.ratio = ratio
        bpy.context.view_layer.objects.active = item
        item.select_set(True)
        bpy.ops.object.modifier_apply(modifier=modifier.name)
        item.select_set(False)
        item["bvmcp_lod_source"] = source.name
        item["bvmcp_lod_ratio"] = ratio
        generated.append(
            {
                "source": source.name,
                "object": item.name,
                "polygons": len(item.data.polygons),
            }
        )
    checkpoint = save_checkpoint(root, parameters)
    return {**checkpoint, "ratio": ratio, "generated": generated}


def repair_degenerate_geometry_candidate(
    root: Path, parameters: dict[str, object]
) -> dict[str, object]:
    """Merge only near-identical vertices implicated in known zero-area faces.

    This worker operation creates a checkpoint; it never changes scene lifecycle
    state or claims review acceptance.  Tight guards keep it from becoming a
    general-purpose topology cleanup that could silently reshape a model.
    """

    output = confined(root, str(parameters["output_path"]))
    output.parent.mkdir(parents=True, exist_ok=True)
    object_name = str(parameters.get("object_name", ""))
    if not object_name:
        raise ValueError("degenerate repair requires one explicit object_name")
    target = bpy.data.objects.get(object_name)
    if target is None or target.type != "MESH":
        raise ValueError(f"degenerate repair mesh was not found: {object_name}")
    area_epsilon = float(parameters.get("area_epsilon", 1e-14))
    merge_distance = float(parameters.get("merge_distance", 1e-10))
    expected_degenerate_faces = int(parameters.get("expected_degenerate_faces", 0))
    if not 0.0 < area_epsilon <= 1e-10:
        raise ValueError("area_epsilon must be in (0, 1e-10]")
    if not 0.0 < merge_distance <= 1e-8:
        raise ValueError("merge_distance must be in (0, 1e-8]")
    if not 1 <= expected_degenerate_faces <= 64:
        raise ValueError("expected_degenerate_faces must be between 1 and 64")

    mesh = target.data
    graph = bmesh.new()
    graph.from_mesh(mesh)
    bounds_before = object_world_bounds(target)
    before = {
        "vertices": len(graph.verts),
        "edges": len(graph.edges),
        "faces": len(graph.faces),
        "degenerate_faces": sum(
            face.calc_area() <= area_epsilon for face in graph.faces
        ),
        "non_manifold_edges": sum(not edge.is_manifold for edge in graph.edges),
    }
    if before["degenerate_faces"] != expected_degenerate_faces:
        graph.free()
        raise ValueError(
            "degenerate face precondition changed: "
            f"expected {expected_degenerate_faces}, found {before['degenerate_faces']}"
        )

    bmesh.ops.remove_doubles(graph, verts=list(graph.verts), dist=merge_distance)
    after = {
        "vertices": len(graph.verts),
        "edges": len(graph.edges),
        "faces": len(graph.faces),
        "degenerate_faces": sum(
            face.calc_area() <= area_epsilon for face in graph.faces
        ),
        "non_manifold_edges": sum(not edge.is_manifold for edge in graph.edges),
    }
    if after["degenerate_faces"]:
        graph.free()
        raise ValueError(
            f"candidate repair left {after['degenerate_faces']} degenerate faces"
        )
    if after["non_manifold_edges"] > before["non_manifold_edges"]:
        graph.free()
        raise ValueError("candidate repair introduced non-manifold edges")
    removed_vertices = before["vertices"] - after["vertices"]
    removed_faces = before["faces"] - after["faces"]
    if removed_vertices <= 0 or removed_faces != expected_degenerate_faces:
        graph.free()
        raise ValueError(
            "candidate repair changed an unexpected topology scope: "
            f"removed_vertices={removed_vertices}, removed_faces={removed_faces}"
        )
    graph.to_mesh(mesh)
    graph.free()
    mesh.update()
    bounds_after = object_world_bounds(target)
    maximum_bound_delta = max(
        abs(float(bounds_after[group][axis]) - float(bounds_before[group][axis]))
        for group in ("minimum", "maximum", "dimensions")
        for axis in range(3)
    )
    if maximum_bound_delta > merge_distance:
        raise ValueError(
            "candidate repair changed the governed object envelope by "
            f"{maximum_bound_delta:.12g}, exceeding merge_distance"
        )

    checkpoint = save_checkpoint(root, parameters)
    return {
        **checkpoint,
        "candidate_only": True,
        "acceptance": False,
        "object": object_name,
        "area_epsilon": area_epsilon,
        "merge_distance": merge_distance,
        "expected_degenerate_faces": expected_degenerate_faces,
        "removed_vertices": removed_vertices,
        "removed_faces": removed_faces,
        "maximum_bound_delta": maximum_bound_delta,
        "before": before,
        "after": after,
    }


def _mesh_topology(obj) -> dict[str, object]:
    mesh = obj.data
    graph = bmesh.new()
    graph.from_mesh(mesh)
    non_manifold_edges = sum(not edge.is_manifold for edge in graph.edges)
    euler = len(graph.verts) - len(graph.edges) + len(graph.faces)
    unseen = set(graph.verts)
    connected_components = 0
    while unseen:
        connected_components += 1
        stack = [unseen.pop()]
        while stack:
            vertex = stack.pop()
            for edge in vertex.link_edges:
                other = edge.other_vert(vertex)
                if other in unseen:
                    unseen.remove(other)
                    stack.append(other)
    genus = connected_components - euler / 2.0 if non_manifold_edges == 0 else None
    graph.free()
    return {
        "vertices": len(mesh.vertices),
        "edges": len(mesh.edges),
        "faces": len(mesh.polygons),
        "euler": euler,
        "non_manifold_edges": non_manifold_edges,
        "connected_components": connected_components,
        "closed_surface_genus": genus,
    }


def repair_mac_studio_grille(root: Path, parameters: dict[str, object]) -> dict[str, object]:
    output = confined(root, str(parameters["output_path"]))
    output.parent.mkdir(parents=True, exist_ok=True)
    body_name = str(parameters.get("body_object", "mac-studio"))
    old_panel_name = str(parameters.get("existing_panel_object", "mac-vent-mesh"))
    body = bpy.data.objects.get(body_name)
    if body is None or body.type != "MESH":
        raise ValueError(f"Mac Studio body mesh was not found: {body_name}")
    if any(abs(float(value) - 1.0) > 1e-6 for value in body.scale):
        raise ValueError("Mac Studio body must have applied scale before parametric repair")
    scale_to_mm = float(bpy.context.scene.unit_settings.scale_length) * 1000.0
    to_local = 1.0 / scale_to_mm
    width_mm = float(parameters.get("field_width_mm", 171.5))
    height_mm = float(parameters.get("field_height_mm", 48.5))
    z_center_mm = float(parameters.get("z_center_mm", 66.3))
    pitch_mm = float(parameters.get("pitch_mm", 1.934))
    diameter_mm = float(parameters.get("diameter_mm", 0.918))
    thickness_mm = float(parameters.get("panel_thickness_mm", 1.2))
    recess_depth_mm = float(parameters.get("recess_depth_mm", 15.0))
    corner_radius_mm = float(parameters.get("corner_radius_mm", 28.0))
    target_count = int(parameters.get("target_hole_count", 2349))
    minimum_open_fraction = float(parameters.get("minimum_open_fraction", 0.9))
    width = width_mm * to_local
    height = height_mm * to_local
    z_center = z_center_mm * to_local - float(body.location.z)
    pitch = pitch_mm * to_local
    diameter = diameter_mm * to_local
    thickness = thickness_mm * to_local
    recess_depth = recess_depth_mm * to_local
    corner_radius = corner_radius_mm * to_local
    local_bounds = [Vector(corner) for corner in body.bound_box]
    rear_y = max(point.y for point in local_bounds)
    centers = _grille_centers(width, height, pitch, diameter, target_count, corner_radius)
    center_width = max(x for x, _z in centers) - min(x for x, _z in centers)
    center_height = max(z for _x, z in centers) - min(z for _x, z in centers)
    if center_width < width - 2.0 * pitch or center_height < height - 2.0 * pitch:
        raise ValueError(
            "generated grille centers do not span the evidence-bound field: "
            f"{center_width * scale_to_mm:.3f} x {center_height * scale_to_mm:.3f} mm"
        )
    old_panel = bpy.data.objects.get(old_panel_name)
    replaced_object = old_panel.name if old_panel else None
    if old_panel:
        bpy.data.objects.remove(old_panel, do_unlink=True)
    body_world_bounds = [body.matrix_world @ point for point in local_bounds]
    body_front_world = min(point.y for point in body_world_bounds)
    body_rear_world = max(point.y for point in body_world_bounds)
    envelope_corrections = []
    for detail in list(bpy.context.scene.objects):
        if detail == body or detail.type != "MESH" or detail.parent == body:
            continue
        detail_bounds = [detail.matrix_world @ Vector(corner) for corner in detail.bound_box]
        minimum_y = min(point.y for point in detail_bounds)
        maximum_y = max(point.y for point in detail_bounds)
        correction = 0.0
        if detail.matrix_world.translation.y > 0.0 and maximum_y > body_rear_world:
            correction = body_rear_world - maximum_y
        elif detail.matrix_world.translation.y < 0.0 and minimum_y < body_front_world:
            correction = body_front_world - minimum_y
        if correction and abs(correction) <= 2.0 * to_local:
            local_correction = Vector((0.0, correction, 0.0))
            if detail.parent:
                local_correction = detail.parent.matrix_world.inverted().to_3x3() @ local_correction
            detail.location += local_correction
            envelope_corrections.append(
                {"object": detail.name, "y_shift_mm": correction * scale_to_mm}
            )
    cutter = _local_cube(
        "BVMCP_grille-recess-cutter",
        body,
        (0.0, rear_y - recess_depth / 2.0 + 0.5 * to_local, z_center),
        (width + 2.0 * to_local, recess_depth + 1.0 * to_local, height + 2.0 * to_local),
    )
    modifier = body.modifiers.new("BVMCP_rear-grille-recess", "BOOLEAN")
    modifier.operation = "DIFFERENCE"
    modifier.solver = "EXACT"
    modifier.object = cutter
    bpy.context.view_layer.objects.active = body
    body.select_set(True)
    bpy.ops.object.modifier_apply(modifier=modifier.name)
    bpy.data.objects.remove(cutter, do_unlink=True)
    panel = _create_annular_hex_panel(
        "BVMCP_mac-hero-grille",
        centers,
        pitch=pitch,
        diameter=diameter,
        thickness=thickness,
        y_center=rear_y - thickness / 2.0,
        z_center=z_center,
        parent=body,
    )
    panel_material = bpy.data.materials.get("mac-rear-perf-alu") or _material(
        "BVMCP_rear-perf-aluminium", (0.18, 0.19, 0.2, 1.0), 0.82, 0.28
    )
    panel.data.materials.append(panel_material)
    dark_material = _material("BVMCP_rear-interior-dark", (0.003, 0.004, 0.005, 1.0), 0.0, 0.7)
    backing = _local_cube(
        "BVMCP_mac-rear-interior-dark",
        body,
        (0.0, rear_y - recess_depth + 0.7 * to_local, z_center),
        (width + 1.0 * to_local, 1.0 * to_local, height + 1.0 * to_local),
        dark_material,
    )
    panel["bvmcp_component_type"] = "Grille"
    panel["bvmcp_evidence_class"] = "SINGLE_VIEW_OBSERVED"
    panel["bvmcp_hole_count"] = target_count
    panel["bvmcp_pitch_mm"] = pitch_mm
    panel["bvmcp_hole_diameter_mm"] = diameter_mm
    panel["bvmcp_parent_frame"] = body_name
    backing["bvmcp_component_type"] = "Panel"
    bpy.context.view_layer.update()
    dependency_graph = bpy.context.evaluated_depsgraph_get()
    test_centers = centers[:: max(1, len(centers) // 240)]
    hit_counts: dict[str, int] = {}
    for x, z in test_centers:
        local_origin = Vector((x, rear_y + 5.0 * to_local, z_center + z))
        origin = body.matrix_world @ local_origin
        direction = (body.matrix_world.to_3x3() @ Vector((0.0, -1.0, 0.0))).normalized()
        hit, _location, _normal, _face, hit_object, _matrix = bpy.context.scene.ray_cast(
            dependency_graph, origin, direction, distance=(recess_depth + 10.0 * to_local)
        )
        name = hit_object.name if hit and hit_object else "MISS"
        hit_counts[name] = hit_counts.get(name, 0) + 1
    rays = len(test_centers)
    backing_hits = hit_counts.get(backing.name, 0)
    open_fraction = backing_hits / rays if rays else 0.0
    topology = _mesh_topology(panel)
    if topology["non_manifold_edges"]:
        raise ValueError(
            f"generated grille has non-manifold edges: {topology['non_manifold_edges']}"
        )
    if topology["connected_components"] != 1 or topology["closed_surface_genus"] != len(centers):
        raise ValueError(
            "generated grille topology does not prove one through-hole per requested aperture"
        )
    if open_fraction < minimum_open_fraction:
        raise ValueError(
            f"generated grille open-ray fraction {open_fraction:.3f} is below "
            f"{minimum_open_fraction:.3f}; first hits: {hit_counts}"
        )
    bounds = [panel.matrix_world @ Vector(corner) for corner in panel.bound_box]
    bound_minimum = [min(point[index] for point in bounds) * scale_to_mm for index in range(3)]
    bound_maximum = [max(point[index] for point in bounds) * scale_to_mm for index in range(3)]
    bound_dimensions = [bound_maximum[index] - bound_minimum[index] for index in range(3)]
    dimensional_checks = {
        "field_width_within_2_percent": abs(bound_dimensions[0] - width_mm) / width_mm <= 0.02,
        "field_height_within_2_percent": abs(bound_dimensions[2] - height_mm) / height_mm <= 0.02,
        "z_center_within_0_5_mm": abs((bound_minimum[2] + bound_maximum[2]) / 2.0 - z_center_mm)
        <= 0.5,
    }
    if not all(dimensional_checks.values()):
        raise ValueError(f"generated grille failed dimensional checks: {dimensional_checks}")
    bpy.ops.wm.save_as_mainfile(filepath=str(output), check_existing=False)
    return {
        "checkpoint_path": str(output.relative_to(root)),
        "body_object": body.name,
        "replaced_object": replaced_object,
        "envelope_corrections": envelope_corrections,
        "panel_object": panel.name,
        "backing_object": backing.name,
        "parameters": {
            "field_width_mm": width_mm,
            "field_height_mm": height_mm,
            "z_center_mm": z_center_mm,
            "pitch_mm": pitch_mm,
            "diameter_mm": diameter_mm,
            "panel_thickness_mm": thickness_mm,
            "target_hole_count": target_count,
            "rear_face": "+Y body-local",
        },
        "generated_hole_count": len(centers),
        "center_field_span_mm": {
            "width": center_width * scale_to_mm,
            "height": center_height * scale_to_mm,
        },
        "topology": topology,
        "construction_validation": {
            "method": panel["bvmcp_construction_method"],
            "cell_count": panel["bvmcp_cell_count"],
            "shared_cell_edges": panel["bvmcp_shared_cell_edges"],
            "boundary_cell_edges": panel["bvmcp_boundary_cell_edges"],
            "duplicate_faces": panel["bvmcp_duplicate_faces"],
            "claim": (
                "Convex Voronoi cells are welded on every shared edge; only one exterior wall is "
                "emitted per boundary edge."
            ),
        },
        "ray_validation": {
            "rays": rays,
            "backing_hits": backing_hits,
            "open_fraction": open_fraction,
            "first_hit_counts": hit_counts,
        },
        "panel_bounds_mm": {
            "minimum": bound_minimum,
            "maximum": bound_maximum,
            "dimensions": bound_dimensions,
        },
        "dimensional_checks": dimensional_checks,
    }


def main() -> None:
    arguments = sys.argv[sys.argv.index("--") + 1 :] if "--" in sys.argv else []
    if len(arguments) != 1:
        raise ValueError("expected exactly one manifest path")
    manifest_path = Path(arguments[0]).resolve()
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    provided_hash = manifest.pop("manifest_hash", None)
    encoded_manifest = json.dumps(
        manifest, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode()
    if provided_hash != hashlib.sha256(encoded_manifest).hexdigest():
        raise ValueError("manifest hash mismatch")
    if manifest.get("schema_version") != 1:
        raise ValueError("unsupported manifest schema")
    operation = manifest.get("operation")
    if operation not in ALLOWED_OPERATIONS:
        raise ValueError(f"operation is not allowlisted: {operation}")
    root = Path(manifest["project_root"]).resolve()
    confined(root, str(manifest_path), must_exist=True)
    confined(root, manifest["scene_path"], must_exist=True)
    result_path = confined(root, manifest["result_path"])
    parameters = manifest.get("parameters", {})
    if not isinstance(parameters, dict):
        raise TypeError("manifest parameters must be an object")
    if operation == "inspect_scene":
        result = inspect_scene(root, bool(manifest.get("safe_mode", True)))
    elif operation == "validate_scene":
        inventory = inspect_scene(root, bool(manifest.get("safe_mode", True)))
        result = {
            "inventory": inventory,
            "valid": not any(
                finding["severity"] == "error" for finding in inventory["audit_findings"]
            ),
            "summary": {
                "errors": sum(
                    finding["severity"] == "error" for finding in inventory["audit_findings"]
                ),
                "warnings": sum(
                    finding["severity"] == "warning" for finding in inventory["audit_findings"]
                ),
            },
        }
    elif operation == "render_passes":
        result = render_passes(root, parameters)
    elif operation == "evaluate_camera_candidates":
        result = evaluate_camera_candidates(root, parameters)
    elif operation == "import_asset":
        result = import_asset(root, parameters)
    elif operation == "create_component":
        result = create_component(root, parameters)
    elif operation == "update_component":
        result = update_component(root, parameters)
    elif operation == "apply_constraints":
        result = apply_constraints(root, parameters)
    elif operation == "create_camera":
        result = create_camera(root, parameters)
    elif operation == "apply_camera_solution":
        result = apply_camera_solution(root, parameters)
    elif operation == "export_glb":
        result = export_glb(root, parameters)
    elif operation == "export_blend":
        result = export_blend(root, parameters)
    elif operation == "generate_lod":
        result = generate_lod(root, parameters)
    elif operation == "prepare_asset":
        result = prepare_asset(root, parameters)
    elif operation == "save_checkpoint":
        result = save_checkpoint(root, parameters)
    elif operation == "repair_degenerate_geometry_candidate":
        result = repair_degenerate_geometry_candidate(root, parameters)
    elif operation == "repair_mac_studio_grille":
        result = repair_mac_studio_grille(root, parameters)
    elif operation == "revise_rtx_5090_fe_candidate":
        result = revise_rtx_5090_fe_candidate(root, parameters)
    elif operation == "refine_rtx_5090_fe_visual_candidate":
        result = refine_rtx_5090_fe_visual_candidate(root, parameters)
    elif operation == "refine_rtx_5090_fe_front_frame_candidate":
        result = refine_rtx_5090_fe_front_frame_candidate(root, parameters)
    elif operation == "refine_dgx_spark_visual_candidate":
        result = refine_dgx_spark_visual_candidate(root, parameters)
    elif operation == "refine_dgx_spark_base_foot_candidate":
        result = refine_dgx_spark_base_foot_candidate(root, parameters)
    elif operation == "generate_components":
        result = generate_components(root, parameters)
    elif operation == "generate_semantic_seed":
        result = generate_semantic_seed(root, parameters)
    elif operation == "generate_synthetic_dataset":
        result = generate_synthetic_dataset(root, parameters)
    elif operation == "generate_calibration_benchmark":
        result = generate_calibration_benchmark(root, parameters)
    elif operation == "generate_asset_preparation_benchmark":
        result = generate_asset_preparation_benchmark(root, parameters)
    elif operation == "generate_appearance_benchmark":
        result = generate_appearance_benchmark(root, parameters)
    else:
        raise AssertionError(operation)
    encoded = json.dumps(result, indent=2, sort_keys=True)
    if len(encoded.encode("utf-8")) > int(manifest["limits"]["max_output_bytes"]):
        raise ValueError("worker result exceeds output limit")
    result_path.write_text(encoded + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
