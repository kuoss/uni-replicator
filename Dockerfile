FROM golang:1.25 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/uni-replicator ./cmd/uni-replicator

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/uni-replicator /uni-replicator
USER nonroot:nonroot
ENTRYPOINT ["/uni-replicator"]
