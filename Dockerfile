FROM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/overlay-s3 .

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

RUN apk add --no-cache ca-certificates
COPY --from=builder /out/overlay-s3 /usr/local/bin/overlay-s3
# the source tree ships in the image too: the config reference (README.md) and
# the config structs can be read with `docker run --entrypoint cat ... /src/...`
# before the gateway is configured and started
COPY --from=builder /src /src

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/overlay-s3"]
