from __future__ import annotations

from blender_vision.perception.browser import BrowserAdapter
from blender_vision.perception.bus import AdapterRegistry, CaptureBus
from blender_vision.perception.design import FigmaExportAdapter, StorybookExportAdapter
from blender_vision.perception.experience import BrowserExperienceAdapter
from blender_vision.perception.graphics import GraphicsRuntimeAdapter
from blender_vision.perception.media import (
    CameraFrameAdapter,
    DesktopSnapshotAdapter,
    ImageFileAdapter,
    LiveCameraAdapter,
    VideoFileAdapter,
)
from blender_vision.perception.source import CodeRepositoryAdapter
from blender_vision.projects.store import ProjectStore


def default_adapter_registry() -> AdapterRegistry:
    registry = AdapterRegistry()
    registry.register(BrowserAdapter())
    registry.register(BrowserExperienceAdapter())
    registry.register(FigmaExportAdapter())
    registry.register(GraphicsRuntimeAdapter())
    registry.register(ImageFileAdapter())
    registry.register(CameraFrameAdapter())
    registry.register(LiveCameraAdapter())
    registry.register(CodeRepositoryAdapter())
    registry.register(VideoFileAdapter())
    registry.register(DesktopSnapshotAdapter())
    registry.register(StorybookExportAdapter())
    return registry


def default_capture_bus(project: ProjectStore) -> CaptureBus:
    return CaptureBus(project, default_adapter_registry())
