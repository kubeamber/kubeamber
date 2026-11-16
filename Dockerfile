# Base images are pinned by digest. The runtime image is Chainguard's static
# image (Wolfi, signed by Chainguard; see docs/releasing.md). The build stage
# stays on golang:1.26 to match go.mod and CI: cgr.dev/chainguard/go is free
# only as :latest, which is already the next Go release.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:f6946db66567acc1497655237e58f06ec3a57478a5e0d67fca29d2ffcf2ea136 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/kubeamber/kubeamber/internal/version.Version=${VERSION}" \
    -o /out/kubeamber ./cmd/kubeamber

FROM cgr.dev/chainguard/static:latest@sha256:fc42af33e643f20d08baf03cddcc1c02ba57052dfb02edb0d79f3fc47bcea8d9
COPY --from=build /out/kubeamber /kubeamber
LABEL org.opencontainers.image.source="https://github.com/kubeamber/kubeamber" \
      org.opencontainers.image.description="KubeAmber: traffic lights for AI agents on Kubernetes" \
      org.opencontainers.image.licenses="Apache-2.0"
USER 65532:65532
ENTRYPOINT ["/kubeamber"]
