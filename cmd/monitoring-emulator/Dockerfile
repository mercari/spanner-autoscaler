FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o monitoring-emulator ./cmd/monitoring-emulator

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/monitoring-emulator .
USER 65532:65532

EXPOSE 9090 9091

ENTRYPOINT ["/monitoring-emulator"]
