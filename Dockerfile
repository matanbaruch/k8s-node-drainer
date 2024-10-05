# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o node-drainer main.go

# Run stage
FROM alpine:latest
WORKDIR /root/

COPY --from=builder /app/node-drainer .

CMD ["./node-drainer"]

