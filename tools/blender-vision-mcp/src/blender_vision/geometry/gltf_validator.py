from __future__ import annotations

import base64
import binascii
import json
import math
import struct
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

from blender_vision.core.util import sha256_file

_GLB_MAGIC = b"glTF"
_JSON_CHUNK = 0x4E4F534A
_BIN_CHUNK = 0x004E4942
_COMPONENT_BYTES = {
    5120: 1,
    5121: 1,
    5122: 2,
    5123: 2,
    5125: 4,
    5126: 4,
}
_TYPE_COMPONENTS = {
    "SCALAR": 1,
    "VEC2": 2,
    "VEC3": 3,
    "VEC4": 4,
    "MAT2": 4,
    "MAT3": 9,
    "MAT4": 16,
}
_ANIMATION_PATHS = {"translation", "rotation", "scale", "weights"}
_INTERPOLATIONS = {"LINEAR", "STEP", "CUBICSPLINE"}
_DATA_IMAGE_PREFIXES = {
    "data:image/png;base64,",
    "data:image/jpeg;base64,",
    "data:image/webp;base64,",
}


@dataclass(slots=True)
class GlbValidationResult:
    path: str
    sha256: str
    size: int
    valid: bool
    errors: list[dict[str, Any]]
    warnings: list[dict[str, Any]]
    metrics: dict[str, Any]
    named_identity: dict[str, Any]
    extensions: dict[str, Any]
    authority: str = "STRUCTURAL_OBSERVED"
    validator: str = "visionmcp-glb-validator/v1"

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(slots=True)
class _Validation:
    document: dict[str, Any]
    binary: bytes
    errors: list[dict[str, Any]] = field(default_factory=list)
    warnings: list[dict[str, Any]] = field(default_factory=list)

    def error(self, code: str, pointer: str, message: str) -> None:
        self.errors.append({"code": code, "pointer": pointer, "message": message})

    def warning(self, code: str, pointer: str, message: str) -> None:
        self.warnings.append({"code": code, "pointer": pointer, "message": message})


class GlbValidator:
    """Strict, bounded GLB 2.0 structural validator with no external fetches."""

    def __init__(self, *, maximum_bytes: int = 512 * 1024 * 1024):
        if not 1 <= maximum_bytes <= 2 * 1024 * 1024 * 1024:
            raise ValueError("maximum_bytes must be between 1 byte and 2 GiB")
        self.maximum_bytes = maximum_bytes

    def validate(
        self,
        path: Path,
        *,
        required_node_names: list[str] | None = None,
        required_mesh_names: list[str] | None = None,
    ) -> GlbValidationResult:
        supplied_path = path.expanduser().absolute()
        if supplied_path.is_symlink():
            raise ValueError("GLB path must be a non-symlink regular file")
        path = supplied_path.resolve()
        if not path.is_file():
            raise ValueError("GLB path must be a non-symlink regular file")
        digest, size = sha256_file(path)
        if size > self.maximum_bytes:
            raise ValueError(
                f"GLB exceeds bounded validator size: {size} > {self.maximum_bytes}"
            )
        data = path.read_bytes()
        validation = self._decode(data)
        self._validate_document(validation)
        node_names = {
            str(node.get("name"))
            for node in validation.document.get("nodes", [])
            if node.get("name") is not None
        }
        mesh_names = {
            str(mesh.get("name"))
            for mesh in validation.document.get("meshes", [])
            if mesh.get("name") is not None
        }
        required_nodes = sorted(set(required_node_names or []))
        required_meshes = sorted(set(required_mesh_names or []))
        missing_nodes = sorted(set(required_nodes) - node_names)
        missing_meshes = sorted(set(required_meshes) - mesh_names)
        for name in missing_nodes:
            validation.error(
                "MISSING_REQUIRED_NODE",
                "/nodes",
                f"Required named node is missing: {name}",
            )
        for name in missing_meshes:
            validation.error(
                "MISSING_REQUIRED_MESH",
                "/meshes",
                f"Required named mesh is missing: {name}",
            )
        document = validation.document
        metrics = {
            "scene_count": len(document.get("scenes", [])),
            "node_count": len(document.get("nodes", [])),
            "mesh_count": len(document.get("meshes", [])),
            "primitive_count": sum(
                len(mesh.get("primitives", [])) for mesh in document.get("meshes", [])
            ),
            "material_count": len(document.get("materials", [])),
            "texture_count": len(document.get("textures", [])),
            "image_count": len(document.get("images", [])),
            "animation_count": len(document.get("animations", [])),
            "skin_count": len(document.get("skins", [])),
            "accessor_count": len(document.get("accessors", [])),
            "buffer_view_count": len(document.get("bufferViews", [])),
            "binary_chunk_bytes": len(validation.binary),
        }
        return GlbValidationResult(
            path=str(path),
            sha256=digest,
            size=size,
            valid=not validation.errors,
            errors=validation.errors,
            warnings=validation.warnings,
            metrics=metrics,
            named_identity={
                "required_nodes": required_nodes,
                "observed_nodes": sorted(node_names),
                "missing_nodes": missing_nodes,
                "required_meshes": required_meshes,
                "observed_meshes": sorted(mesh_names),
                "missing_meshes": missing_meshes,
            },
            extensions={
                "used": sorted(document.get("extensionsUsed", [])),
                "required": sorted(document.get("extensionsRequired", [])),
            },
        )

    @staticmethod
    def _decode(data: bytes) -> _Validation:
        if len(data) < 12:
            return _Validation(
                document={},
                binary=b"",
                errors=[
                    {
                        "code": "TRUNCATED_HEADER",
                        "pointer": "/",
                        "message": "GLB header is shorter than 12 bytes.",
                    }
                ],
            )
        magic, version, declared_length = struct.unpack_from("<4sII", data, 0)
        result = _Validation(document={}, binary=b"")
        if magic != _GLB_MAGIC:
            result.error("INVALID_MAGIC", "/", "GLB magic must be glTF.")
        if version != 2:
            result.error("UNSUPPORTED_VERSION", "/", f"GLB version must be 2, observed {version}.")
        if declared_length != len(data):
            result.error(
                "LENGTH_MISMATCH",
                "/",
                f"Header declares {declared_length} bytes, observed {len(data)}.",
            )
        offset = 12
        chunks: list[tuple[int, bytes]] = []
        while offset < len(data):
            if offset + 8 > len(data):
                result.error("TRUNCATED_CHUNK_HEADER", "/", "GLB chunk header is truncated.")
                break
            chunk_length, chunk_type = struct.unpack_from("<II", data, offset)
            offset += 8
            end = offset + chunk_length
            if end > len(data):
                result.error(
                    "TRUNCATED_CHUNK",
                    "/",
                    f"Chunk extends {end - len(data)} bytes past the file.",
                )
                break
            chunks.append((chunk_type, data[offset:end]))
            if chunk_length % 4:
                result.error(
                    "UNALIGNED_CHUNK",
                    "/",
                    f"GLB chunk length must be 4-byte aligned, observed {chunk_length}.",
                )
            offset = end
        if not chunks or chunks[0][0] != _JSON_CHUNK:
            result.error("MISSING_JSON_CHUNK", "/", "First GLB chunk must be JSON.")
            return result
        if sum(chunk_type == _JSON_CHUNK for chunk_type, _chunk in chunks) != 1:
            result.error("DUPLICATE_JSON_CHUNK", "/", "GLB must contain exactly one JSON chunk.")
        if len(chunks) > 2 or any(
            chunk_type not in {_JSON_CHUNK, _BIN_CHUNK} for chunk_type, _chunk in chunks
        ):
            result.error("INVALID_CHUNK_LAYOUT", "/", "GLB may contain JSON then one BIN chunk.")
        if len(chunks) == 2 and chunks[1][0] != _BIN_CHUNK:
            result.error("INVALID_BIN_CHUNK", "/", "Second GLB chunk must be BIN.")
        try:
            decoded = chunks[0][1].decode("utf-8").rstrip(" \t\r\n\x00")
            document = json.loads(decoded)
            if not isinstance(document, dict):
                raise TypeError("root is not an object")
            result.document = document
        except (UnicodeDecodeError, json.JSONDecodeError, TypeError) as error:
            result.error("INVALID_JSON", "/", f"GLB JSON chunk is invalid: {error}")
        if len(chunks) >= 2 and chunks[1][0] == _BIN_CHUNK:
            result.binary = chunks[1][1]
        return result

    def _validate_document(self, value: _Validation) -> None:
        document = value.document
        if not document:
            return
        asset = document.get("asset")
        if not isinstance(asset, dict) or str(asset.get("version")) != "2.0":
            value.error("INVALID_ASSET", "/asset", "asset.version must be 2.0.")
        self._list(document, "buffers", value)
        self._list(document, "bufferViews", value)
        self._list(document, "accessors", value)
        self._list(document, "meshes", value)
        self._list(document, "nodes", value)
        self._list(document, "scenes", value)
        self._list(document, "materials", value)
        self._list(document, "textures", value)
        self._list(document, "images", value)
        self._list(document, "samplers", value)
        self._list(document, "animations", value)
        self._list(document, "skins", value)
        self._validate_buffers(value)
        self._validate_buffer_views(value)
        self._validate_accessors(value)
        self._validate_meshes(value)
        self._validate_nodes_and_scenes(value)
        self._validate_materials_and_images(value)
        self._validate_animations_and_skins(value)
        used = document.get("extensionsUsed", [])
        required = document.get("extensionsRequired", [])
        if not isinstance(used, list) or any(not isinstance(item, str) for item in used):
            value.error("INVALID_EXTENSIONS", "/extensionsUsed", "extensionsUsed must be strings.")
            used = []
        if not isinstance(required, list) or any(not isinstance(item, str) for item in required):
            value.error(
                "INVALID_EXTENSIONS",
                "/extensionsRequired",
                "extensionsRequired must be strings.",
            )
            required = []
        for extension in sorted(set(required) - set(used)):
            value.error(
                "UNDECLARED_REQUIRED_EXTENSION",
                "/extensionsRequired",
                f"Required extension is absent from extensionsUsed: {extension}",
            )

    @staticmethod
    def _list(document: dict[str, Any], key: str, value: _Validation) -> list[Any]:
        items = document.get(key, [])
        if not isinstance(items, list):
            value.error("INVALID_COLLECTION", f"/{key}", f"{key} must be an array.")
            return []
        return items

    def _validate_buffers(self, value: _Validation) -> None:
        buffers = self._list(value.document, "buffers", value)
        if len(buffers) > 1:
            value.error("MULTIPLE_GLB_BUFFERS", "/buffers", "GLB supports one embedded buffer.")
        if not buffers and value.binary:
            value.error("UNDECLARED_BIN_CHUNK", "/buffers", "BIN chunk has no buffer declaration.")
        for index, buffer in enumerate(buffers):
            pointer = f"/buffers/{index}"
            if not isinstance(buffer, dict):
                value.error("INVALID_BUFFER", pointer, "Buffer must be an object.")
                continue
            uri = buffer.get("uri")
            if uri is not None:
                value.error(
                    "EXTERNAL_BUFFER_URI",
                    f"{pointer}/uri",
                    "GLB buffer must use the embedded BIN chunk.",
                )
            length = buffer.get("byteLength")
            if not self._nonnegative_integer(length):
                value.error("INVALID_BYTE_LENGTH", f"{pointer}/byteLength", "Invalid byteLength.")
            elif length > len(value.binary):
                value.error(
                    "BUFFER_OVERRUN",
                    f"{pointer}/byteLength",
                    f"Buffer declares {length} bytes but BIN contains {len(value.binary)}.",
                )

    def _validate_buffer_views(self, value: _Validation) -> None:
        buffers = self._list(value.document, "buffers", value)
        views = self._list(value.document, "bufferViews", value)
        for index, view in enumerate(views):
            pointer = f"/bufferViews/{index}"
            if not isinstance(view, dict):
                value.error("INVALID_BUFFER_VIEW", pointer, "bufferView must be an object.")
                continue
            buffer_index = view.get("buffer")
            offset = view.get("byteOffset", 0)
            length = view.get("byteLength")
            stride = view.get("byteStride")
            if not self._index(buffer_index, buffers):
                value.error("INVALID_BUFFER_INDEX", f"{pointer}/buffer", "Invalid buffer index.")
                continue
            if not self._nonnegative_integer(offset) or not self._nonnegative_integer(length):
                value.error("INVALID_BUFFER_RANGE", pointer, "Invalid bufferView byte range.")
                continue
            if stride is not None and (
                not isinstance(stride, int)
                or isinstance(stride, bool)
                or not 4 <= stride <= 252
                or stride % 4
            ):
                value.error("INVALID_BYTE_STRIDE", f"{pointer}/byteStride", "Invalid byteStride.")
            declared = int(buffers[buffer_index].get("byteLength", 0))
            if offset + length > declared or offset + length > len(value.binary):
                value.error("BUFFER_VIEW_OVERRUN", pointer, "bufferView exceeds embedded buffer.")

    def _validate_accessors(self, value: _Validation) -> None:
        accessors = self._list(value.document, "accessors", value)
        views = self._list(value.document, "bufferViews", value)
        for index, accessor in enumerate(accessors):
            pointer = f"/accessors/{index}"
            if not isinstance(accessor, dict):
                value.error("INVALID_ACCESSOR", pointer, "Accessor must be an object.")
                continue
            view_index = accessor.get("bufferView")
            if view_index is not None and not self._index(view_index, views):
                value.error(
                    "INVALID_BUFFER_VIEW_INDEX",
                    f"{pointer}/bufferView",
                    "Invalid bufferView index.",
                )
            component_type = accessor.get("componentType")
            accessor_type = accessor.get("type")
            count = accessor.get("count")
            offset = accessor.get("byteOffset", 0)
            if component_type not in _COMPONENT_BYTES:
                value.error(
                    "INVALID_COMPONENT_TYPE",
                    f"{pointer}/componentType",
                    "Unsupported accessor componentType.",
                )
            if accessor_type not in _TYPE_COMPONENTS:
                value.error("INVALID_ACCESSOR_TYPE", f"{pointer}/type", "Invalid accessor type.")
            if not self._positive_integer(count):
                value.error("INVALID_ACCESSOR_COUNT", f"{pointer}/count", "Invalid accessor count.")
            if not self._nonnegative_integer(offset):
                value.error("INVALID_ACCESSOR_OFFSET", f"{pointer}/byteOffset", "Invalid offset.")
            if (
                self._index(view_index, views)
                and component_type in _COMPONENT_BYTES
                and accessor_type in _TYPE_COMPONENTS
                and self._positive_integer(count)
                and self._nonnegative_integer(offset)
            ):
                view = views[view_index]
                element_bytes = _COMPONENT_BYTES[component_type] * _TYPE_COMPONENTS[accessor_type]
                stride = int(view.get("byteStride", element_bytes))
                needed = offset + (max(0, count - 1) * stride) + element_bytes
                if needed > int(view.get("byteLength", 0)):
                    value.error("ACCESSOR_OVERRUN", pointer, "Accessor exceeds its bufferView.")
            if "sparse" in accessor:
                value.warning(
                    "SPARSE_ACCESSOR_BOUNDS_NOT_EXPANDED",
                    f"{pointer}/sparse",
                    "Sparse indices are structurally retained but not value-decoded.",
                )
            for bound in ("min", "max"):
                vector = accessor.get(bound)
                if vector is not None and (
                    not isinstance(vector, list)
                    or any(
                        not isinstance(item, int | float)
                        or isinstance(item, bool)
                        or not math.isfinite(float(item))
                        for item in vector
                    )
                ):
                    value.error("INVALID_ACCESSOR_BOUND", f"{pointer}/{bound}", "Invalid bound.")

    def _validate_meshes(self, value: _Validation) -> None:
        accessors = self._list(value.document, "accessors", value)
        materials = self._list(value.document, "materials", value)
        meshes = self._list(value.document, "meshes", value)
        for mesh_index, mesh in enumerate(meshes):
            pointer = f"/meshes/{mesh_index}"
            if not isinstance(mesh, dict):
                value.error("INVALID_MESH", pointer, "Mesh must be an object.")
                continue
            primitives = mesh.get("primitives")
            if not isinstance(primitives, list) or not primitives:
                value.error("EMPTY_MESH", f"{pointer}/primitives", "Mesh needs primitives.")
                continue
            for primitive_index, primitive in enumerate(primitives):
                primitive_pointer = f"{pointer}/primitives/{primitive_index}"
                if not isinstance(primitive, dict):
                    value.error("INVALID_PRIMITIVE", primitive_pointer, "Invalid primitive.")
                    continue
                attributes = primitive.get("attributes")
                if not isinstance(attributes, dict) or not attributes:
                    value.error(
                        "MISSING_ATTRIBUTES",
                        f"{primitive_pointer}/attributes",
                        "Primitive needs attributes.",
                    )
                else:
                    for semantic, accessor_index in attributes.items():
                        if not isinstance(semantic, str) or not self._index(
                            accessor_index, accessors
                        ):
                            value.error(
                                "INVALID_ATTRIBUTE_ACCESSOR",
                                f"{primitive_pointer}/attributes/{semantic}",
                                "Attribute references an invalid accessor.",
                            )
                if "indices" in primitive and not self._index(
                    primitive["indices"], accessors
                ):
                    value.error(
                        "INVALID_INDEX_ACCESSOR",
                        f"{primitive_pointer}/indices",
                        "Indices reference an invalid accessor.",
                    )
                if "material" in primitive and not self._index(
                    primitive["material"], materials
                ):
                    value.error(
                        "INVALID_MATERIAL_INDEX",
                        f"{primitive_pointer}/material",
                        "Material index is invalid.",
                    )
                mode = primitive.get("mode", 4)
                if not isinstance(mode, int) or isinstance(mode, bool) or not 0 <= mode <= 6:
                    value.error("INVALID_PRIMITIVE_MODE", f"{primitive_pointer}/mode", "Bad mode.")

    def _validate_nodes_and_scenes(self, value: _Validation) -> None:
        document = value.document
        nodes = self._list(document, "nodes", value)
        meshes = self._list(document, "meshes", value)
        cameras = self._list(document, "cameras", value)
        skins = self._list(document, "skins", value)
        for index, node in enumerate(nodes):
            pointer = f"/nodes/{index}"
            if not isinstance(node, dict):
                value.error("INVALID_NODE", pointer, "Node must be an object.")
                continue
            for key, collection in (("mesh", meshes), ("camera", cameras), ("skin", skins)):
                if key in node and not self._index(node[key], collection):
                    value.error("INVALID_NODE_REFERENCE", f"{pointer}/{key}", "Bad node reference.")
            children = node.get("children", [])
            if not isinstance(children, list) or any(
                not self._index(child, nodes) for child in children
            ):
                value.error("INVALID_NODE_CHILDREN", f"{pointer}/children", "Bad child index.")
        state = [0] * len(nodes)

        def visit(node_index: int) -> None:
            if state[node_index] == 1:
                value.error("NODE_CYCLE", f"/nodes/{node_index}", "Node graph contains a cycle.")
                return
            if state[node_index] == 2:
                return
            state[node_index] = 1
            node = nodes[node_index]
            if isinstance(node, dict):
                for child in node.get("children", []):
                    if self._index(child, nodes):
                        visit(child)
            state[node_index] = 2

        for index in range(len(nodes)):
            visit(index)
        scenes = self._list(document, "scenes", value)
        for index, scene in enumerate(scenes):
            roots = scene.get("nodes", []) if isinstance(scene, dict) else None
            if not isinstance(roots, list) or any(
                not self._index(node_index, nodes) for node_index in roots
            ):
                value.error("INVALID_SCENE_ROOT", f"/scenes/{index}/nodes", "Bad scene root.")
        if "scene" in document and not self._index(document["scene"], scenes):
            value.error("INVALID_DEFAULT_SCENE", "/scene", "Default scene index is invalid.")

    def _validate_materials_and_images(self, value: _Validation) -> None:
        document = value.document
        textures = self._list(document, "textures", value)
        images = self._list(document, "images", value)
        samplers = self._list(document, "samplers", value)
        views = self._list(document, "bufferViews", value)
        materials = self._list(document, "materials", value)
        for index, texture in enumerate(textures):
            pointer = f"/textures/{index}"
            if not isinstance(texture, dict) or not self._index(texture.get("source"), images):
                value.error("INVALID_TEXTURE_SOURCE", f"{pointer}/source", "Bad image index.")
            if isinstance(texture, dict) and "sampler" in texture and not self._index(
                texture["sampler"], samplers
            ):
                value.error("INVALID_SAMPLER", f"{pointer}/sampler", "Bad sampler index.")
        for index, image in enumerate(images):
            pointer = f"/images/{index}"
            if not isinstance(image, dict):
                value.error("INVALID_IMAGE", pointer, "Image must be an object.")
                continue
            uri, view_index = image.get("uri"), image.get("bufferView")
            if (uri is None) == (view_index is None):
                value.error(
                    "INVALID_IMAGE_SOURCE",
                    pointer,
                    "Image needs exactly one URI or bufferView.",
                )
            if uri is not None:
                if not isinstance(uri, str) or not any(
                    uri.startswith(prefix) for prefix in _DATA_IMAGE_PREFIXES
                ):
                    value.error(
                        "EXTERNAL_IMAGE_URI",
                        f"{pointer}/uri",
                        "Only embedded image data URIs are permitted.",
                    )
                else:
                    try:
                        base64.b64decode(uri.split(",", 1)[1], validate=True)
                    except (binascii.Error, ValueError):
                        value.error("INVALID_DATA_URI", f"{pointer}/uri", "Malformed data URI.")
            if view_index is not None and not self._index(view_index, views):
                value.error(
                    "INVALID_IMAGE_BUFFER_VIEW",
                    f"{pointer}/bufferView",
                    "Bad image bufferView.",
                )
            if view_index is not None and image.get("mimeType") not in {
                "image/png",
                "image/jpeg",
                "image/webp",
            }:
                value.error(
                    "INVALID_IMAGE_MIME",
                    f"{pointer}/mimeType",
                    "Embedded image needs PNG, JPEG, or WebP MIME type.",
                )
        for index, material in enumerate(materials):
            if not isinstance(material, dict):
                value.error("INVALID_MATERIAL", f"/materials/{index}", "Material must be object.")
                continue
            self._walk_texture_indices(
                material,
                textures,
                value,
                pointer=f"/materials/{index}",
            )

    def _validate_animations_and_skins(self, value: _Validation) -> None:
        document = value.document
        accessors = self._list(document, "accessors", value)
        nodes = self._list(document, "nodes", value)
        animations = self._list(document, "animations", value)
        for animation_index, animation in enumerate(animations):
            pointer = f"/animations/{animation_index}"
            if not isinstance(animation, dict):
                value.error("INVALID_ANIMATION", pointer, "Animation must be an object.")
                continue
            samplers = animation.get("samplers", [])
            channels = animation.get("channels", [])
            if not isinstance(samplers, list) or not isinstance(channels, list):
                value.error("INVALID_ANIMATION", pointer, "Animation arrays are invalid.")
                continue
            for index, sampler in enumerate(samplers):
                sampler_pointer = f"{pointer}/samplers/{index}"
                if not isinstance(sampler, dict):
                    value.error("INVALID_ANIMATION_SAMPLER", sampler_pointer, "Bad sampler.")
                    continue
                if not self._index(sampler.get("input"), accessors) or not self._index(
                    sampler.get("output"), accessors
                ):
                    value.error(
                        "INVALID_ANIMATION_ACCESSOR",
                        sampler_pointer,
                        "Animation sampler has an invalid accessor.",
                    )
                if sampler.get("interpolation", "LINEAR") not in _INTERPOLATIONS:
                    value.error(
                        "INVALID_INTERPOLATION",
                        f"{sampler_pointer}/interpolation",
                        "Unsupported interpolation.",
                    )
            for index, channel in enumerate(channels):
                channel_pointer = f"{pointer}/channels/{index}"
                if not isinstance(channel, dict) or not self._index(
                    channel.get("sampler"), samplers
                ):
                    value.error(
                        "INVALID_ANIMATION_CHANNEL",
                        channel_pointer,
                        "Animation channel has an invalid sampler.",
                    )
                    continue
                target = channel.get("target", {})
                if not isinstance(target, dict) or not self._index(target.get("node"), nodes):
                    value.error(
                        "INVALID_ANIMATION_TARGET",
                        f"{channel_pointer}/target",
                        "Animation target node is invalid.",
                    )
                if isinstance(target, dict) and target.get("path") not in _ANIMATION_PATHS:
                    value.error(
                        "INVALID_ANIMATION_PATH",
                        f"{channel_pointer}/target/path",
                        "Animation path is invalid.",
                    )
        skins = self._list(document, "skins", value)
        for index, skin in enumerate(skins):
            pointer = f"/skins/{index}"
            if not isinstance(skin, dict):
                value.error("INVALID_SKIN", pointer, "Skin must be an object.")
                continue
            joints = skin.get("joints")
            if not isinstance(joints, list) or not joints or any(
                not self._index(joint, nodes) for joint in joints
            ):
                value.error("INVALID_SKIN_JOINTS", f"{pointer}/joints", "Invalid joints.")
            if "inverseBindMatrices" in skin and not self._index(
                skin["inverseBindMatrices"], accessors
            ):
                value.error(
                    "INVALID_BIND_MATRICES",
                    f"{pointer}/inverseBindMatrices",
                    "Invalid inverse bind matrices accessor.",
                )

    def _walk_texture_indices(
        self,
        value: Any,
        textures: list[Any],
        validation: _Validation,
        *,
        pointer: str,
    ) -> None:
        if isinstance(value, dict):
            for key, item in value.items():
                child_pointer = f"{pointer}/{key}"
                if key.endswith("Texture") and isinstance(item, dict) and not self._index(
                    item.get("index"), textures
                ):
                    validation.error(
                        "INVALID_TEXTURE_INDEX",
                        f"{child_pointer}/index",
                        "Material texture index is invalid.",
                    )
                self._walk_texture_indices(
                    item,
                    textures,
                    validation,
                    pointer=child_pointer,
                )
        elif isinstance(value, list):
            for index, item in enumerate(value):
                self._walk_texture_indices(
                    item,
                    textures,
                    validation,
                    pointer=f"{pointer}/{index}",
                )

    @staticmethod
    def _index(value: Any, collection: list[Any]) -> bool:
        return (
            isinstance(value, int)
            and not isinstance(value, bool)
            and 0 <= value < len(collection)
        )

    @staticmethod
    def _nonnegative_integer(value: Any) -> bool:
        return isinstance(value, int) and not isinstance(value, bool) and value >= 0

    @staticmethod
    def _positive_integer(value: Any) -> bool:
        return isinstance(value, int) and not isinstance(value, bool) and value > 0
