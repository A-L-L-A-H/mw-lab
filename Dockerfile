FROM golang:1.24-alpine AS builder

WORKDIR /src

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mw-lab ./cmd/mw-lab

FROM alpine:3.21

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /app

COPY --from=builder /out/mw-lab /app/mw-lab

ENV MW_LAB_ADDR=:8090
ENV MW_LAB_DATA_DIR=/data

VOLUME ["/data"]
EXPOSE 8090

ENTRYPOINT ["/app/mw-lab"]
