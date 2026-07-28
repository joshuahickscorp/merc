# VisionMCP core census

This is the deterministic Wave 0 census of the recovered July 21 authority.
Capability claims remain bounded by executable tests and runtime receipts.

## Authority

- Branch: `feature/blender-vision-mcp`
- Census base commit: `cbac4a9743b423e8b410bd0bcd53160069e4154e`
- Expansion bible SHA-256: `f9a8045fbec58e1f8879766ae9b3da4c79df4fd293ce75edbbbc886d7219c109`
- Project source-tree SHA-256: `fbfe824b92f7a49ffe86586e43a2fd481e2de715325fb7d495a3794ed193d751`

## Repository

- Tracked project files: 278
- Python files: 206
- Python lines: 70912
- Collected test functions: 300
- JSON Schema files: 18

## Dependencies

- Python: `>=3.11`
- Runtime dependencies: `mcp>=1.12,<2`, `numpy>=1.26`, `opencv-python-headless>=4.10`, `Pillow>=10.4`, `platformdirs>=4.2`, `pydantic>=2.8,<3`
- Lockfile SHA-256: `07776353e14ef5b1ab2ac85ddf5c82123699f59d4ad65b244755b7defa3c0c68`

## State and artifacts

- State engine: SQLite
- SQLite tables: 91
- Embedded schema SHA-256: `85a414971f2f942191294d2e94b821336981ca84352338996a8878a1d0614921`
- Artifact identity: sha256
- Artifact layout: `artifacts/sha256/{digest[0:2]}/{digest[2:4]}/{digest}`
- Source bytes are immutable, content-addressed, portable, and tamper-verified.

The complete table and optional-dependency inventories are in
`docs/CORE_CENSUS.json`.
