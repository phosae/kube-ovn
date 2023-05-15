#!/usr/bin/env bash
set -eux

PROJECT_ROOT=$(realpath "$(dirname "${BASH_SOURCE[0]}")/..")

# set GOPROXY you like
GOPROXY="https://goproxy.cn"

# wait for opts allowDangerousTypes=true ready... #https://github.com/kubernetes-sigs/controller-tools/pull/815
# controller-gen schemapatch:manifests=./yamls/crd,maxDescLen=0 paths=./pkg/apis/... output:dir=./yamls/crd
docker run -it --rm --platform linux/amd64 \
    -v ${PROJECT_ROOT}:/src\
    -e GOPROXY=${GOPROXY} \
    -e GO_PROJECT_ROOT="/src"\
    -e CRD_FLAG="schemapatch:manifests=./yamls/crd,maxDescLen=0"\
    -e CRD_TYPES_PATH="/src/pkg/apis"\
    -e CRD_OUT_PATH="/src/yamls/crd"\
    quay.io/slok/kube-code-generator:v1.26.0\
    update-crd.sh