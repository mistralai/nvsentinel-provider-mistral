# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
WORKDIR /src
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOBIN=/tools go install github.com/google/go-licenses/v2@v2.0.1
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY scripts/collect-licenses.sh /tools/collect-licenses.sh
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/provider ./cmd/provider
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GO_LICENSES=/tools/go-licenses \
    sh /tools/collect-licenses.sh /out/licenses

FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
LABEL org.opencontainers.image.source="https://github.com/mistralai/nvsentinel-provider-mistral" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY LICENSE /LICENSE
COPY --from=build /out/licenses /usr/share/licenses/nvsentinel-provider-mistral
COPY --from=build /out/provider /provider
USER 65532:65532
EXPOSE 50051 8080
ENTRYPOINT ["/provider"]
