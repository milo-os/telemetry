FROM --platform=$BUILDPLATFORM docker.io/golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w" \
    -o nkey-generator .

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/nkey-generator .
USER 65532:65532

ENTRYPOINT ["/nkey-generator"]
