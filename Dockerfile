FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X github.com/EsDmitrii/kconmon-ng/internal/config.Version=${VERSION} -X github.com/EsDmitrii/kconmon-ng/internal/config.Commit=${COMMIT} -X github.com/EsDmitrii/kconmon-ng/internal/config.BuildDate=${BUILD_DATE}" \
    -o /bin/kconmon-ng-agent ./cmd/agent

RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X github.com/EsDmitrii/kconmon-ng/internal/config.Version=${VERSION} -X github.com/EsDmitrii/kconmon-ng/internal/config.Commit=${COMMIT} -X github.com/EsDmitrii/kconmon-ng/internal/config.BuildDate=${BUILD_DATE}" \
    -o /bin/kconmon-ng-controller ./cmd/controller

FROM gcr.io/distroless/static-debian12:nonroot AS agent
COPY --from=builder /bin/kconmon-ng-agent /usr/local/bin/kconmon-ng-agent
USER nonroot:nonroot
ENTRYPOINT ["kconmon-ng-agent"]

FROM gcr.io/distroless/static-debian12:nonroot AS controller
COPY --from=builder /bin/kconmon-ng-controller /usr/local/bin/kconmon-ng-controller
USER nonroot:nonroot
ENTRYPOINT ["kconmon-ng-controller"]
