# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$(cat VERSION 2>/dev/null || echo dev)" \
    -o /out/cometduty ./cmd/cometduty

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cometduty /cometduty
# config is expected at /config/config.yml (mount a volume or secret)
USER nonroot:nonroot
EXPOSE 8888 28686
ENTRYPOINT ["/cometduty", "-f", "/config/config.yml", "--state", "/data/state.json"]
