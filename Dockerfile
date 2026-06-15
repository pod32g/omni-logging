# Build stage
FROM golang:1.25.11-bookworm@sha256:bbb255b0e131db500cf0520adc97441d2260cf629c7fa7e39e025ddf53995a24 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Security checks are part of the image build so CI cannot publish an image
# whose tests fail or whose reachable Go symbols have known vulnerabilities.
RUN go test ./...
RUN go run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...
# Pure-Go SQLite means we can build a fully static binary (no CGO).
RUN mkdir /data && CGO_ENABLED=0 go build -ldflags "-X main.version=docker" -o /omnilog ./cmd/omnilog

# Runtime stage: minimal, static, non-root.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639
COPY --from=build --chown=65532:65532 /omnilog /omnilog
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/omnilog"]
CMD ["serve", "--addr", ":8080", "--db", "/data/omni.db"]
