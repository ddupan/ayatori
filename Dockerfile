# Build the manager binary
# Override BASE_IMAGE to build from another registry, e.g. docker.io/library/golang:1.27.2
ARG BASE_IMAGE=golang:1.27.4@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666
FROM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH
# CI 传入内网 Go 模块缓存；未传时为空，go 使用默认的 proxy.golang.org。
ARG GOPROXY
ARG GOPRIVATE
# 发布构建传入 tag（release.yml）；manager 在 kube-public/ayatori-version 公布它。
ARG VERSION=dev

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-X main.version=${VERSION}" -o manager ./cmd
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -o client-credentials ./cmd/client-credentials
# ayatori-api（ADR-0027）与 manager 同一镜像、同一版本，部署时以 --entrypoint 选用。
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -o ayatori-api ./cmd/ayatori-api
# cloud-provider（ADR-0028）由消费方部署在 workload 集群，同一镜像、同一版本。
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -o cloud-provider ./cmd/cloud-provider

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/client-credentials .
COPY --from=builder /workspace/ayatori-api .
COPY --from=builder /workspace/cloud-provider .
USER 65532:65532

ENTRYPOINT ["/manager"]
