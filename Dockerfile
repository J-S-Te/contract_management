
FROM golang:1.26.6-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
ARG GOPROXY=https://goproxy.cn|https://proxy.golang.org|direct
ARG GOSUMDB=sum.golang.google.cn
ENV GOPROXY=${GOPROXY} \
    GOSUMDB=${GOSUMDB}
RUN set -eu; \
    for attempt in 1 2 3 4 5; do \
      if go mod download && go mod verify; then exit 0; fi; \
      echo "go module download failed (attempt ${attempt}/5)" >&2; \
      sleep $((attempt * 2)); \
    done; \
    exit 1

COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY authz/ ./authz/
COPY migrations/ ./migrations/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/authz-catalog ./cmd/authz-catalog \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/file-backfill ./cmd/file-backfill \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/worker ./cmd/worker \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/worker-rollout ./cmd/worker-rollout \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/migrate ./cmd/migrate

FROM alpine:3.23

RUN apk add --no-cache ca-certificates tzdata wget libreoffice-writer font-noto-cjk

WORKDIR /app

COPY --from=builder /out/api ./api
COPY --from=builder /out/authz-catalog ./authz-catalog
COPY --from=builder /out/file-backfill ./file-backfill
COPY --from=builder /out/worker ./worker
COPY --from=builder /out/worker-rollout ./worker-rollout
COPY --from=builder /out/migrate ./migrate
COPY docker-entrypoint.sh /usr/local/bin/contract-management-entrypoint

RUN chmod +x /usr/local/bin/contract-management-entrypoint

EXPOSE 8081

ENTRYPOINT ["/usr/local/bin/contract-management-entrypoint"]
CMD ["./api"]
