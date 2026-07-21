# syntax=docker/dockerfile:1.7
ARG GO_IMAGE=golang:1.26.5-bookworm
ARG RUNTIME_BASE_IMAGE=alpine:3.22.1
FROM ${GO_IMAGE} AS build
WORKDIR /src/runtime
COPY runtime/go.mod runtime/go.sum ./
RUN go mod download
COPY runtime/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hermes-trigger ./cmd/trigger \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hermes-worker ./cmd/worker
FROM ${RUNTIME_BASE_IMAGE}
RUN apk add --no-cache ca-certificates wget \
 && addgroup -S hermes \
 && adduser -S -G hermes -h /nonexistent -s /sbin/nologin hermes
COPY --from=build /out/hermes-trigger /app/hermes-trigger
COPY --from=build /out/hermes-worker /app/hermes-worker
COPY ci/variants.yaml /etc/hermes/variants.yaml
USER hermes
WORKDIR /app
CMD ["/app/hermes-trigger"]
