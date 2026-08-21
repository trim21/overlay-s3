FROM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/overlay-s3 .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates
COPY --from=builder /out/overlay-s3 /usr/local/bin/overlay-s3

EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/usr/local/bin/overlay-s3"]
