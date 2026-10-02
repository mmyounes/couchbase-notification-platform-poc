# Single static binary. No node_modules, no native addon toolchain, and - the
# point of the rewrite - no replicas, so no nginx and no distributed state.
FROM golang:1.24-bookworm AS builder
WORKDIR /src
COPY apps/pipeline-go/go.mod apps/pipeline-go/go.sum ./
RUN go mod download
COPY apps/pipeline-go/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pipeline .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/pipeline /pipeline
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/pipeline"]
