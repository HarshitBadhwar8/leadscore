# syntax=docker/dockerfile:1
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
# The leadscore image: one static binary, run as the non-root user 10001
# (docs/reference.md, "Receiver"). On Docker, compose.yaml runs `serve --every`; on
# Google Cloud the same image is the receiver service and the run job.

# Cross-compile on the build machine's platform; the binary is pure Go.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine3.24@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS build
ARG TARGETOS
ARG TARGETARCH
# The release sets VERSION to its tag; `leadscore version` prints it.
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X github.com/HarshitBadhwar8/leadscore/internal/cli.Version=${VERSION}" \
    -o /bin/leadscore ./cmd/leadscore \
    && mkdir -p /rootfs/data /rootfs/out /rootfs/home/leadscore \
    && chmod 700 /rootfs/data /rootfs/out

# distroless/static has no shell or package manager; it carries
# ca-certificates (vendor HTTPS) and tzdata (the rubric's `limits.timezone`).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
# The runtime user has the fixed id 10001 (not distroless's 65532), so a
# bind-mounted ./out can be given to it on a Linux host. /data and /out belong
# to it, so a fresh named volume does too, and are owner-only: they hold
# personal data.
COPY --from=build --chown=10001:10001 --chmod=700 /rootfs/data /data
COPY --from=build --chown=10001:10001 --chmod=700 /rootfs/out /out
COPY --from=build --chown=10001:10001 /rootfs/home/leadscore /home/leadscore
COPY --from=build /bin/leadscore /usr/local/bin/leadscore
# The binary's license and its dependencies' license texts travel with it.
COPY LICENSE NOTICE /usr/share/doc/leadscore/
COPY third_party/licenses /usr/share/doc/leadscore/licenses
LABEL org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.title="leadscore"
ENV HOME=/home/leadscore
USER 10001:10001
WORKDIR /home/leadscore
# The receiver's default port (receiver.port, else $PORT, else 8080).
EXPOSE 8080
ENTRYPOINT ["leadscore"]
CMD ["help"]
