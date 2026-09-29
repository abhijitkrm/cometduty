# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$(cat VERSION 2>/dev/null || echo dev)" \
    -o /out/cometduty ./cmd/cometduty \
 && install -d -o 65532 -g 65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cometduty /cometduty
# /data is the state dir — created owned by distroless's nonroot user
# (uid 65532, not 65534 = nobody) so a fresh named volume mounted over it
# inherits that ownership and stays writable.
COPY --from=build --chown=65532:65532 /out/data /data
# config is expected at /config/config.yml (mount a volume or secret)
USER nonroot:nonroot
EXPOSE 28686
ENTRYPOINT ["/cometduty", "-f", "/config/config.yml", "--state", "/data/state.json", "--alert-log", "/data/alerts.jsonl"]
