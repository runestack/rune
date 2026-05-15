# Build stage
FROM golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git make

# Set working directory
WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN make build

# Final stage
FROM alpine:3.16

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN adduser -D -h /home/rune rune
USER rune
WORKDIR /home/rune

# Copy binary from build stage
COPY --from=builder /app/bin/rune /usr/local/bin/rune
COPY --from=builder /app/bin/runed /usr/local/bin/runed

# Set environment variables
ENV RUNE_CONFIG_DIR=/etc/rune

# Expose ports
EXPOSE 8080

# Set entry point
ENTRYPOINT ["runed"]

# Set default command
CMD ["serve"] 