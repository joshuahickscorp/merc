from __future__ import annotations

import ipaddress
import json
import mimetypes
import socket
import uuid
from pathlib import Path
from typing import Any
from urllib import error as urlerror
from urllib import parse, request
from urllib.robotparser import RobotFileParser

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import (
    atomic_write_json,
    canonical_json,
    sha256_file,
    utc_now,
)
from blender_vision.evidence.conflicts import EvidenceConflictStore
from blender_vision.evidence.duplicates import EvidenceDuplicateStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.projects.store import ProjectStore

GENERAL_TERMS = (
    "front",
    "rear",
    "left",
    "right",
    "top",
    "bottom",
    "underbody",
    "three-quarter",
    "dimensions",
    "technical drawing",
    "manual",
    "parts",
    "teardown",
    "walkaround",
    "close-up",
)

VEHICLE_TERMS = (
    "wheelbase",
    "track width",
    "ride height",
    "overhang",
    "wheel dimensions",
    "aero positions",
    "brake geometry",
    "lighting geometry",
    "intake geometry",
    "active aero states",
)

ACCEPTED_GOVERNANCE_REVIEWS = {"approved", "not_applicable", "user_owned"}
ACQUISITION_SOURCE_FIELDS = {
    "url",
    "retrieval_timestamp",
    "content_hash",
    "media_hash",
    "retrieval",
}


def _text(value: Any, label: str, *, maximum: int) -> str:
    normalized = str(value or "").strip()
    if (
        not normalized
        or len(normalized) > maximum
        or any(ord(character) < 32 for character in normalized)
    ):
        raise ValueError(f"{label} must be non-empty printable text")
    return normalized


def _governance_source_snapshot(source: dict[str, Any]) -> dict[str, Any]:
    return {
        key: value
        for key, value in source.items()
        if key not in ACQUISITION_SOURCE_FIELDS
    }


class EvidenceAcquisitionStore:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def plan_search(
        self,
        target_id: str | None = None,
        *,
        category: str = "general_product",
        focus_terms: list[str] | None = None,
    ) -> dict[str, Any]:
        resolution = TargetResolver(self.project).get(target_id)
        target = resolution["target"]
        identity = " ".join(
            str(value)
            for value in (
                target.get("model_year"),
                target.get("manufacturer"),
                target.get("model"),
                target.get("generation"),
                target.get("trim"),
            )
            if value
        )
        if focus_terms is not None:
            if not isinstance(focus_terms, list) or not 1 <= len(focus_terms) <= 32:
                raise ValueError("focused evidence search requires one to 32 terms")
            terms = []
            for raw in focus_terms:
                term = str(raw).strip().lower()
                if (
                    not term
                    or len(term) > 100
                    or any(ord(character) < 32 for character in term)
                ):
                    raise ValueError("focused evidence search term is invalid")
                if term not in terms:
                    terms.append(term)
        else:
            terms = list(GENERAL_TERMS)
        if category == "vehicles" and focus_terms is None:
            terms.extend(VEHICLE_TERMS)
        tasks = []
        for index, term in enumerate(terms):
            coverage_gain = 0.95 if term in {"bottom", "underbody", "technical drawing"} else 0.7
            uncertainty_reduction = 0.9 if term in {"dimensions", "technical drawing"} else 0.65
            cost = 0.15 + 0.01 * index
            latency = 0.2 + 0.01 * index
            license_value = 1.0 if term in {"manual", "technical drawing", "dimensions"} else 0.6
            value_score = (
                uncertainty_reduction * 0.4
                + coverage_gain * 0.3
                + license_value * 0.2
                - cost * 0.05
                - latency * 0.05
            )
            tasks.append(
                {
                    "query": f"{identity} {term}",
                    "term": term,
                    "expected_uncertainty_reduction": uncertainty_reduction,
                    "expected_coverage_gain": coverage_gain,
                    "cost": round(cost, 4),
                    "latency": round(latency, 4),
                    "license_value": license_value,
                    "value_score": round(value_score, 6),
                    "discovery_resolution_px": 512,
                }
            )
        tasks.sort(key=lambda item: (-item["value_score"], item["query"]))
        return {
            "target_id": resolution["id"],
            "canonical_identity": identity,
            "category": category,
            "focus_terms": terms if focus_terms is not None else [],
            "queries": [item["query"] for item in tasks],
            "ranked_tasks": tasks,
            "resolution_schedule": {
                "discovery_px": 512,
                "camera_and_coarse_geometry_px": 1024,
                "feature_validation_px": 2048,
                "final_regions": "4096_or_native",
            },
            "source_priority": [
                "user_owned",
                "manufacturer_authoritative",
                "licensed_or_reusable",
                "public_factual_technical",
                "diagnostic_third_party",
            ],
            "policy": (
                "focused discovery plan only; access restrictions and rights review remain "
                "mandatory"
                if focus_terms is not None
                else "discovery plan only; access restrictions and rights review remain mandatory"
            ),
        }

    def register_source(
        self,
        target_id: str,
        source: dict[str, Any],
        *,
        rights: dict[str, Any],
        reviewed_by: str | None = None,
    ) -> dict[str, Any]:
        TargetResolver(self.project).get(target_id)
        required = {
            "origin",
            "publisher",
            "page_title",
            "authority_class",
            "target_variant",
            "viewpoint",
            "quality_score",
        }
        missing = sorted(required - set(source))
        if missing:
            raise ValueError(f"source record missing fields: {', '.join(missing)}")
        rights_required = {"status", "internal_use", "redistribution"}
        if rights_required - set(rights):
            raise ValueError("rights record requires status, internal_use, and redistribution")
        if (
            not str(rights.get("status", "")).strip()
            or not isinstance(rights.get("internal_use"), bool)
            or not isinstance(rights.get("redistribution"), bool)
        ):
            raise ValueError("source rights require a named status and boolean permissions")
        if source.get("bypass_required") is True or source.get("access_control_bypass") is True:
            raise PermissionError("sources requiring access-control bypass cannot be registered")
        access = {
            "robots_respected": True,
            "authentication_boundary": "none",
            "source_terms_review": (
                "not_applicable"
                if source.get("authority_class") == "user_owned"
                else "pending"
            ),
            "privacy_review": (
                "user_owned"
                if source.get("authority_class") == "user_owned"
                else "not_applicable"
            ),
            "rate_limit_policy": "publisher_limits",
            "maximum_download_bytes": 512 * 1024 * 1024,
            **dict(source.get("access_policy") or {}),
        }
        if access["authentication_boundary"] not in {"none", "user_authorized"}:
            raise PermissionError("source authentication boundary is not authorized")
        if access["robots_respected"] is not True:
            raise PermissionError("source discovery must respect robots and access restrictions")
        if reviewed_by and (
            access.get("source_terms_review") not in ACCEPTED_GOVERNANCE_REVIEWS
            or access.get("privacy_review") not in ACCEPTED_GOVERNANCE_REVIEWS
        ):
            raise ValueError(
                "a named source reviewer requires completed terms and privacy decisions"
            )
        source_id = str(uuid.uuid4())
        now = utc_now()
        record = {
            "id": source_id,
            "target_id": target_id,
            "reference_id": None,
            "source": {
                "url": None,
                "retrieval_timestamp": now,
                "content_hash": None,
                "media_hash": None,
                "editing_suspicion": "unassessed",
                "cropping": {},
                "known_scale": None,
                "included_evidence": [],
                "excluded_evidence": [],
                **source,
                "access_policy": access,
            },
            "rights": rights,
            "status": "DISCOVERED",
            "created_at": now,
            "updated_at": now,
        }
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO evidence_sources(id,target_id,reference_id,source_json,status,"
                "created_at,updated_at) VALUES(?,?,?,?,?,?,?)",
                (source_id, target_id, None, json.dumps(record["source"]), "DISCOVERED", now, now),
            )
            connection.execute(
                "INSERT INTO rights_ledger(source_id,rights_json,reviewed_by,reviewed_at,"
                "updated_at) "
                "VALUES(?,?,?,?,?)",
                (
                    source_id,
                    json.dumps(rights),
                    None,
                    None,
                    now,
                ),
            )
        if reviewed_by:
            return self.review_governance(
                source_id,
                reviewed_by=reviewed_by,
                source_terms_review=str(access["source_terms_review"]),
                privacy_review=str(access["privacy_review"]),
                reviewer_type=str(access.get("reviewer_type", "human")),
                review_basis=(
                    dict(access["review_basis"])
                    if isinstance(access.get("review_basis"), dict)
                    else None
                ),
            )
        return record

    def review_governance(
        self,
        source_id: str,
        *,
        reviewed_by: str,
        source_terms_review: str,
        privacy_review: str,
        rights: dict[str, Any] | None = None,
        reviewer_type: str = "human",
        review_basis: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Record the named access/rights review required before governed acquisition."""
        reviewer = _text(reviewed_by, "evidence governance reviewer", maximum=200)
        if (
            source_terms_review not in ACCEPTED_GOVERNANCE_REVIEWS
            or privacy_review not in ACCEPTED_GOVERNANCE_REVIEWS
        ):
            raise ValueError("source terms and privacy review must be approved or not applicable")
        if reviewer_type not in {"human", "policy_agent"}:
            raise ValueError("evidence governance reviewer type must be human or policy_agent")
        record = self.get(source_id)
        governed_rights = dict(rights or record["rights"])
        if (
            not {"status", "internal_use", "redistribution"}.issubset(governed_rights)
            or not str(governed_rights.get("status", "")).strip()
            or not isinstance(governed_rights.get("internal_use"), bool)
            or not isinstance(governed_rights.get("redistribution"), bool)
        ):
            raise ValueError("rights record requires status, internal_use, and redistribution")
        normalized_basis = dict(review_basis or {})
        if reviewer_type == "policy_agent":
            required_basis = {
                "terms_urls",
                "terms_retrieved_at",
                "scope",
                "decision",
                "redistribution_prohibited",
            }
            missing_basis = sorted(required_basis - set(normalized_basis))
            if missing_basis:
                raise ValueError(
                    "policy-agent governance review lacks basis fields: "
                    + ", ".join(missing_basis)
                )
            terms_urls = normalized_basis.get("terms_urls")
            if not isinstance(terms_urls, list) or not terms_urls or any(
                parse.urlsplit(str(url)).scheme != "https" for url in terms_urls
            ):
                raise ValueError("policy-agent review requires one or more HTTPS terms URLs")
            if (
                not str(normalized_basis.get("terms_retrieved_at", "")).strip()
                or not str(normalized_basis.get("scope", "")).strip()
                or source_terms_review != "approved"
                or normalized_basis.get("decision") != "internal_use_permitted"
                or normalized_basis.get("redistribution_prohibited") is not True
                or governed_rights.get("internal_use") is not True
                or governed_rights.get("redistribution") is not False
            ):
                raise ValueError(
                    "policy-agent review may authorize only non-redistributed internal use"
                )
        source = dict(record["source"])
        source["access_policy"] = dict(source.get("access_policy") or {})
        access = source["access_policy"]
        access["source_terms_review"] = source_terms_review
        access["privacy_review"] = privacy_review
        now = utc_now()
        access["reviewed_by"] = reviewer
        access["reviewed_at"] = now
        access["reviewer_type"] = reviewer_type
        if normalized_basis:
            access["review_basis"] = normalized_basis
        else:
            access.pop("review_basis", None)
        expected_state = canonical_json(
            {
                "source": record["source"],
                "rights": record["rights"],
                "reviewed_by": record.get("reviewed_by"),
                "reviewed_at": record.get("reviewed_at"),
            }
        )
        with self.project.connection() as connection:
            previous_row = connection.execute(
                "SELECT receipt_digest FROM evidence_source_governance_reviews "
                "WHERE source_id=? ORDER BY rowid DESC LIMIT 1",
                (source_id,),
            ).fetchone()
        previous_digest = previous_row["receipt_digest"] if previous_row else None
        review_id = str(uuid.uuid4())
        receipt = {
            "schema_version": 1,
            "receipt_type": "evidence_source_governance_review",
            "id": review_id,
            "source_id": source_id,
            "target_id": record["target_id"],
            "reviewer": reviewer,
            "reviewer_type": reviewer_type,
            "source": _governance_source_snapshot(source),
            "rights": governed_rights,
            "reviewed_at": now,
            "supersedes_receipt_digest": previous_digest,
            "authority": "NAMED_EVIDENCE_GOVERNANCE_REVIEW",
        }
        relative = Path("receipts") / f"source-governance-{review_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.evidence-source-governance+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT s.source_json,r.rights_json,r.reviewed_by,r.reviewed_at "
                "FROM evidence_sources s JOIN rights_ledger r ON r.source_id=s.id "
                "WHERE s.id=?",
                (source_id,),
            ).fetchone()
            if current is None or canonical_json(
                {
                    "source": json.loads(current["source_json"]),
                    "rights": json.loads(current["rights_json"]),
                    "reviewed_by": current["reviewed_by"],
                    "reviewed_at": current["reviewed_at"],
                }
            ) != expected_state:
                raise RuntimeError("evidence source changed during governance review")
            current_previous = connection.execute(
                "SELECT receipt_digest FROM evidence_source_governance_reviews "
                "WHERE source_id=? ORDER BY rowid DESC LIMIT 1",
                (source_id,),
            ).fetchone()
            if (
                current_previous["receipt_digest"] if current_previous else None
            ) != previous_digest:
                raise RuntimeError("evidence source governance changed during named review")
            connection.execute(
                "UPDATE evidence_sources SET source_json=?,updated_at=? WHERE id=?",
                (json.dumps(source), now, source_id),
            )
            connection.execute(
                "UPDATE rights_ledger SET rights_json=?,reviewed_by=?,reviewed_at=?,updated_at=? "
                "WHERE source_id=?",
                (json.dumps(governed_rights), reviewer, now, now, source_id),
            )
            connection.execute(
                "INSERT INTO evidence_source_governance_reviews("
                "id,source_id,reviewer,reviewer_type,source_json,rights_json,receipt_digest,"
                "supersedes_receipt_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
                (
                    review_id,
                    source_id,
                    reviewer,
                    reviewer_type,
                    json.dumps(receipt["source"]),
                    json.dumps(governed_rights),
                    artifact.digest,
                    previous_digest,
                    now,
                ),
            )
        return {
            **self.get(source_id),
            "governance_receipt_digest": artifact.digest,
            "governance_receipt": receipt,
        }

    def acquire_local(self, source_id: str, path: Path) -> dict[str, Any]:
        return self._acquire_file(source_id, path, retrieval=None)

    def _acquire_file(
        self,
        source_id: str,
        path: Path,
        *,
        retrieval: dict[str, Any] | None,
    ) -> dict[str, Any]:
        record = self.get(source_id)
        governance = self.governance_status(source_id)
        if not governance["valid"]:
            raise PermissionError(
                "source acquisition requires a valid receipt-backed governance review"
            )
        if not bool(record["rights"].get("internal_use")):
            raise PermissionError("source rights do not permit internal reconstruction use")
        maximum_size = int(
            record["source"].get("access_policy", {}).get(
                "maximum_download_bytes", 512 * 1024 * 1024
            )
        )
        if path.expanduser().resolve().stat().st_size > maximum_size:
            raise ValueError("source exceeds its governed maximum download size")
        source = dict(record["source"])
        reference = ReferenceIngestor(self.project).import_file(
            path,
            rights_state=str(record["rights"]["status"]),
            viewpoint_label=source.get("viewpoint"),
        )
        source["content_hash"] = reference["artifact"]["digest"]
        source["media_hash"] = reference["artifact"]["digest"]
        now = utc_now()
        if retrieval is not None:
            source["retrieval"] = retrieval
            source["url"] = retrieval["final_url"]
            source["retrieval_timestamp"] = retrieval["retrieved_at"]
        reference_snapshot = self._reference_snapshot(reference)
        expected_state = canonical_json(
            {
                "reference_id": record.get("reference_id"),
                "source": record["source"],
                "status": record["status"],
                "updated_at": record["updated_at"],
            }
        )
        with self.project.connection() as connection:
            previous_row = connection.execute(
                "SELECT receipt_digest FROM evidence_source_acquisitions "
                "WHERE source_id=? ORDER BY rowid DESC LIMIT 1",
                (source_id,),
            ).fetchone()
        previous_digest = previous_row["receipt_digest"] if previous_row else None
        acquisition_id = str(uuid.uuid4())
        receipt = {
            "schema_version": 1,
            "receipt_type": "evidence_source_acquisition",
            "id": acquisition_id,
            "source_id": source_id,
            "target_id": record["target_id"],
            "reference_id": reference["id"],
            "governance_receipt_digest": governance["receipt_digest"],
            "source": source,
            "reference": reference_snapshot,
            "acquired_at": now,
            "supersedes_receipt_digest": previous_digest,
            "authority": "VERIFIED_EVIDENCE_SOURCE_ACQUISITION",
        }
        relative = Path("receipts") / f"source-acquisition-{acquisition_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.evidence-source-acquisition+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT reference_id,source_json,status,updated_at FROM evidence_sources "
                "WHERE id=?",
                (source_id,),
            ).fetchone()
            if current is None or canonical_json(
                {
                    "reference_id": current["reference_id"],
                    "source": json.loads(current["source_json"]),
                    "status": current["status"],
                    "updated_at": current["updated_at"],
                }
            ) != expected_state:
                raise RuntimeError("evidence source changed during acquisition")
            current_governance = connection.execute(
                "SELECT receipt_digest FROM evidence_source_governance_reviews "
                "WHERE source_id=? ORDER BY rowid DESC LIMIT 1",
                (source_id,),
            ).fetchone()
            if (
                current_governance is None
                or current_governance["receipt_digest"] != governance["receipt_digest"]
            ):
                raise RuntimeError("source governance changed during acquisition")
            current_previous = connection.execute(
                "SELECT receipt_digest FROM evidence_source_acquisitions "
                "WHERE source_id=? ORDER BY rowid DESC LIMIT 1",
                (source_id,),
            ).fetchone()
            if (
                current_previous["receipt_digest"] if current_previous else None
            ) != previous_digest:
                raise RuntimeError("source acquisition changed during acquisition")
            connection.execute(
                "UPDATE evidence_sources SET reference_id=?,source_json=?,status='ACQUIRED',"
                "updated_at=? WHERE id=?",
                (reference["id"], json.dumps(source), now, source_id),
            )
            connection.execute(
                "INSERT INTO evidence_source_acquisitions("
                "id,source_id,reference_id,governance_receipt_digest,source_json,reference_json,"
                "receipt_digest,supersedes_receipt_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
                (
                    acquisition_id,
                    source_id,
                    reference["id"],
                    governance["receipt_digest"],
                    json.dumps(source),
                    json.dumps(reference_snapshot),
                    artifact.digest,
                    previous_digest,
                    now,
                ),
            )
        conflict_audit = EvidenceConflictStore(self.project).audit(
            record["target_id"], record=True
        )
        duplicate_audit = EvidenceDuplicateStore(self.project).audit(
            record["target_id"], record=True
        )
        return {
            **record,
            "reference_id": reference["id"],
            "reference": reference,
            "source": source,
            "status": "ACQUIRED",
            "updated_at": now,
            "governance_receipt_digest": governance["receipt_digest"],
            "acquisition_receipt_digest": artifact.digest,
            "acquisition_receipt": receipt,
            "conflict_audit": conflict_audit,
            "duplicate_audit": duplicate_audit,
        }

    def acquire_url(self, source_id: str, *, timeout_seconds: float = 30.0) -> dict[str, Any]:
        """Download one pre-reviewed source without bypassing robots or access boundaries."""
        record = self.get(source_id)
        source = record["source"]
        access = source.get("access_policy", {})
        governance = self.governance_status(source_id)
        if not governance["valid"]:
            raise PermissionError(
                "URL acquisition requires a valid receipt-backed governance review"
            )
        if not record.get("reviewed_by") or not record.get("reviewed_at"):
            raise PermissionError("URL acquisition requires a named rights review")
        if access.get("source_terms_review") not in ACCEPTED_GOVERNANCE_REVIEWS:
            raise PermissionError("URL acquisition requires approved source-terms review")
        if access.get("privacy_review") not in ACCEPTED_GOVERNANCE_REVIEWS:
            raise PermissionError("URL acquisition requires approved privacy review")
        if not bool(record["rights"].get("internal_use")):
            raise PermissionError("source rights do not permit internal reconstruction use")
        if (
            not isinstance(timeout_seconds, (int, float))
            or timeout_seconds <= 0
            or timeout_seconds > 300
        ):
            raise ValueError("URL acquisition timeout must be between zero and 300 seconds")
        origin = str(source.get("origin", "")).strip()
        parsed = parse.urlsplit(origin)
        if parsed.scheme not in {"http", "https"} or not parsed.hostname:
            raise ValueError("URL acquisition requires an absolute HTTP(S) source origin")
        allow_private = bool(access.get("private_network_authorized")) and (
            access.get("authentication_boundary") == "user_authorized"
        )
        self._validate_remote_host(parsed.hostname, allow_private=allow_private)
        user_agent = "VisionMCP/1.0 (governed evidence acquisition)"
        robots_url = parse.urlunsplit((parsed.scheme, parsed.netloc, "/robots.txt", "", ""))
        robots = RobotFileParser()
        robots.set_url(robots_url)
        try:
            robots_request = request.Request(robots_url, headers={"User-Agent": user_agent})
            with request.urlopen(robots_request, timeout=float(timeout_seconds)) as response:
                robots_body = response.read(1024 * 1024).decode(
                    "utf-8", errors="replace"
                )
                robots.parse(robots_body.splitlines())
        except urlerror.HTTPError as error:
            if error.code in {404, 410}:
                robots.parse([])
            else:
                raise PermissionError(
                    f"robots policy could not be verified (HTTP {error.code})"
                ) from error
        except (urlerror.URLError, TimeoutError, OSError) as error:
            raise PermissionError("robots policy could not be verified") from error
        if not robots.can_fetch(user_agent, origin):
            raise PermissionError("robots policy disallows governed acquisition of this URL")
        maximum_size = int(access.get("maximum_download_bytes", 512 * 1024 * 1024))
        download_request = request.Request(
            origin,
            headers={
                "User-Agent": user_agent,
                "Accept": "image/*,video/*,application/pdf;q=0.9,*/*;q=0.1",
            },
        )
        downloads = self.project.root / "references" / "downloads"
        downloads.mkdir(parents=True, exist_ok=True)
        temporary: Path | None = None
        try:
            with request.urlopen(download_request, timeout=float(timeout_seconds)) as response:
                final_url = response.geturl()
                final_parsed = parse.urlsplit(final_url)
                if final_parsed.scheme not in {"http", "https"} or not final_parsed.hostname:
                    raise PermissionError("source redirected outside HTTP(S)")
                self._validate_remote_host(final_parsed.hostname, allow_private=allow_private)
                declared_length = response.headers.get("Content-Length")
                if declared_length and int(declared_length) > maximum_size:
                    raise ValueError("source exceeds its governed maximum download size")
                content_type = response.headers.get_content_type()
                suffix = Path(final_parsed.path).suffix.lower()
                if not suffix or len(suffix) > 10:
                    suffix = mimetypes.guess_extension(content_type) or ".bin"
                temporary = downloads / f"{source_id}-{uuid.uuid4().hex[:12]}{suffix}"
                received = 0
                with temporary.open("xb") as destination:
                    while True:
                        chunk = response.read(min(1024 * 1024, maximum_size + 1 - received))
                        if not chunk:
                            break
                        received += len(chunk)
                        if received > maximum_size:
                            raise ValueError("source exceeds its governed maximum download size")
                        destination.write(chunk)
                if received == 0:
                    raise ValueError("source download was empty")
                retrieval = {
                    "requested_url": origin,
                    "final_url": final_url,
                    "retrieved_at": utc_now(),
                    "http_status": int(getattr(response, "status", 200)),
                    "content_type": content_type,
                    "bytes": received,
                    "headers": {
                        name: response.headers.get(name)
                        for name in ("ETag", "Last-Modified", "Content-Length", "Content-Type")
                        if response.headers.get(name) is not None
                    },
                    "robots_url": robots_url,
                    "robots_allowed": True,
                    "redirected": final_url != origin,
                    "private_network_authorized": allow_private,
                    "rate_limit_policy": access.get("rate_limit_policy"),
                    "retry_count": 0,
                }
            acquired = self._acquire_file(source_id, temporary, retrieval=retrieval)
            return {**acquired, "retrieval": retrieval}
        finally:
            if temporary is not None and temporary.is_file():
                temporary.unlink()

    @staticmethod
    def _validate_remote_host(hostname: str, *, allow_private: bool) -> None:
        try:
            addresses = {
                item[4][0]
                for item in socket.getaddrinfo(hostname, None, type=socket.SOCK_STREAM)
            }
        except socket.gaierror as error:
            raise ValueError(f"source host could not be resolved: {hostname}") from error
        if not addresses:
            raise ValueError("source host resolved to no addresses")
        unsafe = []
        for address in addresses:
            value = ipaddress.ip_address(address)
            if (
                value.is_private
                or value.is_loopback
                or value.is_link_local
                or value.is_multicast
                or value.is_reserved
                or value.is_unspecified
            ):
                unsafe.append(address)
        if unsafe and not allow_private:
            raise PermissionError(
                f"private, local, or reserved source addresses are prohibited: {sorted(unsafe)}"
            )

    @staticmethod
    def _reference_snapshot(reference: dict[str, Any]) -> dict[str, Any]:
        artifact = reference["artifact"]
        return {
            "id": reference["id"],
            "artifact": {
                "digest": artifact["digest"],
                "size": int(artifact["size"]),
                "media_type": artifact["media_type"],
            },
            "original_name": reference["original_name"],
            "media_type": reference["media_type"],
            "relative_path": reference["relative_path"],
            "metadata": reference["metadata"],
            "quality": reference["quality"],
            "rights_state": reference["rights_state"],
            "viewpoint_label": reference.get("viewpoint_label"),
            "duplicate_of": reference.get("duplicate_of"),
        }

    @staticmethod
    def _reference_snapshot_from_row(row: Any) -> dict[str, Any]:
        return {
            "id": row["id"],
            "artifact": {
                "digest": row["artifact_digest"],
                "size": int(row["artifact_size"]),
                "media_type": row["artifact_media_type"],
            },
            "original_name": row["original_name"],
            "media_type": row["media_type"],
            "relative_path": row["relative_path"],
            "metadata": json.loads(row["metadata_json"]),
            "quality": json.loads(row["quality_json"]),
            "rights_state": row["rights_state"],
            "viewpoint_label": row["viewpoint_label"],
            "duplicate_of": row["duplicate_of"],
        }

    def prepare_adoption_authority(
        self,
        *,
        source_id: str,
        target_id: str,
        source: dict[str, Any],
        rights: dict[str, Any],
        reviewer: str,
        reviewed_at: str,
        reference_id: str,
    ) -> dict[str, Any]:
        """Prepare source-ledger receipts for an atomic named legacy-adoption decision."""
        reviewer_name = _text(reviewer, "evidence governance reviewer", maximum=200)
        source_snapshot = dict(source)
        source_snapshot["access_policy"] = dict(source_snapshot.get("access_policy") or {})
        source_snapshot["access_policy"]["reviewer_type"] = "human"
        source_snapshot["access_policy"]["reviewed_by"] = reviewer_name
        source_snapshot["access_policy"]["reviewed_at"] = reviewed_at
        semantics_error = self._governance_semantics_error(
            _governance_source_snapshot(source_snapshot),
            rights,
            reviewer=reviewer_name,
            reviewer_type="human",
            reviewed_at=reviewed_at,
        )
        if semantics_error:
            raise ValueError(semantics_error)
        with self.project.connection() as connection:
            reference_row = connection.execute(
                "SELECT r.*,a.size AS artifact_size,a.media_type AS artifact_media_type "
                "FROM reference_items r JOIN artifacts a ON a.digest=r.artifact_digest "
                "WHERE r.id=?",
                (reference_id,),
            ).fetchone()
        if reference_row is None:
            raise ValueError("legacy adoption source reference is missing")
        reference = self._reference_snapshot_from_row(reference_row)
        reference["rights_state"] = rights["status"]
        governance_id = str(uuid.uuid4())
        governance_receipt = {
            "schema_version": 1,
            "receipt_type": "evidence_source_governance_review",
            "id": governance_id,
            "source_id": source_id,
            "target_id": target_id,
            "reviewer": reviewer_name,
            "reviewer_type": "human",
            "source": _governance_source_snapshot(source_snapshot),
            "rights": rights,
            "reviewed_at": reviewed_at,
            "supersedes_receipt_digest": None,
            "authority": "NAMED_EVIDENCE_GOVERNANCE_REVIEW",
        }
        governance_relative = (
            Path("receipts") / f"source-governance-{governance_id}.json"
        )
        atomic_write_json(self.project.root / governance_relative, governance_receipt)
        governance_artifact = self.artifacts.ingest_file(
            self.project.root / governance_relative,
            media_type="application/vnd.bvmcp.evidence-source-governance+json",
        )
        acquisition_id = str(uuid.uuid4())
        acquisition_receipt = {
            "schema_version": 1,
            "receipt_type": "evidence_source_acquisition",
            "id": acquisition_id,
            "source_id": source_id,
            "target_id": target_id,
            "reference_id": reference_id,
            "governance_receipt_digest": governance_artifact.digest,
            "source": source_snapshot,
            "reference": reference,
            "acquired_at": reviewed_at,
            "supersedes_receipt_digest": None,
            "authority": "VERIFIED_EVIDENCE_SOURCE_ACQUISITION",
        }
        acquisition_relative = (
            Path("receipts") / f"source-acquisition-{acquisition_id}.json"
        )
        atomic_write_json(self.project.root / acquisition_relative, acquisition_receipt)
        acquisition_artifact = self.artifacts.ingest_file(
            self.project.root / acquisition_relative,
            media_type="application/vnd.bvmcp.evidence-source-acquisition+json",
        )
        return {
            "source": source_snapshot,
            "governance": {
                "receipt": governance_receipt,
                "digest": governance_artifact.digest,
            },
            "acquisition": {
                "receipt": acquisition_receipt,
                "digest": acquisition_artifact.digest,
            },
        }

    def migrate_legacy_authority(self, source_id: str) -> dict[str, Any]:
        """Wrap a complete legacy named review and acquired bytes without inventing review."""
        record = self.get(source_id)
        with self.project.connection() as connection:
            event_counts = {
                "governance": connection.execute(
                    "SELECT COUNT(*) FROM evidence_source_governance_reviews WHERE source_id=?",
                    (source_id,),
                ).fetchone()[0],
                "acquisition": connection.execute(
                    "SELECT COUNT(*) FROM evidence_source_acquisitions WHERE source_id=?",
                    (source_id,),
                ).fetchone()[0],
            }
        if event_counts != {"governance": 0, "acquisition": 0}:
            raise ValueError("source authority migration requires an untouched legacy ledger")
        reviewer = _text(
            record.get("reviewed_by"), "legacy evidence governance reviewer", maximum=200
        )
        reviewed_at = _text(
            record.get("reviewed_at"), "legacy evidence governance review time", maximum=100
        )
        legacy_source = dict(record["source"])
        legacy_source["access_policy"] = dict(legacy_source.get("access_policy") or {})
        legacy_access = legacy_source["access_policy"]
        if legacy_access.get("reviewed_by") != reviewer:
            raise ValueError("legacy source reviewer identity is inconsistent")
        reviewer_type = str(legacy_access.get("reviewer_type") or "human")
        if reviewer_type not in {"human", "policy_agent"}:
            raise ValueError("legacy source reviewer type cannot be migrated")
        completed_source = json.loads(json.dumps(legacy_source))
        completed_access = completed_source["access_policy"]
        normalized_fields = []
        if "reviewer_type" not in completed_access:
            completed_access["reviewer_type"] = reviewer_type
            normalized_fields.append("access_policy.reviewer_type")
        if completed_access.get("reviewed_at") != reviewed_at:
            completed_access["reviewed_at"] = reviewed_at
            normalized_fields.append("access_policy.reviewed_at")
        semantics_error = self._governance_semantics_error(
            _governance_source_snapshot(completed_source),
            record["rights"],
            reviewer=reviewer,
            reviewer_type=reviewer_type,
            reviewed_at=reviewed_at,
        )
        if semantics_error:
            raise ValueError(f"legacy source governance cannot be migrated: {semantics_error}")
        migration = {
            "kind": "legacy_named_source_review_schema_completion",
            "legacy_governance_source": _governance_source_snapshot(legacy_source),
            "legacy_rights_reviewed_at": reviewed_at,
            "normalized_fields": normalized_fields,
            "new_review_performed": False,
        }
        governance_id = str(uuid.uuid4())
        governance_receipt = {
            "schema_version": 2,
            "receipt_type": "evidence_source_governance_review",
            "id": governance_id,
            "source_id": source_id,
            "target_id": record["target_id"],
            "reviewer": reviewer,
            "reviewer_type": reviewer_type,
            "source": _governance_source_snapshot(legacy_source),
            "rights": record["rights"],
            "reviewed_at": reviewed_at,
            "supersedes_receipt_digest": None,
            "authority": "MIGRATED_NAMED_EVIDENCE_GOVERNANCE_REVIEW",
            "migration": migration,
        }
        governance_relative = (
            Path("receipts") / f"source-governance-migration-{governance_id}.json"
        )
        atomic_write_json(self.project.root / governance_relative, governance_receipt)
        governance_artifact = self.artifacts.ingest_file(
            self.project.root / governance_relative,
            media_type="application/vnd.bvmcp.evidence-source-governance+json",
        )
        acquisition_receipt = None
        acquisition_artifact = None
        if record["status"] == "ACQUIRED":
            if not record.get("reference_id"):
                raise ValueError("legacy acquired source lacks a reference identity")
            with self.project.connection() as connection:
                reference_row = connection.execute(
                    "SELECT r.*,a.size AS artifact_size,a.media_type AS artifact_media_type "
                    "FROM reference_items r JOIN artifacts a ON a.digest=r.artifact_digest "
                    "WHERE r.id=?",
                    (record["reference_id"],),
                ).fetchone()
            if reference_row is None:
                raise ValueError("legacy acquired source reference is missing")
            reference = self._reference_snapshot_from_row(reference_row)
            digest = reference["artifact"]["digest"]
            materialized = (self.project.root / reference["relative_path"]).resolve()
            materialized.relative_to(self.project.root.resolve())
            if (
                legacy_source.get("content_hash") != digest
                or legacy_source.get("media_hash") != digest
                or not self.artifacts.path_for(digest).is_file()
                or sha256_file(self.artifacts.path_for(digest))
                != (digest, int(reference["artifact"]["size"]))
                or not materialized.is_file()
                or sha256_file(materialized)[0] != digest
            ):
                raise ValueError("legacy acquired source bytes cannot be replayed")
            acquisition_id = str(uuid.uuid4())
            acquisition_receipt = {
                "schema_version": 1,
                "receipt_type": "evidence_source_acquisition",
                "id": acquisition_id,
                "source_id": source_id,
                "target_id": record["target_id"],
                "reference_id": record["reference_id"],
                "governance_receipt_digest": governance_artifact.digest,
                "source": legacy_source,
                "reference": reference,
                "acquired_at": record["updated_at"],
                "supersedes_receipt_digest": None,
                "authority": "VERIFIED_EVIDENCE_SOURCE_ACQUISITION",
            }
            acquisition_relative = (
                Path("receipts")
                / f"source-acquisition-migration-{acquisition_id}.json"
            )
            atomic_write_json(self.project.root / acquisition_relative, acquisition_receipt)
            acquisition_artifact = self.artifacts.ingest_file(
                self.project.root / acquisition_relative,
                media_type="application/vnd.bvmcp.evidence-source-acquisition+json",
            )
        elif record["status"] != "DISCOVERED":
            raise ValueError("legacy source status cannot be migrated")
        expected_state = canonical_json(
            {
                "source": record["source"],
                "rights": record["rights"],
                "reviewed_by": record.get("reviewed_by"),
                "reviewed_at": record.get("reviewed_at"),
                "status": record["status"],
                "reference_id": record.get("reference_id"),
                "updated_at": record["updated_at"],
            }
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT s.source_json,s.status,s.reference_id,s.updated_at,r.rights_json,"
                "r.reviewed_by,r.reviewed_at FROM evidence_sources s "
                "JOIN rights_ledger r ON r.source_id=s.id WHERE s.id=?",
                (source_id,),
            ).fetchone()
            if current is None or canonical_json(
                {
                    "source": json.loads(current["source_json"]),
                    "rights": json.loads(current["rights_json"]),
                    "reviewed_by": current["reviewed_by"],
                    "reviewed_at": current["reviewed_at"],
                    "status": current["status"],
                    "reference_id": current["reference_id"],
                    "updated_at": current["updated_at"],
                }
            ) != expected_state:
                raise RuntimeError("legacy source changed during authority migration")
            if any(
                connection.execute(
                    f"SELECT 1 FROM {table} WHERE source_id=? LIMIT 1", (source_id,)
                ).fetchone()
                for table in (
                    "evidence_source_governance_reviews",
                    "evidence_source_acquisitions",
                )
            ):
                raise RuntimeError("source authority ledger changed during migration")
            connection.execute(
                "UPDATE evidence_sources SET source_json=? WHERE id=?",
                (json.dumps(legacy_source), source_id),
            )
            connection.execute(
                "INSERT INTO evidence_source_governance_reviews("
                "id,source_id,reviewer,reviewer_type,source_json,rights_json,receipt_digest,"
                "supersedes_receipt_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
                (
                    governance_id,
                    source_id,
                    reviewer,
                    reviewer_type,
                    json.dumps(governance_receipt["source"]),
                    json.dumps(record["rights"]),
                    governance_artifact.digest,
                    None,
                    reviewed_at,
                ),
            )
            if acquisition_receipt and acquisition_artifact:
                connection.execute(
                    "INSERT INTO evidence_source_acquisitions("
                    "id,source_id,reference_id,governance_receipt_digest,source_json,"
                    "reference_json,receipt_digest,supersedes_receipt_digest,created_at) "
                    "VALUES(?,?,?,?,?,?,?,?,?)",
                    (
                        acquisition_receipt["id"],
                        source_id,
                        record["reference_id"],
                        governance_artifact.digest,
                        json.dumps(legacy_source),
                        json.dumps(acquisition_receipt["reference"]),
                        acquisition_artifact.digest,
                        None,
                        record["updated_at"],
                    ),
                )
        return {
            "source_id": source_id,
            "migration": migration,
            "governance_receipt_digest": governance_artifact.digest,
            "acquisition_receipt_digest": (
                acquisition_artifact.digest if acquisition_artifact else None
            ),
            "authority": self.authority_status(source_id),
        }

    @staticmethod
    def _governance_semantics_error(
        source: dict[str, Any],
        rights: dict[str, Any],
        *,
        reviewer: str,
        reviewer_type: str,
        reviewed_at: str,
    ) -> str | None:
        access = source.get("access_policy")
        if not isinstance(access, dict):
            return "source access policy is missing"
        if (
            access.get("robots_respected") is not True
            or access.get("authentication_boundary") not in {"none", "user_authorized"}
            or access.get("source_terms_review") not in ACCEPTED_GOVERNANCE_REVIEWS
            or access.get("privacy_review") not in ACCEPTED_GOVERNANCE_REVIEWS
            or not str(access.get("rate_limit_policy", "")).strip()
            or isinstance(access.get("maximum_download_bytes"), bool)
            or not isinstance(access.get("maximum_download_bytes"), int)
            or access["maximum_download_bytes"] <= 0
        ):
            return "source access policy is incomplete or unsafe"
        if (
            access.get("reviewed_by") != reviewer
            or access.get("reviewed_at") != reviewed_at
            or access.get("reviewer_type") != reviewer_type
        ):
            return "source access review identity is inconsistent"
        if (
            set(rights) < {"status", "internal_use", "redistribution"}
            or not str(rights.get("status", "")).strip()
            or not isinstance(rights.get("internal_use"), bool)
            or not isinstance(rights.get("redistribution"), bool)
        ):
            return "source rights record is invalid"
        if reviewer_type == "policy_agent":
            basis = access.get("review_basis")
            required = {
                "terms_urls",
                "terms_retrieved_at",
                "scope",
                "decision",
                "redistribution_prohibited",
            }
            if not isinstance(basis, dict) or set(basis) < required:
                return "policy-agent review basis is incomplete"
            urls = basis.get("terms_urls")
            if (
                not isinstance(urls, list)
                or not urls
                or any(parse.urlsplit(str(url)).scheme != "https" for url in urls)
                or not str(basis.get("terms_retrieved_at", "")).strip()
                or not str(basis.get("scope", "")).strip()
                or basis.get("decision") != "internal_use_permitted"
                or basis.get("redistribution_prohibited") is not True
                or rights.get("internal_use") is not True
                or rights.get("redistribution") is not False
            ):
                return "policy-agent review exceeds internal non-redistributed authority"
        return None

    def governance_status(self, source_id: str) -> dict[str, Any]:
        record = self.get(source_id)
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT rowid AS ledger_sequence,* "
                "FROM evidence_source_governance_reviews WHERE source_id=? "
                "ORDER BY rowid DESC LIMIT 1",
                (source_id,),
            ).fetchone()
        result = {
            "required": True,
            "valid": False,
            "source_id": source_id,
            "receipt_digest": row["receipt_digest"] if row else None,
            "error": None,
        }
        if row is None:
            result["error"] = "source has no authoritative governance-review ledger row"
            return result
        try:
            target_authority = TargetResolver(self.project).authority_status(
                record["target_id"]
            )
            if not target_authority["valid"]:
                raise ValueError("source target lacks valid canonical resolution authority")
            path = self.artifacts.path_for(row["receipt_digest"])
            if not path.is_file() or sha256_file(path)[0] != row["receipt_digest"]:
                raise ValueError("source governance receipt is missing or corrupt")
            receipt = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(receipt, dict):
                raise ValueError("source governance receipt must be an object")
            receipt_fields = {
                "schema_version",
                "receipt_type",
                "id",
                "source_id",
                "target_id",
                "reviewer",
                "reviewer_type",
                "source",
                "rights",
                "reviewed_at",
                "supersedes_receipt_digest",
                "authority",
            }
            schema_version = receipt.get("schema_version")
            migration_valid = schema_version == 1
            expected_authority = "NAMED_EVIDENCE_GOVERNANCE_REVIEW"
            semantics_source = (
                receipt.get("source") if isinstance(receipt.get("source"), dict) else {}
            )
            if schema_version == 2:
                receipt_fields.add("migration")
                expected_authority = "MIGRATED_NAMED_EVIDENCE_GOVERNANCE_REVIEW"
                migration = receipt.get("migration")
                migration_fields = {
                    "kind",
                    "legacy_governance_source",
                    "legacy_rights_reviewed_at",
                    "normalized_fields",
                    "new_review_performed",
                }
                if isinstance(migration, dict) and set(migration) == migration_fields:
                    legacy = migration.get("legacy_governance_source")
                    normalized_fields = migration.get("normalized_fields")
                    if isinstance(legacy, dict) and isinstance(normalized_fields, list):
                        replayed = json.loads(json.dumps(legacy))
                        replayed_access = replayed.get("access_policy")
                        expected_normalized = []
                        if isinstance(replayed_access, dict):
                            if "reviewer_type" not in replayed_access:
                                replayed_access["reviewer_type"] = row["reviewer_type"]
                                expected_normalized.append("access_policy.reviewer_type")
                            if replayed_access.get("reviewed_at") != row["created_at"]:
                                replayed_access["reviewed_at"] = row["created_at"]
                                expected_normalized.append("access_policy.reviewed_at")
                            semantics_source = replayed
                            migration_valid = bool(
                                migration.get("kind")
                                == "legacy_named_source_review_schema_completion"
                                and migration.get("legacy_rights_reviewed_at")
                                == row["created_at"]
                                and migration.get("normalized_fields")
                                == expected_normalized
                                and migration.get("new_review_performed") is False
                                and canonical_json(legacy)
                                == canonical_json(receipt.get("source"))
                            )
            reviewer = _text(row["reviewer"], "evidence governance reviewer", maximum=200)
            semantics_error = self._governance_semantics_error(
                semantics_source,
                receipt.get("rights") if isinstance(receipt.get("rights"), dict) else {},
                reviewer=reviewer,
                reviewer_type=str(row["reviewer_type"]),
                reviewed_at=str(row["created_at"]),
            )
            if semantics_error:
                raise ValueError(semantics_error)
            with self.project.connection() as connection:
                previous = connection.execute(
                    "SELECT receipt_digest FROM evidence_source_governance_reviews "
                    "WHERE source_id=? AND rowid<? ORDER BY rowid DESC LIMIT 1",
                    (source_id, row["ledger_sequence"]),
                ).fetchone()
            expected_supersedes = previous["receipt_digest"] if previous else None
            valid = bool(
                set(receipt) == receipt_fields
                and schema_version in {1, 2}
                and receipt.get("receipt_type") == "evidence_source_governance_review"
                and receipt.get("id") == row["id"]
                and receipt.get("source_id") == source_id
                and receipt.get("target_id") == record["target_id"]
                and receipt.get("reviewer") == reviewer
                and receipt.get("reviewer_type") == row["reviewer_type"]
                and receipt.get("reviewed_at") == row["created_at"]
                and receipt.get("authority") == expected_authority
                and migration_valid
                and receipt.get("supersedes_receipt_digest") == expected_supersedes
                and row["supersedes_receipt_digest"] == expected_supersedes
                and canonical_json(receipt.get("source")) == canonical_json(
                    json.loads(row["source_json"])
                )
                and canonical_json(receipt.get("rights")) == canonical_json(
                    json.loads(row["rights_json"])
                )
                and canonical_json(receipt.get("source")) == canonical_json(
                    _governance_source_snapshot(record["source"])
                )
                and canonical_json(receipt.get("rights")) == canonical_json(record["rights"])
                and record.get("reviewed_by") == reviewer
                and record.get("reviewed_at") == row["created_at"]
            )
            result["valid"] = valid
            if not valid:
                result["error"] = "source governance receipt semantics are inconsistent"
        except (OSError, TypeError, ValueError, json.JSONDecodeError) as error:
            result["error"] = str(error)
        return result

    def acquisition_status(self, source_id: str) -> dict[str, Any]:
        record = self.get(source_id)
        required = record["status"] == "ACQUIRED"
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT rowid AS ledger_sequence,* FROM evidence_source_acquisitions "
                "WHERE source_id=? ORDER BY rowid DESC LIMIT 1",
                (source_id,),
            ).fetchone()
        result = {
            "required": required,
            "valid": False,
            "source_id": source_id,
            "receipt_digest": row["receipt_digest"] if row else None,
            "error": None,
        }
        if not required:
            return result
        if row is None:
            result["error"] = "acquired source has no authoritative acquisition ledger row"
            return result
        try:
            receipt_path = self.artifacts.path_for(row["receipt_digest"])
            if not receipt_path.is_file() or sha256_file(receipt_path)[0] != row["receipt_digest"]:
                raise ValueError("source acquisition receipt is missing or corrupt")
            receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
            if not isinstance(receipt, dict):
                raise ValueError("source acquisition receipt must be an object")
            governance = self.governance_status(source_id)
            if not governance["valid"]:
                raise ValueError("source acquisition is not backed by valid current governance")
            with self.project.connection() as connection:
                reference_row = connection.execute(
                    "SELECT r.*,a.size AS artifact_size,a.media_type AS artifact_media_type "
                    "FROM reference_items r JOIN artifacts a ON a.digest=r.artifact_digest "
                    "WHERE r.id=?",
                    (row["reference_id"],),
                ).fetchone()
                previous = connection.execute(
                    "SELECT receipt_digest FROM evidence_source_acquisitions "
                    "WHERE source_id=? AND rowid<? ORDER BY rowid DESC LIMIT 1",
                    (source_id, row["ledger_sequence"]),
                ).fetchone()
            if reference_row is None:
                raise ValueError("source acquisition reference is missing")
            reference = self._reference_snapshot_from_row(reference_row)
            digest = reference["artifact"]["digest"]
            artifact_path = self.artifacts.path_for(digest)
            materialized = (self.project.root / reference["relative_path"]).resolve()
            materialized.relative_to(self.project.root.resolve())
            if (
                not artifact_path.is_file()
                or not materialized.is_file()
                or sha256_file(artifact_path)
                != (digest, int(reference["artifact"]["size"]))
                or sha256_file(materialized)[0] != digest
            ):
                raise ValueError("source acquisition bytes are missing or corrupt")
            expected_supersedes = previous["receipt_digest"] if previous else None
            receipt_fields = {
                "schema_version",
                "receipt_type",
                "id",
                "source_id",
                "target_id",
                "reference_id",
                "governance_receipt_digest",
                "source",
                "reference",
                "acquired_at",
                "supersedes_receipt_digest",
                "authority",
            }
            valid = bool(
                set(receipt) == receipt_fields
                and receipt.get("schema_version") == 1
                and receipt.get("receipt_type") == "evidence_source_acquisition"
                and receipt.get("id") == row["id"]
                and receipt.get("source_id") == source_id
                and receipt.get("target_id") == record["target_id"]
                and receipt.get("reference_id") == row["reference_id"]
                and receipt.get("reference_id") == record["reference_id"]
                and receipt.get("governance_receipt_digest") == governance["receipt_digest"]
                and row["governance_receipt_digest"] == governance["receipt_digest"]
                and receipt.get("acquired_at") == row["created_at"]
                and receipt.get("authority") == "VERIFIED_EVIDENCE_SOURCE_ACQUISITION"
                and receipt.get("supersedes_receipt_digest") == expected_supersedes
                and row["supersedes_receipt_digest"] == expected_supersedes
                and canonical_json(receipt.get("source")) == canonical_json(
                    json.loads(row["source_json"])
                )
                and canonical_json(receipt.get("source")) == canonical_json(record["source"])
                and canonical_json(receipt.get("reference")) == canonical_json(
                    json.loads(row["reference_json"])
                )
                and canonical_json(receipt.get("reference")) == canonical_json(reference)
                and record["status"] == "ACQUIRED"
                and record["source"].get("content_hash") == digest
                and record["source"].get("media_hash") == digest
                and reference["rights_state"] == record["rights"].get("status")
            )
            result["valid"] = valid
            if not valid:
                result["error"] = "source acquisition receipt semantics are inconsistent"
        except (OSError, TypeError, ValueError, json.JSONDecodeError) as error:
            result["error"] = str(error)
        return result

    def authority_status(self, source_id: str) -> dict[str, Any]:
        governance = self.governance_status(source_id)
        acquisition = self.acquisition_status(source_id)
        record = self.get(source_id)
        return {
            "source_id": source_id,
            "status": record["status"],
            "governance": governance,
            "acquisition": acquisition,
            "governance_valid": governance["valid"],
            "acquisition_valid": (
                acquisition["valid"] if acquisition["required"] else False
            ),
            "valid": governance["valid"]
            and (not acquisition["required"] or acquisition["valid"]),
        }

    def get(self, source_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT s.*,r.rights_json,r.reviewed_by,r.reviewed_at "
                "FROM evidence_sources s JOIN rights_ledger r ON r.source_id=s.id WHERE s.id=?",
                (source_id,),
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown evidence source: {source_id}")
        value = dict(row)
        value["source"] = json.loads(value.pop("source_json"))
        value["rights"] = json.loads(value.pop("rights_json"))
        return value

    def list(self, target_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            if target_id:
                rows = connection.execute(
                    "SELECT id FROM evidence_sources WHERE target_id=? ORDER BY created_at,id",
                    (target_id,),
                ).fetchall()
            else:
                rows = connection.execute(
                    "SELECT id FROM evidence_sources ORDER BY created_at,id"
                ).fetchall()
        return [self.get(row["id"]) for row in rows]

    def audit(self, target_id: str | None = None) -> dict[str, Any]:
        sources = self.list(target_id)
        authority = {item["id"]: self.authority_status(item["id"]) for item in sources}
        incomplete = [
            item["id"]
            for item in sources
            if not {"status", "internal_use", "redistribution"}.issubset(item["rights"])
            or not item.get("reviewed_by")
            or not item.get("reviewed_at")
            or not authority[item["id"]]["governance_valid"]
        ]
        incomplete_access = [
            item["id"]
            for item in sources
            if item["source"].get("access_policy", {}).get("robots_respected") is not True
            or item["source"].get("access_policy", {}).get("authentication_boundary")
            not in {"none", "user_authorized"}
            or item["source"].get("access_policy", {}).get("source_terms_review")
            not in ACCEPTED_GOVERNANCE_REVIEWS
            or item["source"].get("access_policy", {}).get("privacy_review")
            not in ACCEPTED_GOVERNANCE_REVIEWS
            or not item["source"].get("access_policy", {}).get("rate_limit_policy")
            or int(
                item["source"].get("access_policy", {}).get("maximum_download_bytes", 0)
            )
            <= 0
        ]
        invalid_acquisitions = [
            item["id"]
            for item in sources
            if item["status"] == "ACQUIRED"
            and not authority[item["id"]]["acquisition_valid"]
        ]
        return {
            "source_count": len(sources),
            "rights_ledger_complete": bool(sources) and not incomplete,
            "governance_complete": bool(sources)
            and not incomplete
            and not incomplete_access
            and not invalid_acquisitions,
            "incomplete_rights_source_ids": incomplete,
            "incomplete_access_policy_source_ids": incomplete_access,
            "invalid_acquisition_source_ids": invalid_acquisitions,
            "internal_use_permitted": sum(
                bool(item["rights"].get("internal_use")) for item in sources
            ),
            "redistribution_permitted": sum(
                bool(item["rights"].get("redistribution")) for item in sources
            ),
            "sources": [
                {**item, "authority": authority[item["id"]]} for item in sources
            ],
        }

    def deduplicate(self, target_id: str | None = None) -> dict[str, Any]:
        return EvidenceDuplicateStore(self.project).audit(target_id, record=True)

    def resolve_conflicts(self, target_id: str | None = None) -> dict[str, Any]:
        return EvidenceConflictStore(self.project).audit(target_id, record=True)

    def analyze_coverage(self, target_id: str | None = None) -> dict[str, Any]:
        resolution = TargetResolver(self.project).get(target_id)
        conflict_audit = EvidenceConflictStore(self.project).audit(
            resolution["id"], record=False
        )
        duplicate_audit = EvidenceDuplicateStore(self.project).audit(
            resolution["id"], record=False
        )
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id,source_json,status FROM evidence_sources WHERE target_id=? "
                "ORDER BY created_at,id",
                (resolution["id"],),
            ).fetchall()
        sources = [
            {
                "id": row["id"],
                "source": json.loads(row["source_json"]),
                "status": row["status"],
            }
            for row in rows
        ]
        authority = {item["id"]: self.authority_status(item["id"]) for item in sources}
        directions = {
            name: [] for name in ("front", "rear", "left", "right", "top", "bottom", "underbody")
        }
        for item in sources:
            eligible = conflict_audit["source_eligibility"].get(item["id"], {})
            duplicate_eligible = duplicate_audit["source_eligibility"].get(item["id"], {})
            if (
                item["status"] != "ACQUIRED"
                or not authority[item["id"]]["acquisition_valid"]
                or not eligible.get("coverage_eligible", False)
                or not duplicate_eligible.get("coverage_eligible", False)
            ):
                continue
            viewpoint = str(item["source"].get("viewpoint", "")).lower()
            for direction in directions:
                if direction in viewpoint:
                    directions[direction].append(item["source"].get("origin"))
        missing = [name for name, values in directions.items() if not values]
        report = {
            "target_id": resolution["id"],
            "directions": directions,
            "directional_coverage": round((len(directions) - len(missing)) / len(directions), 6),
            "missing_directions": missing,
            "next_best_evidence": [
                {
                    "surface": direction,
                    "request": f"Acquire a sharp {direction} view with the complete object visible",
                }
                for direction in missing
            ],
            "source_count": len(sources),
            "acquired_count": sum(item["status"] == "ACQUIRED" for item in sources),
            "eligible_acquired_count": sum(
                item["status"] == "ACQUIRED"
                and authority[item["id"]]["acquisition_valid"]
                and conflict_audit["source_eligibility"].get(item["id"], {}).get(
                    "coverage_eligible", False
                )
                and duplicate_audit["source_eligibility"].get(item["id"], {}).get(
                    "coverage_eligible", False
                )
                for item in sources
            ),
            "conflict_audit": conflict_audit,
            "duplicate_audit": duplicate_audit,
        }
        atomic_write_json(self.project.root / "comparisons" / "evidence-coverage.json", report)
        return report
