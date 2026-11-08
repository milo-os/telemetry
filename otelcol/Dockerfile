# Build context is the repo root (matches the top-level Dockerfile's
# convention) -- needed so this stage can see the sibling
# processor/unbatchprocessor, exporter/natsexporter, and
# receiver/natsreceiver modules that builder-config.yaml's replace
# directives point at.
FROM --platform=$BUILDPLATFORM docker.io/golang:1.27 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY processor/unbatchprocessor/ processor/unbatchprocessor/
COPY exporter/natsexporter/ exporter/natsexporter/
COPY receiver/natsreceiver/ receiver/natsreceiver/
COPY otelcol/builder-config.yaml otelcol/builder-config.yaml

WORKDIR /workspace/otelcol
# Install the builder tool natively (no GOOS/GOARCH override) -- it must
# run on the build host, not the target platform. Only the collector
# binary it produces should be cross-compiled; setting GOARCH around a
# `go run` invocation makes go try to execute the freshly built tool
# itself as a foreign-arch binary, which fails without QEMU registered.
RUN go install go.opentelemetry.io/collector/cmd/builder@v0.157.0
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    "$(go env GOPATH)/bin/builder" --config builder-config.yaml

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/otelcol/_build/otelcol .
USER 65532:65532

ENTRYPOINT ["/otelcol"]
