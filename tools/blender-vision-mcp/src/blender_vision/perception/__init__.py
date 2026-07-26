"""Evidence-governed perception capture and query services."""

from blender_vision.perception.browser import BrowserAdapter
from blender_vision.perception.browser_benchmark import (
    BrowserBenchmarkReceipt,
    BrowserBenchmarkRunner,
    load_browser_benchmark_manifest,
)
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
from blender_vision.perception.learning import PerceptionLearningService
from blender_vision.perception.media import (
    CameraFrameAdapter,
    DesktopSnapshotAdapter,
    ImageFileAdapter,
    LiveCameraAdapter,
    MediaReconstructionService,
    VideoFileAdapter,
)
from blender_vision.perception.query import ObservationQueryService
from blender_vision.perception.repair import (
    FrontendComparisonService,
    FrontendRepairService,
)
from blender_vision.perception.runtime import default_adapter_registry, default_capture_bus
from blender_vision.perception.source import CodeRepositoryAdapter, SourceIntelligenceService
from blender_vision.perception.workspace import PerceptionWorkspace

__all__ = [
    "AdapterRegistry",
    "BrowserAdapter",
    "BrowserBenchmarkReceipt",
    "BrowserBenchmarkRunner",
    "BrowserExperienceAdapter",
    "CameraFrameAdapter",
    "CaptureBus",
    "CodeRepositoryAdapter",
    "DesignIntelligenceService",
    "DesktopSnapshotAdapter",
    "default_adapter_registry",
    "default_capture_bus",
    "ExperienceIRCompiler",
    "FeatureCapsuleCompiler",
    "FeatureCapsuleVerifier",
    "FigmaExportAdapter",
    "FrontendComparisonService",
    "FrontendRepairService",
    "GraphicsRoundTripService",
    "GraphicsRuntimeAdapter",
    "ImageFileAdapter",
    "LiveCameraAdapter",
    "load_browser_benchmark_manifest",
    "MediaReconstructionService",
    "MotionGraphReplay",
    "ObservationQueryService",
    "PerceptionLearningService",
    "PerceptionWorkspace",
    "RuntimeGltfCompiler",
    "StorybookExportAdapter",
    "SourceIntelligenceService",
    "VideoFileAdapter",
]
