class BlenderVisionError(RuntimeError):
    """Base error for expected Blender Vision failures."""


class ProjectError(BlenderVisionError):
    """A project is missing, invalid, or inaccessible."""


class SecurityError(BlenderVisionError):
    """An operation violated a safe-mode boundary."""


class BackendUnavailable(BlenderVisionError):
    """A requested backend is unavailable on this worker."""


class EvidenceUnavailable(BlenderVisionError):
    """Governed evidence is insufficient for the requested operation."""


class JobCancelled(BlenderVisionError):
    """A cooperative job cancellation was observed."""


class ValidationError(BlenderVisionError):
    """A record, schema, or contract failed validation."""
