FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/caffe-board .

FROM alpine:3.24
RUN apk --no-cache upgrade && apk --no-cache add tini ca-certificates
RUN addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=build /out/caffe-board /app/caffe-board
COPY templates /app/templates
USER app
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:${PORT:-8080}/healthz" || exit 1
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/app/caffe-board"]
