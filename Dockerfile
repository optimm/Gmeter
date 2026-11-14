# The release image: the gmeter binary on a minimal base that has CA certificates (for
# https targets) and no shell. GoReleaser builds it in CI (.goreleaser.yaml, dockers_v2)
# and places the binaries in a directory per platform of the build context.
#
#   docker run --rm ghcr.io/optimm/gmeter -rate 100 -duration 10s http://host.docker.internal:8080/
#
# To build it from source instead: GOOS=linux go build -o linux/$(go env GOARCH)/gmeter ./cmd/gmeter
# and docker build --build-arg TARGETPLATFORM=linux/$(go env GOARCH) .
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/gmeter /usr/local/bin/gmeter
ENTRYPOINT ["/usr/local/bin/gmeter"]
