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

FROM gcr.io/distroless/static:nonroot@sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087
COPY --from=build /out/kubeamber /kubeamber
LABEL org.opencontainers.image.source="https://github.com/kubeamber/kubeamber" \
      org.opencontainers.image.description="KubeAmber: traffic lights for AI agents on Kubernetes" \
      org.opencontainers.image.licenses="Apache-2.0"
USER 65532:65532
ENTRYPOINT ["/kubeamber"]
