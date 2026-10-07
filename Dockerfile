# syntax=docker/dockerfile:1
# The leadscore image: one static binary, run as the non-root user `leadscore`
# (contracts section 5.1). On Docker, compose.yaml runs `serve --every`; on
# Google Cloud the same image is the receiver service and the run job.

# Cross-compile on the build machine's platform; the binary is pure Go.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine3.24@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /bin/leadscore ./cmd/leadscore

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
# ca-certificates for vendor HTTPS; tzdata for the rubric's `limits.timezone`.
# /data and /out belong to the runtime user so a fresh named volume does too,
# and are owner-only: they hold personal data. The user has a fixed id
# (10001) so a bind-mounted ./out can be given to it on a Linux host.
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 leadscore \
    && adduser -S -u 10001 -G leadscore -h /home/leadscore leadscore \
    && mkdir -p /data /out \
    && chown leadscore:leadscore /data /out \
    && chmod 700 /data /out
COPY --from=build /bin/leadscore /usr/local/bin/leadscore
ENV HOME=/home/leadscore
USER leadscore
WORKDIR /home/leadscore
# The receiver's default port (receiver.port, else $PORT, else 8080).
EXPOSE 8080
ENTRYPOINT ["leadscore"]
CMD ["help"]
