FROM golang:1.22 AS builder

LABEL org.opencontainers.image.source="https://github.com/agentic-research/mache"
LABEL org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /src

ENV CGO_ENABLED=0
ENV GO111MODULE=on
ENV GOFLAGS=-trimpath

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /out/mache .

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=builder /out/mache /app/mache

EXPOSE 7532
EXPOSE 7533

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/app/mache", "version"]

ENTRYPOINT ["/app/mache"]
CMD ["serve", "--http", ":7532"]
