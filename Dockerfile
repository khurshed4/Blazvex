FROM golang:1.21-bullseye AS builder

WORKDIR /app

# Install sqlite3 dependencies (needed for go-sqlite3 CGo)
RUN apt-get update && apt-get install -y gcc libsqlite3-dev && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o blazvex ./main.go

# ── Runtime ──
FROM debian:bullseye-slim
RUN apt-get update && apt-get install -y ca-certificates libsqlite3-0 && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/blazvex .
COPY --from=builder /app/static ./static

RUN mkdir -p uploads

EXPOSE 8080

CMD ["./blazvex"]
