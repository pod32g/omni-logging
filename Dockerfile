# UI stage. The built bundle is also committed, so `go build` alone produces a
# working server without Node — but the image rebuilds it from source so a
# published container can never ship a stale UI that someone forgot to rebuild.
FROM node:24-bookworm-slim@sha256:6f7b03f7c2c8e2e784dcf9295400527b9b1270fd37b7e9a7285cf83b6951452d AS ui
WORKDIR /ui
COPY internal/web/ui/package.json internal/web/ui/package-lock.json ./
RUN npm ci
COPY internal/web/ui/ ./
RUN npm run build

# Build stage
FROM golang:1.25.12-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Replace the committed bundle with the one just built from source.
COPY --from=ui /dist/ ./internal/web/dist/
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
