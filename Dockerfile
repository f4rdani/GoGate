# GoGate AI Gateway — multi-stage Docker build.
# Build:   docker build -t aigateway .
# Run:     docker run -p 8080:8080 -v ./config.yaml:/app/config.yaml:ro aigateway
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /gogate .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /gogate /app/gogate
COPY --from=build /src/config.yaml.example /app/config.yaml.example
EXPOSE 8080
ENTRYPOINT ["/app/gogate", "serve", "-config", "/app/config.yaml"]
