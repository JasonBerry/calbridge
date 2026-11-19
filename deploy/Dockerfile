# calbridge container (PLAN.md §13). Build from the repository root:
#   docker build -f deploy/Dockerfile -t calbridge .
# Run with one volume holding config.yaml, the secrets and the state; paths in
# config.yaml must point inside /data (e.g. /data/google-client.json,
# /data/google-token.json, /data/icloud-app-password, /data/calbridge.db,
# /data/backups). See README.md "Running it".
FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/calbridge ./cmd/calbridge

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/calbridge /calbridge
VOLUME ["/data"]
# The status server listens on server.listen; set it to 0.0.0.0:8089 in the
# container's config and publish it only to localhost (-p 127.0.0.1:8089:8089).
EXPOSE 8089
USER nonroot:nonroot
ENTRYPOINT ["/calbridge"]
CMD ["serve", "--config", "/data/config.yaml"]
