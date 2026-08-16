#!/usr/bin/env bash
# 生成 docs/openapi/edgeflow-openapi.yaml（ROADMAP WBS 1.4：OpenAPI 独立 schema）。
cd "$(dirname "$0")/.." && go run ./hack/openapi-gen -out docs/openapi/edgeflow-openapi.yaml
