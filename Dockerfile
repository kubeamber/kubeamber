# Base images are pinned by digest; Dependabot keeps them current.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:3745f1aa11822297b79113f5e84ce2bffeb039734571c39855809cfe9e5d7be6 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/kubeamber/kubeamber/internal/version.Version=${VERSION}" \
    -o /out/kubeamber ./cmd/kubeamber

FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /out/kubeamber /kubeamber
LABEL org.opencontainers.image.source="https://github.com/kubeamber/kubeamber" \
      org.opencontainers.image.description="KubeAmber: traffic lights for AI agents on Kubernetes" \
      org.opencontainers.image.licenses="Apache-2.0"
USER 65532:65532
ENTRYPOINT ["/kubeamber"]
