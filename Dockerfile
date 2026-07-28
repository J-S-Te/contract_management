
FROM golang:1.26.4-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . ./

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/worker ./cmd/worker

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata wget

WORKDIR /app

COPY --from=builder /out/api ./api
COPY --from=builder /out/worker ./worker
COPY docker-entrypoint.sh /usr/local/bin/contract-management-entrypoint

RUN chmod +x /usr/local/bin/contract-management-entrypoint

EXPOSE 8081

ENTRYPOINT ["/usr/local/bin/contract-management-entrypoint"]
CMD ["./api"]
