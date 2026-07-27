"""Web scene delivery compiler: LODs, measured compression, streaming manifests."""

from __future__ import annotations

from blender_vision.delivery.compress import (
    CompressionCandidate,
    CompressionSelection,
    measure_and_select_compression,
)
from blender_vision.delivery.lods import LodLevel, LodReport, generate_lods
from blender_vision.delivery.manifest import (
    FROZEN_BUDGETS,
    build_delivery_manifest,
    evaluate_budgets,
)
from blender_vision.delivery.stream import StreamingPlan, build_streaming_plan

__all__ = [
    "CompressionCandidate",
    "CompressionSelection",
    "FROZEN_BUDGETS",
    "LodLevel",
    "LodReport",
    "StreamingPlan",
    "build_delivery_manifest",
    "build_streaming_plan",
    "evaluate_budgets",
    "generate_lods",
    "measure_and_select_compression",
]
