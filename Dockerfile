# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o epubtrans .

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy binary
COPY --from=builder /build/epubtrans .

# Create directories for uploads and unpacked ebooks
RUN mkdir -p /app/uploads /app/unpackage

EXPOSE 3000

CMD ["./epubtrans", "serve", "/app/unpackage", "--port", "3000"]
