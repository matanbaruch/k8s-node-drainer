# Dockerfile
FROM golang:1.22 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o node-drainer

FROM scratch

COPY --from=builder /app/node-drainer .

CMD ["./node-drainer"]
