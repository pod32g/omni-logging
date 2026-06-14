# Build stage
FROM golang:1.25 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go SQLite means we can build a fully static binary (no CGO).
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=docker" -o /omnilog ./cmd/omnilog

# Runtime stage: minimal, static, non-root.
FROM gcr.io/distroless/static-debian12
COPY --from=build /omnilog /omnilog
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/omnilog"]
CMD ["serve", "--addr", ":8080", "--db", "/data/omni.db"]
