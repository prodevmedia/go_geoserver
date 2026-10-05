# Stage 1: Build binary
FROM golang:1.24.3-alpine AS builder

WORKDIR /app

# Install dependencies (git dibutuhkan jika ada module dari github)
RUN apk add --no-cache git

# Copy Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o geojson-server main.go

# Stage 2: Minimal image with GDAL/ogr2ogr
FROM alpine:3.18

WORKDIR /app

# Install GDAL tools, GDAL library, and PostgreSQL driver
RUN apk add --no-cache gdal-tools gdal gdal-driver-PG ca-certificates

# Copy binary dari builder
COPY --from=builder /app/geojson-server .

# Expose port
EXPOSE 8080

# Run server
CMD ["./geojson-server"]