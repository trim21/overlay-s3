# overlay-s3

S3-compatible gateway that layers a local write overlay on top of a real S3
backend. It exposes an S3 API to clients; reads prefer the local overlay and
fall back to the remote backend, writes land only in the local overlay, and
listings merge both stores with local keys shadowing remote ones.

Useful when you want to keep an existing S3 bucket as a read-only baseline
while writes go to a fast local store — no deletion, no propagation, just a
merged view.

## How it works

```
client ──▶ overlay-s3 (S3 API, SigV4) ──▶ local overlay (writes, reads win)
                                     └──▶ remote S3 (read fallback, baseline)
```

- `GET` / `HEAD`: local store first, remote store on miss
- `PUT` / multipart: local store only, the remote backend is never modified
- `ListObjects` / `ListBuckets`: merged view of both stores, local keys win
- deletion is not supported and answered with `NotImplemented`

The protocol layer (S3 XML, multipart flow, pagination, error codes) is
provided by [gofakes3](https://github.com/johannesboyne/gofakes3); signature
validation is enforced by a SigV4 middleware in front of it.

## Usage

```bash
go build -o overlay-s3 .
./overlay-s3 \
  -listen :8080 \
  -local-dir ./data \
  -remote-endpoint https://s3.amazonaws.com \
  -remote-region us-east-1 \
  -remote-access-key <remote-access-key> \
  -remote-secret-key <remote-secret-key> \
  -auth-key <client-key> \
  -auth-secret <client-secret>
```

| flag | default | description |
| --- | --- | --- |
| `-listen` | `:8080` | HTTP listen address |
| `-local-dir` | `./data` | local overlay storage directory |
| `-remote-endpoint` | (AWS) | remote S3 endpoint, empty uses AWS |
| `-remote-region` | `us-east-1` | remote S3 region |
| `-remote-access-key` / `-remote-secret-key` | | credentials for the remote S3 backend |
| `-auth-key` / `-auth-secret` | (disabled) | key pair clients must sign requests with; empty disables signature checks |

Clients (aws cli, SDKs, rclone) connect to the gateway and sign with
`-auth-key`/`-auth-secret`:

```bash
aws --endpoint-url http://127.0.0.1:8080 s3api create-bucket --bucket demo
aws --endpoint-url http://127.0.0.1:8080 s3 cp local-file s3://demo/key
aws --endpoint-url http://127.0.0.1:8080 s3 ls s3://demo/
```

## Docker Compose

也可以直接用 Docker 跑。下面这个 compose 文件构建镜像，并起一个
[silo](https://github.com/pgsty/silo) 容器（S3 兼容后端）作为远端基线，
得到开箱即用的演示环境：

```yaml
services:
  overlay-s3:
    build: .
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data
    command:
      - -listen=:8080
      - -local-dir=/data
      - -remote-endpoint=http://silo:9000
      - -remote-region=us-east-1
      - -remote-access-key=minioadmin
      - -remote-secret-key=minioadmin-secret
      - -auth-key=demo
      - -auth-secret=demo-secret
    depends_on:
      silo:
        condition: service_healthy

  silo:
    image: pgsty/silo:latest
    command: server /data
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin-secret
    volumes:
      - silo-data:/data
    healthcheck:
      test: ["CMD-SHELL", "bash -c 'exec 3<>/dev/tcp/127.0.0.1/9000'"]
      interval: 5s
      timeout: 3s
      retries: 20

volumes:
  silo-data:
```

```bash
docker compose up -d --build
```

启动后客户端用 `demo` / `demo-secret` 签名访问：

```bash
aws --endpoint-url http://127.0.0.1:8080 s3api create-bucket --bucket demo
aws --endpoint-url http://127.0.0.1:8080 s3 cp local-file s3://demo/key
```

写操作只落在 overlay 卷 `./data`（容器内 `/data`），silo 仅作为远端基线提供读回退。
要对接真实 S3 时，把 `-remote-*` 参数换成自己的凭据即可（`-remote-endpoint` 留空则使用 AWS）。

## Local storage layout

Objects live at `local-dir/{bucket}/{key}`, with an etag/content-type sidecar
under `local-dir/.meta/` and in-progress multipart uploads under
`local-dir/.multipart/`. The layout survives restarts, so the overlay is
durable on disk.

## Testing

Unit tests need no external services:

```bash
go test ./...
```

Integration tests exercise the overlay semantics (remote fallback reads,
local shadowing writes, merged listings, multipart) against a real S3
backend. They run when `S3_TEST_ENDPOINT`, `S3_TEST_ACCESS_KEY` and
`S3_TEST_SECRET_KEY` are set, and are skipped otherwise. CI starts a
[silo](https://github.com/pgsty/silo) container and runs them automatically.

## Limitations

- object and bucket deletion are not supported (`NotImplemented`)
- `chunked` (streaming) payload signing and presigned URLs are not supported
- multipart object `GET`/`HEAD` ETags carry the combined digest without the
  `-N` part count suffix
- listings merge both stores in full; very large buckets make listing slow
- an unreachable remote backend makes listings fail
