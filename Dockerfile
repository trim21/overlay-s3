FROM golang:1.25-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/overlay-s3 .

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

RUN apk add --no-cache ca-certificates
COPY --from=builder /out/overlay-s3 /usr/local/bin/overlay-s3

EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/usr/local/bin/overlay-s3"]
