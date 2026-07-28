from __future__ import annotations

import hashlib
import json
import mimetypes
import shutil
import subprocess
import uuid
from pathlib import Path
from typing import Any

from PIL import ExifTags, Image, ImageFilter, ImageOps, ImageStat

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import utc_now
from blender_vision.projects.store import ProjectStore
from blender_vision.security.paths import safe_filename

IMAGE_TYPES = {"image/jpeg", "image/png", "image/tiff", "image/heic", "image/webp"}
VIDEO_TYPES = {"video/mp4", "video/quicktime", "video/x-matroska"}
MODEL_TYPES = {
    ".blend": "application/x-blender",
    ".glb": "model/gltf-binary",
    ".gltf": "model/gltf+json",
    ".obj": "model/obj",
    ".ply": "model/ply",
    ".stl": "model/stl",
    ".usdz": "model/vnd.usdz+zip",
}


def _json_value(value: Any) -> Any:
    if isinstance(value, (str, int, float, bool)) or value is None:
        return value
    if isinstance(value, bytes):
        return value.hex()
    if isinstance(value, (list, tuple)):
        return [_json_value(item) for item in value]
    return str(value)


def inspect_image(path: Path) -> tuple[dict[str, Any], dict[str, Any]]:
    metadata: dict[str, Any] = {}
    quality: dict[str, Any] = {}
    try:
        with Image.open(path) as image:
            image.load()
            exif = image.getexif()
            named_exif = {
                ExifTags.TAGS.get(key, str(key)): _json_value(value) for key, value in exif.items()
            }
            orientation = int(exif.get(274, 1) or 1)
            analysis_image = ImageOps.exif_transpose(image)
            icc_profile = image.info.get("icc_profile")
            metadata.update(
                {
                    "width": analysis_image.width,
                    "height": analysis_image.height,
                    "source_width": image.width,
                    "source_height": image.height,
                    "format": image.format,
                    "mode": image.mode,
                    "orientation": orientation,
                    "orientation_corrected": orientation not in {0, 1},
                    "camera": {
                        key: named_exif[key]
                        for key in ("Make", "Model", "Software", "DateTimeOriginal")
                        if key in named_exif
                    },
                    "lens": {
                        key: named_exif[key]
                        for key in (
                            "LensMake",
                            "LensModel",
                            "FocalLength",
                            "FocalLengthIn35mmFilm",
                            "FNumber",
                        )
                        if key in named_exif
                    },
                    "color_profile": (
                        {
                            "embedded": True,
                            "size": len(icc_profile),
                            "sha256": hashlib.sha256(icc_profile).hexdigest(),
                        }
                        if isinstance(icc_profile, bytes)
                        else {"embedded": False}
                    ),
                }
            )
            metadata["exif"] = named_exif
            thumbnail = analysis_image.convert("L")
            thumbnail.thumbnail((1024, 1024))
            histogram = thumbnail.histogram()
            pixels = max(1, sum(histogram))
            mean = ImageStat.Stat(thumbnail).mean[0] / 255.0
            edges = thumbnail.filter(ImageFilter.FIND_EDGES)
            edge_variance = ImageStat.Stat(edges).var[0]
            quality.update(
                {
                    "decode_ok": True,
                    "exposure_mean": round(mean, 6),
                    "clipped_black_fraction": round(sum(histogram[:3]) / pixels, 6),
                    "clipped_white_fraction": round(sum(histogram[-3:]) / pixels, 6),
                    "edge_variance": round(edge_variance, 6),
                    "blur_warning": edge_variance < 20.0,
                    "exposure_warning": mean < 0.08 or mean > 0.92,
                }
            )
    except Exception as error:
        quality.update({"decode_ok": False, "error": f"{type(error).__name__}: {error}"})
    return metadata, quality


def inspect_video(path: Path) -> tuple[dict[str, Any], dict[str, Any]]:
    try:
        result = subprocess.run(
            [
                "ffprobe",
                "-v",
                "error",
                "-show_entries",
                "format=duration,format_name:stream=index,codec_type,codec_name,width,height,avg_frame_rate",
                "-of",
                "json",
                str(path),
            ],
            capture_output=True,
            text=True,
            timeout=30,
            check=True,
        )
        return json.loads(result.stdout), {"probe_ok": True}
    except Exception as error:
        return {}, {"probe_ok": False, "error": f"{type(error).__name__}: {error}"}


class ReferenceIngestor:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def import_file(
        self,
        source: Path,
        *,
        rights_state: str = "UNKNOWN",
        viewpoint_label: str | None = None,
        evidence_role: str | None = None,
        acceptance_eligible: bool | None = None,
    ) -> dict[str, Any]:
        source = source.expanduser().resolve()
        media_type = MODEL_TYPES.get(source.suffix.lower()) or (
            mimetypes.guess_type(source.name)[0] or "application/octet-stream"
        )
        artifact = self.artifacts.ingest_file(source, media_type=media_type)
        if evidence_role is None:
            evidence_role = (
                "acceptance_reference" if media_type.startswith("image/") else "source_media"
            )
        if acceptance_eligible is None:
            acceptance_eligible = media_type.startswith("image/")
        metadata: dict[str, Any] = {"source_suffix": source.suffix.lower()}
        quality: dict[str, Any] = {}
        if media_type in IMAGE_TYPES or media_type.startswith("image/"):
            metadata, quality = inspect_image(source)
        elif media_type in VIDEO_TYPES:
            metadata, quality = inspect_video(source)
        else:
            quality = {"inspection": "not_implemented_for_media_type"}
        reference_id = str(uuid.uuid4())
        destination_name = f"{reference_id}_{safe_filename(source.name)}"
        relative_path = Path("references") / "originals" / destination_name
        self.artifacts.materialize(artifact.digest, self.project.root / relative_path)
        with self.project.connection() as connection:
            duplicate = connection.execute(
                "SELECT id FROM reference_items WHERE artifact_digest=? "
                "ORDER BY created_at LIMIT 1",
                (artifact.digest,),
            ).fetchone()
            quality["duplicate_score"] = 1.0 if duplicate else 0.0
            quality["exact_duplicate"] = bool(duplicate)
            connection.execute(
                """
                INSERT INTO reference_items(
                    id,artifact_digest,original_name,media_type,relative_path,metadata_json,
                    quality_json,rights_state,viewpoint_label,duplicate_of,created_at,
                    evidence_role,acceptance_eligible
                ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
                """,
                (
                    reference_id,
                    artifact.digest,
                    source.name,
                    media_type,
                    str(relative_path),
                    json.dumps(metadata),
                    json.dumps(quality),
                    rights_state,
                    viewpoint_label,
                    duplicate["id"] if duplicate else None,
                    utc_now(),
                    evidence_role,
                    int(acceptance_eligible),
                ),
            )
        return {
            "id": reference_id,
            "artifact": artifact.to_dict(),
            "original_name": source.name,
            "media_type": media_type,
            "relative_path": str(relative_path),
            "metadata": metadata,
            "quality": quality,
            "rights_state": rights_state,
            "viewpoint_label": viewpoint_label,
            "duplicate_of": duplicate["id"] if duplicate else None,
            "evidence_role": evidence_role,
            "acceptance_eligible": bool(acceptance_eligible),
        }

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM reference_items ORDER BY created_at"
            ).fetchall()
        items = []
        for row in rows:
            item = dict(row)
            item["metadata"] = json.loads(item.pop("metadata_json"))
            item["quality"] = json.loads(item.pop("quality_json"))
            item["acceptance_eligible"] = bool(item["acceptance_eligible"])
            items.append(item)
        return items

    def import_pdf_pages(
        self,
        source: Path,
        *,
        rights_state: str,
        maximum_pages: int = 200,
        resolution_dpi: int = 200,
    ) -> dict[str, Any]:
        if rights_state == "UNKNOWN":
            raise ValueError("PDF page extraction requires reviewed source rights")
        if not 1 <= maximum_pages <= 1000 or not 72 <= resolution_dpi <= 600:
            raise ValueError("PDF extraction bounds are invalid")
        executable = shutil.which("pdftoppm")
        if executable is None:
            raise RuntimeError("Poppler pdftoppm is required for PDF page extraction")
        source = source.expanduser().resolve()
        parent = self.import_file(source, rights_state=rights_state, viewpoint_label="source PDF")
        output_directory = self.project.root / "references" / "pdf-pages" / str(uuid.uuid4())
        output_directory.mkdir(parents=True, exist_ok=True)
        prefix = output_directory / "page"
        result = subprocess.run(
            [
                executable,
                "-png",
                "-r",
                str(resolution_dpi),
                "-f",
                "1",
                "-l",
                str(maximum_pages),
                str(source),
                str(prefix),
            ],
            capture_output=True,
            text=True,
            timeout=1800,
            check=False,
        )
        if result.returncode != 0:
            raise RuntimeError(f"PDF page extraction failed: {result.stderr[-1000:]}")
        pages = []
        for page_number, path in enumerate(sorted(output_directory.glob("page-*.png")), start=1):
            page = self.import_file(
                path,
                rights_state=rights_state,
                viewpoint_label=f"PDF page {page_number}",
            )
            metadata = {
                **page["metadata"],
                "document_source_reference_id": parent["id"],
                "page_number": page_number,
                "resolution_dpi": resolution_dpi,
            }
            with self.project.connection() as connection:
                connection.execute(
                    "UPDATE reference_items SET metadata_json=? WHERE id=?",
                    (json.dumps(metadata), page["id"]),
                )
            pages.append({**page, "metadata": metadata})
        if not pages:
            raise RuntimeError("PDF extraction produced no pages")
        return {
            "source_reference": parent,
            "pages": pages,
            "page_count": len(pages),
            "maximum_pages": maximum_pages,
            "resolution_dpi": resolution_dpi,
            "network_used": False,
        }
