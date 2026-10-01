# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/sharkusmanch/rclone-metrics-shim/internal/shim.Version=${VERSION}" \
      -o /out/rclone-metrics-shim ./cmd/rclone-metrics-shim

# scratch: the image carries one static binary. `install` copies it out, so no
# shell or cp is needed (see README, "Kubernetes").
FROM scratch
COPY --from=build /out/rclone-metrics-shim /rclone-metrics-shim
USER 65532:65532
ENTRYPOINT ["/rclone-metrics-shim"]
CMD ["help"]
