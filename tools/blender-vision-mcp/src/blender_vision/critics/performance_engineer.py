"""Performance engineer critic: consumes real measurements only, never simulates."""

from __future__ import annotations

from blender_vision.core.errors import ValidationError
from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.critics.measures import percentile
from blender_vision.v2.records import CriticFinding


class PerformanceEngineerCritic(Critic):
    role = CriticRole.PERFORMANCE_ENGINEER

    MAX_FRAME_P50_MS = 16.7
    MAX_FRAME_P95_MS = 33.4
    MAX_LONG_TASKS = 0
    MAX_HEAP_GROWTH_BYTES = 8_000_000
    MAX_CLS = 0.1

    def applies_to(self, subject: CritiqueSubject) -> bool:
        m = subject.metrics
        return "frame_times_ms" in m or "performance_measurements" in m

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []
        m = subject.metrics

        # Real measurements only — refuse invented samples.
        if m.get("measurements_are_simulated") is True:
            raise ValidationError(
                "performance_engineer refuses simulated measurements; "
                "provide runtime-observed frame times"
            )

        frame_times = m.get("frame_times_ms")
        if frame_times is None and "performance_measurements" in m:
            frame_times = m["performance_measurements"].get("frame_times_ms")
        if frame_times is not None:
            if not frame_times:
                raise ValidationError("frame_times_ms is empty; no real samples to consume")
            p50 = percentile(frame_times, 50)
            p95 = percentile(frame_times, 95)
            if p50 > self.MAX_FRAME_P50_MS:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:frame-p50",
                        role=self.role,
                        diagnosis="frame time p50 exceeds 60 fps budget",
                        evidence=refs,
                        measured={"frame_time_p50_ms": float(p50)},
                        severity="major",
                        likely_cause="GPU/CPU bound per-frame work",
                        bounded_repair={
                            "parameters": ["lod", "dpr", "draw_calls"],
                            "action": "reduce_frame_cost",
                        },
                        blast_radius=["performance", "lod"],
                        acceptance_test="frame_time_p50_ms <= 16.7",
                    )
                )
            if p95 > self.MAX_FRAME_P95_MS:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:frame-p95",
                        role=self.role,
                        diagnosis="frame time p95 exceeds 30 fps tail budget",
                        evidence=refs,
                        measured={"frame_time_p95_ms": float(p95)},
                        severity="critical",
                        likely_cause="long tasks or GPU spikes",
                        bounded_repair={
                            "parameters": ["long_tasks", "shader_compile"],
                            "action": "smooth_tail_latency",
                        },
                        blast_radius=["performance"],
                        acceptance_test="frame_time_p95_ms <= 33.4",
                    )
                )

        long_tasks = m.get("long_task_count")
        if long_tasks is not None and int(long_tasks) > self.MAX_LONG_TASKS:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:long-tasks",
                    role=self.role,
                    diagnosis="long tasks observed on the main thread",
                    evidence=refs,
                    measured={"long_task_count": int(long_tasks)},
                    severity="major",
                    likely_cause="synchronous parse/compile or layout thrash",
                    bounded_repair={
                        "parameters": ["long_task_count"],
                        "action": "break_up_long_tasks",
                    },
                    blast_radius=["performance", "main_thread"],
                    acceptance_test="long_task_count <= 0",
                )
            )

        growth = m.get("javascript_heap_growth_bytes")
        if growth is not None and int(growth) > self.MAX_HEAP_GROWTH_BYTES:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:memory-growth",
                    role=self.role,
                    diagnosis="JavaScript heap growth exceeds budget",
                    evidence=refs,
                    measured={"javascript_heap_growth_bytes": int(growth)},
                    severity="major",
                    likely_cause="retained allocations or listener leaks",
                    bounded_repair={
                        "parameters": ["javascript_heap_growth_bytes"],
                        "action": "cap_retained_allocations",
                    },
                    blast_radius=["performance", "memory"],
                    acceptance_test="javascript_heap_growth_bytes <= 8000000",
                )
            )

        cls = m.get("cumulative_layout_shift")
        if cls is not None and float(cls) > self.MAX_CLS:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:cls",
                    role=self.role,
                    diagnosis="cumulative layout shift exceeds Core Web Vital budget",
                    evidence=refs,
                    measured={"cumulative_layout_shift": float(cls)},
                    severity="major",
                    likely_cause="late-loading media without reserved space",
                    bounded_repair={
                        "parameters": ["cumulative_layout_shift", "layout_slots"],
                        "action": "reserve_media_space",
                    },
                    blast_radius=["performance", "layout"],
                    acceptance_test="cumulative_layout_shift <= 0.1",
                )
            )
        return findings
