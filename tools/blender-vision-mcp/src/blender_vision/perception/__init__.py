"""Evidence-governed perception capture and query services."""

from blender_vision.perception.browser import BrowserAdapter
from blender_vision.perception.bus import AdapterRegistry, CaptureBus
from blender_vision.perception.capsules import (
    ExperienceIRCompiler,
    FeatureCapsuleCompiler,
    FeatureCapsuleVerifier,
)
from blender_vision.perception.design import (
    DesignIntelligenceService,
    FigmaExportAdapter,
    StorybookExportAdapter,
)
from blender_vision.perception.experience import BrowserExperienceAdapter, MotionGraphReplay
from blender_vision.perception.graphics import (
    GraphicsRoundTripService,
    GraphicsRuntimeAdapter,
    RuntimeGltfCompiler,
)
from blender_vision.perception.media import (
    CameraFrameAdapter,
    DesktopSnapshotAdapter,
    ImageFileAdapter,
    VideoFileAdapter,
)
from blender_vision.perception.query import ObservationQueryService
from blender_vision.perception.repair import (
    FrontendComparisonService,
    FrontendRepairService,
)

__all__ = [
    "AdapterRegistry",
    "BrowserAdapter",
    "BrowserExperienceAdapter",
    "CameraFrameAdapter",
    "CaptureBus",
    "DesignIntelligenceService",
    "DesktopSnapshotAdapter",
    "ExperienceIRCompiler",
    "FeatureCapsuleCompiler",
    "FeatureCapsuleVerifier",
    "FigmaExportAdapter",
    "FrontendComparisonService",
    "FrontendRepairService",
    "GraphicsRoundTripService",
    "GraphicsRuntimeAdapter",
    "ImageFileAdapter",
    "MotionGraphReplay",
    "ObservationQueryService",
    "RuntimeGltfCompiler",
    "StorybookExportAdapter",
    "VideoFileAdapter",
]
