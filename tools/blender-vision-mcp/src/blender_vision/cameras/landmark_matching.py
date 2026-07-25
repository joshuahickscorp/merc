from __future__ import annotations

import hashlib
import json
import math
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.cameras.landmarks import CameraLandmarkStore
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.projects.store import ProjectStore

DEFAULT_CONFIG: dict[str, float | int] = {
    "maximum_target_dimension": 1800,
    "pyramid_target_dimension": 900,
    "pyramid_render_dimension": 1024,
    "ratio_test": 0.72,
    "ransac_threshold_px": 4.0,
    "minimum_good_matches": 12,
    "minimum_inliers": 8,
    "minimum_inlier_ratio": 0.28,
    "minimum_target_coverage": 0.01,
    "minimum_render_coverage": 0.01,
    "minimum_projected_anchors": 6,
    "maximum_projected_anchors": 24,
}


def _cv() -> tuple[Any, Any]:
    try:
        import cv2
        import numpy
    except ImportError as error:  # pragma: no cover - exercised without the vision extra
        raise RuntimeError(
            "render landmark matching requires the optional 'vision' dependencies"
        ) from error
    return cv2, numpy


def _configuration(overrides: dict[str, Any] | None) -> dict[str, float | int]:
    config = dict(DEFAULT_CONFIG)
    unknown = set(overrides or {}) - set(config)
    if unknown:
        raise ValueError(f"unknown landmark matcher settings: {sorted(unknown)}")
    config.update(overrides or {})
    integer_keys = {
        "maximum_target_dimension",
        "pyramid_target_dimension",
        "pyramid_render_dimension",
        "minimum_good_matches",
        "minimum_inliers",
        "minimum_projected_anchors",
        "maximum_projected_anchors",
    }
    for key in integer_keys:
        value = config[key]
        if not isinstance(value, int) or value <= 0:
            raise ValueError(f"{key} must be a positive integer")
    if not 256 <= int(config["maximum_target_dimension"]) <= 8192:
        raise ValueError("maximum_target_dimension must be between 256 and 8192")
    if not 256 <= int(config["pyramid_target_dimension"]) <= int(
        config["maximum_target_dimension"]
    ):
        raise ValueError(
            "pyramid_target_dimension must be between 256 and maximum_target_dimension"
        )
    if not 256 <= int(config["pyramid_render_dimension"]) <= 8192:
        raise ValueError("pyramid_render_dimension must be between 256 and 8192")
    if int(config["maximum_projected_anchors"]) < int(
        config["minimum_projected_anchors"]
    ):
        raise ValueError(
            "maximum_projected_anchors must be at least minimum_projected_anchors"
        )
    for key in ("ratio_test", "minimum_inlier_ratio"):
        value = float(config[key])
        if not 0.0 < value < 1.0:
            raise ValueError(f"{key} must be between zero and one")
    for key in ("minimum_target_coverage", "minimum_render_coverage"):
        value = float(config[key])
        if not 0.0 < value <= 1.0:
            raise ValueError(f"{key} must be between zero and one")
    threshold = float(config["ransac_threshold_px"])
    if not math.isfinite(threshold) or not 0.1 <= threshold <= 50.0:
        raise ValueError("ransac_threshold_px must be between 0.1 and 50")
    return config


def _matrix4(value: Any) -> list[list[float]]:
    if not isinstance(value, list) or len(value) != 4:
        raise ValueError("model_to_world_mm must be a numeric 4x4 matrix")
    matrix = []
    for row in value:
        if (
            not isinstance(row, list)
            or len(row) != 4
            or not all(isinstance(item, (int, float)) for item in row)
        ):
            raise ValueError("model_to_world_mm must be a numeric 4x4 matrix")
        numeric = [float(item) for item in row]
        if not all(math.isfinite(item) for item in numeric):
            raise ValueError("model_to_world_mm contains a non-finite value")
        matrix.append(numeric)
    if matrix[3] != [0.0, 0.0, 0.0, 1.0]:
        raise ValueError("model_to_world_mm must be an affine transform")
    return matrix


def _world_point(matrix: list[list[float]], point: list[float]) -> list[float]:
    homogeneous = [float(point[0]), float(point[1]), float(point[2]), 1.0]
    return [
        sum(matrix[row][column] * homogeneous[column] for column in range(4))
        for row in range(3)
    ]


def _read_grayscale(path: Path) -> Any:
    cv2, numpy = _cv()
    data = numpy.fromfile(path, dtype=numpy.uint8)
    image = cv2.imdecode(data, cv2.IMREAD_GRAYSCALE)
    if image is None or image.size == 0:
        raise ValueError(f"could not decode image: {path}")
    return image


def _hull_coverage(points: Any, width: int, height: int) -> float:
    cv2, _ = _cv()
    if len(points) < 3:
        return 0.0
    hull = cv2.convexHull(points.astype("float32"))
    return float(cv2.contourArea(hull)) / float(max(1, width * height))


def _resize_level(image: Any, dimension: int, *, allow_upscale: bool) -> tuple[Any, float]:
    cv2, _ = _cv()
    height, width = image.shape[:2]
    scale = float(dimension) / float(max(width, height))
    if not allow_upscale:
        scale = min(1.0, scale)
    if abs(scale - 1.0) < 1e-6:
        return image, 1.0
    interpolation = cv2.INTER_CUBIC if scale > 1.0 else cv2.INTER_AREA
    return cv2.resize(image, None, fx=scale, fy=scale, interpolation=interpolation), scale


def _target_levels(
    image: Any, *, maximum_dimension: int, pyramid_dimension: int
) -> list[tuple[str, Any, float]]:
    primary, primary_scale = _resize_level(
        image, maximum_dimension, allow_upscale=False
    )
    levels = [("primary", primary, primary_scale)]
    pyramid, pyramid_scale = _resize_level(
        image, pyramid_dimension, allow_upscale=False
    )
    if abs(pyramid_scale - primary_scale) > 1e-6:
        levels.append(("pyramid", pyramid, pyramid_scale))
    return levels


def _render_levels(image: Any, *, pyramid_dimension: int) -> list[tuple[str, Any, float]]:
    levels = [("native", image, 1.0)]
    pyramid, pyramid_scale = _resize_level(
        image, pyramid_dimension, allow_upscale=True
    )
    if abs(pyramid_scale - 1.0) > 1e-6:
        levels.append(("pyramid", pyramid, pyramid_scale))
    return levels


def _viewpoint_policy(
    views: list[dict[str, Any]], hint: str | None
) -> tuple[set[str], dict[str, Any]]:
    all_ids = {str(view.get("id", "")) for view in views}
    normalized = " ".join(str(hint or "").lower().replace("-", " ").split())
    rule = None
    if any(token in normalized for token in ("underbody", "underside", "inverted", "bottom")):
        rule = "underbody"
    elif any(token in normalized for token in ("port side", "portside", "left side")):
        rule = "port_side"
    elif any(token in normalized for token in ("starboard", "right side")):
        rule = "starboard_side"
    elif "rear" in normalized:
        rule = "rear"
    elif "front" in normalized:
        rule = "front"
    elif any(token in normalized for token in ("top", "overhead")):
        rule = "top"

    def compatible_id(view_id: str) -> bool:
        if rule == "underbody":
            return view_id.startswith("bottom")
        if rule == "port_side":
            return "left" in view_id and not view_id.startswith("bottom")
        if rule == "starboard_side":
            return "right" in view_id and not view_id.startswith("bottom")
        if rule == "rear":
            return "rear" in view_id
        if rule == "front":
            return "front" in view_id
        if rule == "top":
            return view_id.startswith("top")
        return False

    compatible = {view_id for view_id in all_ids if compatible_id(view_id)}
    fallback = bool(rule and not compatible)
    allowed = all_ids if not rule or fallback else compatible
    return allowed, {
        "hint": str(hint).strip() if hint else None,
        "normalized_hint": normalized or None,
        "rule": rule,
        "fallback_to_all_views": fallback,
        "allowed_view_ids": sorted(allowed),
        "authority": "GOVERNED_SOURCE_VIEWPOINT_PROPOSAL_FILTER_ONLY",
    }


def _select_diverse_correspondences(
    candidates: list[dict[str, Any]], limit: int
) -> list[dict[str, Any]]:
    if len(candidates) <= limit:
        return candidates
    image_width = max(
        1e-9,
        max(item["_work_pixel"][0] for item in candidates)
        - min(item["_work_pixel"][0] for item in candidates),
    )
    image_height = max(
        1e-9,
        max(item["_work_pixel"][1] for item in candidates)
        - min(item["_work_pixel"][1] for item in candidates),
    )
    world_extents = [
        max(
            1e-9,
            max(item["world_mm"][axis] for item in candidates)
            - min(item["world_mm"][axis] for item in candidates),
        )
        for axis in range(3)
    ]

    def distance(left: dict[str, Any], right: dict[str, Any]) -> float:
        image_distance = math.hypot(
            (left["_work_pixel"][0] - right["_work_pixel"][0]) / image_width,
            (left["_work_pixel"][1] - right["_work_pixel"][1]) / image_height,
        )
        world_distance = math.sqrt(
            sum(
                (
                    (left["world_mm"][axis] - right["world_mm"][axis])
                    / world_extents[axis]
                )
                ** 2
                for axis in range(3)
            )
        )
        return image_distance + world_distance

    remaining = sorted(
        candidates,
        key=lambda item: (-float(item["confidence"]), str(item["landmark_id"])),
    )
    selected = [remaining.pop(0)]
    while remaining and len(selected) < limit:
        choice = max(
            remaining,
            key=lambda item: (
                min(distance(item, existing) for existing in selected)
                + 0.1 * float(item["confidence"]),
                float(item["confidence"]),
                str(item["landmark_id"]),
            ),
        )
        selected.append(choice)
        remaining.remove(choice)
    return selected


class RenderLandmarkMatcher:
    """Turn hash-bound synthetic renders into review-required 2D/3D proposals."""

    def __init__(self, project: ProjectStore | None = None):
        self.project = project

    def match(
        self,
        *,
        render_manifest_path: Path,
        target_image_path: Path,
        model_to_world_mm: list[list[float]],
        config: dict[str, Any] | None = None,
        viewpoint_hint: str | None = None,
    ) -> dict[str, Any]:
        cv2, numpy = _cv()
        matcher_config = _configuration(config)
        transform = _matrix4(model_to_world_mm)
        manifest_path = render_manifest_path.expanduser().resolve()
        target_path = target_image_path.expanduser().resolve()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        if manifest.get("schema_version") != 2:
            raise ValueError("render landmark manifest must use hash-bound schema version 2")
        if manifest.get("authority") != "SYNTHETIC_VIEW_FOR_LANDMARK_PROPOSAL_ONLY":
            raise ValueError("render landmark manifest has invalid authority")
        if not isinstance(manifest.get("views"), list) or not manifest["views"]:
            raise ValueError("render landmark manifest contains no views")
        allowed_view_ids, viewpoint_policy = _viewpoint_policy(
            manifest["views"], viewpoint_hint
        )
        model_path = Path(str(manifest.get("source_model", ""))).expanduser().resolve()
        anchor_path = Path(str(manifest.get("source_anchor_manifest", ""))).expanduser().resolve()
        for path, field in (
            (model_path, "source_model_sha256"),
            (anchor_path, "source_anchor_manifest_sha256"),
        ):
            if not path.is_file() or sha256_file(path)[0] != manifest.get(field):
                raise ValueError(f"render landmark manifest failed {field} verification")

        target_native = _read_grayscale(target_path)
        native_height, native_width = target_native.shape[:2]
        detector = cv2.SIFT_create(nfeatures=10000)
        target_features = []
        for level_name, target_image, target_scale in _target_levels(
            target_native,
            maximum_dimension=int(matcher_config["maximum_target_dimension"]),
            pyramid_dimension=int(matcher_config["pyramid_target_dimension"]),
        ):
            keypoints, descriptors = detector.detectAndCompute(target_image, None)
            target_features.append(
                {
                    "level": level_name,
                    "image": target_image,
                    "scale": target_scale,
                    "keypoints": keypoints,
                    "descriptors": descriptors,
                }
            )
        if not any(
            item["descriptors"] is not None and len(item["keypoints"]) >= 4
            for item in target_features
        ):
            return self._refusal(
                manifest_path,
                target_path,
                matcher_config,
                native_width,
                native_height,
                "target image has insufficient local features",
                [],
                viewpoint_policy,
            )

        diagnostics = []
        eligible: list[
            tuple[float, dict[str, Any], Any, Any, Any, dict[str, Any], float, float]
        ] = []
        base_directory = manifest_path.parent
        for view in manifest["views"]:
            view_id = str(view.get("id", ""))
            if view_id not in allowed_view_ids:
                diagnostics.append(
                    {
                        "view_id": view_id,
                        "render_keypoints": 0,
                        "target_keypoints": 0,
                        "good_matches": 0,
                        "inliers": 0,
                        "eligible": False,
                        "refusal_reasons": [
                            "governed source viewpoint excludes this render orientation"
                        ],
                        "attempts": [],
                    }
                )
                continue
            image_path = (base_directory / str(view.get("image", ""))).resolve()
            if image_path.parent != base_directory or not image_path.is_file():
                raise ValueError(
                    "render view path escapes or is absent from its manifest directory"
                )
            if sha256_file(image_path)[0] != view.get("image_sha256"):
                raise ValueError(f"render view failed digest verification: {view.get('id')}")
            render_native = _read_grayscale(image_path)
            attempts: list[dict[str, Any]] = []
            passing_attempts: list[
                tuple[float, Any, Any, Any, dict[str, Any], float, float]
            ] = []
            for render_level, render_image, render_scale in _render_levels(
                render_native,
                pyramid_dimension=int(matcher_config["pyramid_render_dimension"]),
            ):
                render_height, render_width = render_image.shape[:2]
                render_keypoints, render_descriptors = detector.detectAndCompute(
                    render_image, None
                )
                for target_feature in target_features:
                    target_image = target_feature["image"]
                    target_height, target_width = target_image.shape[:2]
                    target_keypoints = target_feature["keypoints"]
                    target_descriptors = target_feature["descriptors"]
                    attempt: dict[str, Any] = {
                        "render_level": render_level,
                        "target_level": target_feature["level"],
                        "render_scale": round(float(render_scale), 9),
                        "target_scale": round(float(target_feature["scale"]), 9),
                        "render_work_size": [render_width, render_height],
                        "target_work_size": [target_width, target_height],
                        "render_keypoints": len(render_keypoints),
                        "target_keypoints": len(target_keypoints),
                        "good_matches": 0,
                        "inliers": 0,
                        "eligible": False,
                        "refusal_reasons": [],
                    }
                    if render_descriptors is None or len(render_keypoints) < 4:
                        attempt["refusal_reasons"].append("insufficient render features")
                        attempts.append(attempt)
                        continue
                    if target_descriptors is None or len(target_keypoints) < 4:
                        attempt["refusal_reasons"].append("insufficient target features")
                        attempts.append(attempt)
                        continue
                    pairs = cv2.BFMatcher(cv2.NORM_L2).knnMatch(
                        render_descriptors, target_descriptors, k=2
                    )
                    good = [
                        first
                        for pair in pairs
                        if len(pair) == 2
                        for first, second in [pair]
                        if first.distance
                        < float(matcher_config["ratio_test"]) * second.distance
                    ]
                    attempt["good_matches"] = len(good)
                    if len(good) < int(matcher_config["minimum_good_matches"]):
                        attempt["refusal_reasons"].append("too few ratio-test matches")
                        attempts.append(attempt)
                        continue
                    source_points = numpy.float32(
                        [render_keypoints[item.queryIdx].pt for item in good]
                    ).reshape(-1, 1, 2)
                    target_points = numpy.float32(
                        [target_keypoints[item.trainIdx].pt for item in good]
                    ).reshape(-1, 1, 2)
                    estimators = [("RANSAC", cv2.RANSAC)]
                    if hasattr(cv2, "USAC_MAGSAC"):
                        estimators.append(("USAC_MAGSAC", cv2.USAC_MAGSAC))
                    estimates = []
                    for estimator_name, estimator in estimators:
                        homography, mask = cv2.findHomography(
                            source_points,
                            target_points,
                            estimator,
                            float(matcher_config["ransac_threshold_px"]),
                        )
                        if (
                            homography is None
                            or mask is None
                            or not numpy.isfinite(homography).all()
                        ):
                            continue
                        inlier_mask = mask.ravel().astype(bool)
                        inliers = int(inlier_mask.sum())
                        inlier_ratio = inliers / max(1, len(good))
                        projected_matches = cv2.perspectiveTransform(
                            source_points, homography
                        )
                        errors = numpy.linalg.norm(
                            projected_matches - target_points, axis=2
                        ).ravel()
                        median_error = (
                            float(numpy.median(errors[inlier_mask]))
                            if inliers
                            else math.inf
                        )
                        target_coverage = _hull_coverage(
                            target_points.reshape(-1, 2)[inlier_mask],
                            target_width,
                            target_height,
                        )
                        render_coverage = _hull_coverage(
                            source_points.reshape(-1, 2)[inlier_mask],
                            render_width,
                            render_height,
                        )
                        checks = (
                            (
                                inliers >= int(matcher_config["minimum_inliers"]),
                                "too few robust-estimator inliers",
                            ),
                            (
                                inlier_ratio
                                >= float(matcher_config["minimum_inlier_ratio"]),
                                "inlier ratio below threshold",
                            ),
                            (
                                target_coverage
                                >= float(matcher_config["minimum_target_coverage"]),
                                "target inlier support is too localized",
                            ),
                            (
                                render_coverage
                                >= float(matcher_config["minimum_render_coverage"]),
                                "render inlier support is too localized",
                            ),
                        )
                        refusal_reasons = [
                            reason for passed, reason in checks if not passed
                        ]
                        score = (
                            inliers
                            * inlier_ratio
                            * math.sqrt(target_coverage * render_coverage)
                            if not refusal_reasons
                            else 0.0
                        )
                        estimates.append(
                            {
                                "estimator": estimator_name,
                                "homography": homography,
                                "inlier_mask": inlier_mask,
                                "inliers": inliers,
                                "inlier_ratio": inlier_ratio,
                                "median_error": median_error,
                                "target_coverage": target_coverage,
                                "render_coverage": render_coverage,
                                "refusal_reasons": refusal_reasons,
                                "score": score,
                            }
                        )
                    if not estimates:
                        attempt["refusal_reasons"].append("homography recovery failed")
                        attempts.append(attempt)
                        continue
                    estimate = max(
                        estimates,
                        key=lambda item: (
                            not item["refusal_reasons"],
                            item["score"],
                            item["inliers"],
                            item["inlier_ratio"],
                            item["target_coverage"] * item["render_coverage"],
                        ),
                    )
                    inlier_mask = estimate["inlier_mask"]
                    attempt.update(
                        {
                            "estimator": estimate["estimator"],
                            "inliers": estimate["inliers"],
                            "inlier_ratio": round(estimate["inlier_ratio"], 6),
                            "median_inlier_reprojection_error_px": round(
                                estimate["median_error"], 6
                            ),
                            "target_hull_coverage": round(
                                estimate["target_coverage"], 6
                            ),
                            "render_hull_coverage": round(
                                estimate["render_coverage"], 6
                            ),
                            "estimator_candidates": [
                                {
                                    "estimator": item["estimator"],
                                    "inliers": item["inliers"],
                                    "inlier_ratio": round(item["inlier_ratio"], 6),
                                    "target_hull_coverage": round(
                                        item["target_coverage"], 6
                                    ),
                                    "render_hull_coverage": round(
                                        item["render_coverage"], 6
                                    ),
                                    "refusal_reasons": item["refusal_reasons"],
                                }
                                for item in estimates
                            ],
                        }
                    )
                    attempt["refusal_reasons"] = estimate["refusal_reasons"]
                    if not attempt["refusal_reasons"]:
                        score = estimate["score"]
                        attempt["eligible"] = True
                        attempt["score"] = round(score, 6)
                        passing_attempts.append(
                            (
                                score,
                                estimate["homography"],
                                source_points[inlier_mask],
                                target_points[inlier_mask],
                                attempt,
                                float(render_scale),
                                float(target_feature["scale"]),
                            )
                        )
                    attempts.append(attempt)

            def attempt_rank(item: dict[str, Any]) -> tuple[Any, ...]:
                return (
                    bool(item["eligible"]),
                    float(item.get("score", 0.0)),
                    int(item["inliers"]),
                    float(item.get("inlier_ratio", 0.0)),
                    float(item.get("target_hull_coverage", 0.0))
                    * float(item.get("render_hull_coverage", 0.0)),
                    int(item["good_matches"]),
                )

            best_attempt = max(attempts, key=attempt_rank)
            diagnostic = {
                "view_id": str(view.get("id", "")),
                **best_attempt,
                "attempts": attempts,
            }
            diagnostics.append(diagnostic)
            if passing_attempts:
                (
                    score,
                    homography,
                    inlier_sources,
                    inlier_targets,
                    _,
                    render_scale,
                    target_scale,
                ) = max(passing_attempts, key=lambda item: item[0])
                eligible.append(
                    (
                        score,
                        view,
                        homography,
                        inlier_sources,
                        inlier_targets,
                        diagnostic,
                        render_scale,
                        target_scale,
                    )
                )

        if not eligible:
            return self._refusal(
                manifest_path,
                target_path,
                matcher_config,
                native_width,
                native_height,
                "no render view passed geometric match thresholds",
                diagnostics,
                viewpoint_policy,
            )
        (
            _,
            best_view,
            homography,
            inlier_sources,
            inlier_targets,
            best_diagnostic,
            render_scale,
            target_scale,
        ) = max(
            eligible, key=lambda item: item[0]
        )
        render_anchors = best_view.get("anchors")
        if not isinstance(render_anchors, list):
            raise ValueError("selected render view contains no projected anchor list")
        anchor_pixels = numpy.float32(
            [
                [float(value) * render_scale for value in item["render_px"]]
                for item in render_anchors
            ]
        ).reshape(-1, 1, 2)
        target_anchor_pixels = cv2.perspectiveTransform(anchor_pixels, homography).reshape(-1, 2)
        target_inlier_support = inlier_targets.reshape(-1, 2)
        render_inlier_support = inlier_sources.reshape(-1, 2)
        render_support_hull = cv2.convexHull(render_inlier_support.astype("float32"))
        render_width, render_height = best_diagnostic["render_work_size"]
        target_width, target_height = best_diagnostic["target_work_size"]
        target_diagonal = math.hypot(target_width, target_height)
        render_diagonal = math.hypot(render_width, render_height)
        render_support_margin = 0.03 * render_diagonal
        base_confidence = min(
            0.95,
            0.3
            + 0.35 * float(best_diagnostic["inlier_ratio"])
            + 0.2 * min(1.0, float(best_diagnostic["target_hull_coverage"]) / 0.2)
            + 0.1 * min(1.0, int(best_diagnostic["inliers"]) / 30.0),
        )
        correspondences = []
        for anchor, render_pixel, work_pixel in zip(
            render_anchors,
            anchor_pixels.reshape(-1, 2),
            target_anchor_pixels,
            strict=True,
        ):
            x, y = float(work_pixel[0]), float(work_pixel[1])
            if not (math.isfinite(x) and math.isfinite(y)):
                continue
            if not 0.0 <= x < target_width or not 0.0 <= y < target_height:
                continue
            hull_distance = float(
                cv2.pointPolygonTest(
                    render_support_hull,
                    (float(render_pixel[0]), float(render_pixel[1])),
                    True,
                )
            )
            if hull_distance < -render_support_margin:
                continue
            nearest_render = float(
                numpy.linalg.norm(render_inlier_support - render_pixel, axis=1).min()
            )
            nearest_target = float(
                numpy.linalg.norm(target_inlier_support - work_pixel, axis=1).min()
            )
            render_support = max(
                0.15, 1.0 - 3.0 * nearest_render / max(1.0, render_diagonal)
            )
            target_support = max(
                0.15, 1.0 - 3.0 * nearest_target / max(1.0, target_diagonal)
            )
            support = min(render_support, target_support)
            correspondences.append(
                {
                    "landmark_id": str(anchor["landmark_id"]),
                    "world_mm": _world_point(transform, anchor["world_model_units"]),
                    "image_px": [x / target_scale, y / target_scale],
                    "confidence": round(min(0.95, base_confidence * support), 6),
                    "method": (
                        "sift_multiscale_viewpoint_robust_homography_support_proposal"
                    ),
                    "_work_pixel": [x, y],
                }
            )
        support_constrained_count = len(correspondences)
        correspondences = _select_diverse_correspondences(
            correspondences, int(matcher_config["maximum_projected_anchors"])
        )
        for correspondence in correspondences:
            correspondence.pop("_work_pixel", None)
        best_diagnostic["support_constrained_projected_anchor_count"] = (
            support_constrained_count
        )
        best_diagnostic["diverse_projected_anchor_count"] = len(correspondences)
        best_diagnostic["render_support_margin_px"] = round(render_support_margin, 6)
        if len(correspondences) < int(matcher_config["minimum_projected_anchors"]):
            return self._refusal(
                manifest_path,
                target_path,
                matcher_config,
                native_width,
                native_height,
                "selected view projects too few anchors inside the target image",
                diagnostics,
                viewpoint_policy,
            )
        return {
            "schema_version": 1,
            "status": "MATCH_PROPOSAL",
            "authority": "MACHINE_PROPOSAL_NOT_REVIEWED",
            "camera_acceptance_performed": False,
            "render_manifest": str(manifest_path),
            "render_manifest_sha256": sha256_file(manifest_path)[0],
            "target_image": str(target_path),
            "target_image_sha256": sha256_file(target_path)[0],
            "target_size": [native_width, native_height],
            "selected_view_id": best_view["id"],
            "selected_matching_scale": {
                "render_scale": round(render_scale, 9),
                "target_scale": round(target_scale, 9),
                "render_work_size": best_diagnostic["render_work_size"],
                "target_work_size": best_diagnostic["target_work_size"],
            },
            "viewpoint_policy": viewpoint_policy,
            "configuration": matcher_config,
            "diagnostics": diagnostics,
            "correspondences": correspondences,
            "known_limitations": [
                "a single render-to-image homography is only a proposal for a non-planar object",
                "synthetic-to-photographic domain shift can produce coherent but incorrect matches",
                "anchors are retained only inside or near the robust render inlier support hull",
                "every 2D/3D correspondence requires immutable named review before PnP",
                "the supplied model-to-millimetre transform remains an external evidence claim",
            ],
        }

    @staticmethod
    def _refusal(
        manifest_path: Path,
        target_path: Path,
        config: dict[str, float | int],
        width: int,
        height: int,
        reason: str,
        diagnostics: list[dict[str, Any]],
        viewpoint_policy: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        return {
            "schema_version": 1,
            "status": "REFUSED",
            "authority": "NO_LANDMARK_AUTHORITY",
            "camera_acceptance_performed": False,
            "reason": reason,
            "render_manifest": str(manifest_path),
            "render_manifest_sha256": sha256_file(manifest_path)[0],
            "target_image": str(target_path),
            "target_image_sha256": sha256_file(target_path)[0],
            "target_size": [width, height],
            "viewpoint_policy": viewpoint_policy,
            "configuration": config,
            "diagnostics": diagnostics,
            "correspondences": [],
        }

    def propose(
        self,
        *,
        target_id: str,
        model_source_id: str,
        image_source_ids: list[str],
        intrinsics_solution_id: str,
        evidence_binding_ids: list[str],
        render_manifest_path: Path,
        model_to_world_mm: list[list[float]],
        known_limitations: list[str],
        config: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        if self.project is None:
            raise ValueError("governed landmark proposals require a project")
        if not image_source_ids:
            raise ValueError("render matching requires at least one image source")
        acquisition = EvidenceAcquisitionStore(self.project)
        model_source = acquisition.get(model_source_id)
        transform = _matrix4(model_to_world_mm)
        transform_sha256 = hashlib.sha256(canonical_json(transform)).hexdigest()
        manifest_path = render_manifest_path.expanduser().resolve()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        if manifest.get("source_model_sha256") != model_source["source"].get("content_hash"):
            raise ValueError("render manifest is not bound to the governed model source")
        results = []
        views = []
        for source_id in image_source_ids:
            source = acquisition.get(source_id)
            if source["target_id"] != target_id or source["status"] != "ACQUIRED":
                raise ValueError("landmark image must be acquired for the exact target")
            with self.project.connection() as connection:
                row = connection.execute(
                    "SELECT relative_path FROM reference_items WHERE id=?",
                    (source["reference_id"],),
                ).fetchone()
            if row is None:
                raise ValueError("landmark image source has no materialized reference")
            result = self.match(
                render_manifest_path=manifest_path,
                target_image_path=self.project.root / row["relative_path"],
                model_to_world_mm=model_to_world_mm,
                config=config,
                viewpoint_hint=source["source"].get("viewpoint"),
            )
            result["image_source_id"] = source_id
            result["reference_id"] = source["reference_id"]
            results.append(result)
            if result["status"] == "MATCH_PROPOSAL":
                views.append(
                    {
                        "image_source_id": source_id,
                        "correspondences": result["correspondences"],
                    }
                )
        report_id = str(uuid.uuid4())
        report = {
            "schema_version": 1,
            "receipt_type": "render_landmark_match_diagnostic",
            "id": report_id,
            "target_id": target_id,
            "model_source_id": model_source_id,
            "render_manifest_sha256": sha256_file(manifest_path)[0],
            "model_to_world_mm": transform,
            "model_to_world_sha256": transform_sha256,
            "authority": "MACHINE_PROPOSAL_OR_REFUSAL_ONLY",
            "camera_acceptance_performed": False,
            "results": results,
            "created_at": utc_now(),
        }
        report_path = self.project.root / "comparisons" / f"render-landmark-match-{report_id}.json"
        atomic_write_json(report_path, report)
        report_artifact = ArtifactStore(self.project).ingest_file(
            report_path,
            media_type="application/vnd.bvmcp.render-landmark-match+json",
        )
        refused = [item for item in results if item["status"] != "MATCH_PROPOSAL"]
        if refused:
            return {
                "status": "REFUSED",
                "reason": "one or more image sources did not pass landmark match thresholds",
                "diagnostic_report": str(report_path.relative_to(self.project.root)),
                "diagnostic_digest": report_artifact.digest,
                "results": results,
                "camera_acceptance_performed": False,
            }
        proposal = CameraLandmarkStore(self.project).propose(
            target_id=target_id,
            model_source_id=model_source_id,
            intrinsics_solution_id=intrinsics_solution_id,
            evidence_binding_ids=evidence_binding_ids,
            views=views,
            backend_identity={
                "name": "opencv-sift-multiscale-render-homography",
                "version": 2,
                "render_manifest_sha256": sha256_file(manifest_path)[0],
                "diagnostic_digest": report_artifact.digest,
                "model_to_world_mm": transform,
                "model_to_world_sha256": transform_sha256,
                "configuration": _configuration(config),
            },
            known_limitations=list(known_limitations)
            + [item for item in results[0]["known_limitations"]],
        )
        return {
            "status": "PROPOSED",
            "proposal": proposal,
            "diagnostic_report": str(report_path.relative_to(self.project.root)),
            "diagnostic_digest": report_artifact.digest,
            "results": results,
            "camera_acceptance_performed": False,
        }
