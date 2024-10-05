# Start from Golang 1.22 base image
FROM golang:1.22 as builder

WORKDIR /app

# Copy the Go modules definition and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY . .

# Build the application
RUN go build -o /node-drainer main.go

# Use a minimal base image
FROM gcr.io/distroless/base

# Copy the compiled Go binary from the builder stage
COPY --from=builder /node-drainer /node-drainer

# Set the entrypoint
ENTRYPOINT ["/node-drainer"]

