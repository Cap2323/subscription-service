#!/bin/sh
# Requires locally installed Trivy and Syft; image must already have been built.
set -eu
IMAGE=${1:-${IMAGE:-}}
: "${IMAGE:?Set IMAGE to a release image or digest}"
trivy image --exit-code 1 --severity HIGH,CRITICAL "$IMAGE"
syft "$IMAGE" -o spdx-json > sbom.spdx.json
