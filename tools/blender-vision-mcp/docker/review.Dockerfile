FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
WORKDIR /opt/blender-vision
COPY pyproject.toml uv.lock README.md LICENSE ./
COPY src ./src
COPY blender_worker ./blender_worker
COPY benchmarks ./benchmarks
COPY schemas ./schemas
COPY docs ./docs
COPY MODEL_LICENSES.json SECURITY.md ./
RUN pip install --no-cache-dir . && mkdir -p /projects && chown 65532:65532 /projects
ENV BVMCP_PROJECTS_ROOT=/projects HOME=/tmp
USER 65532:65532
ENTRYPOINT ["bvmcp"]
CMD ["--help"]
