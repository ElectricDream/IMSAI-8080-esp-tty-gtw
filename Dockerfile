# syntax=docker/dockerfile:1
#
# Cross-compiles the IMSAI gateway (Go) into a single static binary.
# Override the target via build args:
#   docker buildx build --build-arg TARGETOS=windows --build-arg TARGETARCH=amd64 \
#       --target artifact --output type=local,dest=build .
# The 'artifact' stage is a scratch image holding only /imsai-gw, so --output exports
# just the binary to ./build/imsai-gw.

FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
RUN apk add --no-cache git ca-certificates
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG GOARM=
WORKDIR /src
COPY gateway-go/ ./
# Resolve deps and create go.sum inside the build (no go.sum committed).
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${GOARM} \
    go build -trimpath -ldflags "-s -w" -o /out/imsai-gw .

# Export-only stage: `docker build --output type=local,dest=...` copies /imsai-gw out.
FROM scratch AS artifact
COPY --from=build /out/imsai-gw /imsai-gw

# Runnable image (Linux), published to GHCR. No RUN here on purpose: a COPY-only target
# stage builds for any platform (linux/amd64, linux/arm64) without QEMU emulation. CA certs
# are copied from the build stage (which installed them) in case wss:// is used later.
FROM alpine:3.20 AS runtime
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/imsai-gw /usr/local/bin/imsai-gw
COPY imsai-gw.toml /etc/imsai-gw.toml
EXPOSE 2323
ENTRYPOINT ["/usr/local/bin/imsai-gw"]
CMD ["--config", "/etc/imsai-gw.toml"]
