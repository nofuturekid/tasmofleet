# syntax=docker/dockerfile:1
# tasmofleet container image.
#  - build:   static Go binary (CGO off, pure-Go SQLite), cross-compiled on the build
#             machine for the target platform (linux/amd64 and linux/arm64), no emulation
#  - decoder: decode-config (LGPL-3.0, github.com/tasmota/decode-config), the PyInstaller
#             single-file build, pinned by version and SHA-256. It is executed as a separate
#             program, never linked. Build with --build-arg WITH_DECODER=false to leave it out.
#             amd64: upstream's release asset.
#             arm64: upstream has merged aarch64 builds (tasmota/decode-config#127) but has
#             not released since v16.0.0, so the arm64 image uses an UNOFFICIAL build of
#             exactly upstream v16.0.0 + #127 from github.com/nofuturekid/tasmota-decode-config
#             (release v16.0.0-arm64). TODO: switch to upstream's asset
#             (decode-config-ubuntu-24.04-arm64.tar.gz) once a release ships aarch64.
#  - app:     distroless cc (glibc, needed by the PyInstaller build), nonroot. The default target.
#  - ha:      the same image as root with the Home Assistant labels, for the Home Assistant app
#             (deploy/homeassistant): the Supervisor's /data belongs to root, which the nonroot
#             user cannot write. Build it with --target ha.

ARG GO_VERSION=1.27
ARG DECODER_PLATFORM=linux/amd64
ARG DECODER_VERSION=v16.0.0
ARG DECODER_SHA256=b92b8465fec5a940306cb24becb1365c0b4e56fd6695445a890ff4aec6287d6c
ARG DECODER_ARM64_URL=https://github.com/nofuturekid/tasmota-decode-config/releases/download/v16.0.0-arm64/decode-config-ubuntu-24.04-arm64.tar.gz
ARG DECODER_ARM64_SHA256=c07905137556a9fdb7eb3443d216ec92f549a8115ada0723a5ce42d1d3efdee1

FROM --platform=$BUILDPLATFORM docker.io/library/golang:${GO_VERSION}-trixie AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY changelog.go CHANGELOG.md ./
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/tasmofleet ./cmd/tasmofleet \
 && mkdir -p /out/data/tmp

# Each decoder stage only unpacks a tarball into /out/root, a tree laid over / in the final
# image; the stage for the other architecture is never built. The amd64 stage is pinned to
# linux/amd64 because it copies libz from a matching system; the arm64 stage needs no
# libraries (distroless cc on arm64 already has libz.so.1) and runs on the build platform, so no
# emulation is needed to build the arm64 image.
FROM --platform=$DECODER_PLATFORM docker.io/library/debian:trixie-slim AS decoder-amd64
ARG DECODER_VERSION
ARG DECODER_SHA256
ARG WITH_DECODER=true
ADD https://github.com/tasmota/decode-config/releases/download/${DECODER_VERSION}/decode-config-ubuntu-24.04.tar.gz /tmp/dc.tgz
RUN mkdir -p /out/root/usr/local/bin /out/root/usr/lib/x86_64-linux-gnu && if [ "$WITH_DECODER" = "true" ]; then \
      echo "${DECODER_SHA256}  /tmp/dc.tgz" | sha256sum -c - \
      && tar -xzf /tmp/dc.tgz -C /out/root/usr/local/bin decode-config \
      && chmod 0755 /out/root/usr/local/bin/decode-config \
      && cp -L /lib/x86_64-linux-gnu/libz.so.1 /out/root/usr/lib/x86_64-linux-gnu/; \
    fi

FROM --platform=$BUILDPLATFORM docker.io/library/debian:trixie-slim AS decoder-arm64
ARG DECODER_ARM64_URL
ARG DECODER_ARM64_SHA256
ARG WITH_DECODER=true
ADD ${DECODER_ARM64_URL} /tmp/dc.tgz
RUN mkdir -p /out/root/usr/local/bin && if [ "$WITH_DECODER" = "true" ]; then \
      echo "${DECODER_ARM64_SHA256}  /tmp/dc.tgz" | sha256sum -c - \
      && tar -xzf /tmp/dc.tgz -C /out/root/usr/local/bin decode-config \
      && chmod 0755 /out/root/usr/local/bin/decode-config; \
    fi

FROM decoder-${TARGETARCH} AS decoder

FROM gcr.io/distroless/cc-debian13:nonroot AS app
COPY --from=build /out/tasmofleet /usr/local/bin/tasmofleet
COPY --from=decoder /out/root/ /
COPY --from=build --chown=65532:65532 /out/data /data
ENV TASMOFLEET_DATA_DIR=/data \
    TMPDIR=/data/tmp
VOLUME ["/data"]
# 8080: UI/API (behind the reverse proxy). 80: firmware for devices (plain HTTP).
EXPOSE 8080 80
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/tasmofleet", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/tasmofleet"]
CMD ["serve"]

# Home Assistant app image. io.hass.arch uses the Supervisor's names, so each platform gets its own
# stage (a LABEL cannot map TARGETARCH). io.hass.version is the release version (VERSION).
FROM app AS ha-amd64
LABEL io.hass.arch="amd64"
FROM app AS ha-arm64
LABEL io.hass.arch="aarch64"
FROM ha-${TARGETARCH} AS ha
ARG VERSION=dev
USER root
LABEL io.hass.type="app" \
      io.hass.version="${VERSION}"

# The default target stays the standard image (docker build without --target).
FROM app
