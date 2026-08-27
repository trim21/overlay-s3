# overlay-s3

S3-compatible gateway that layers a write overlay S3 on top of an existing
baseline S3. It exposes one merged view to clients: reads prefer the overlay
store and fall back to the baseline, writes land only in the overlay store,
and listings merge both stores with overlay keys shadowing baseline ones.

Useful when you want to keep an existing S3 bucket as a read-only baseline
while writes go to a separate S3 — no deletion, no propagation, just a
merged view. Both layers are ordinary S3 endpoints, so no local storage or
multipart bookkeeping is needed: multipart upload state is owned by the
overlay backend itself.

## How it works

```
client ──▶ overlay-s3 (S3 API, SigV4) ──▶ overlay S3 (writes, reads win)
                                     └──▶ baseline S3 (read fallback)
```

- `GET` / `HEAD`: overlay first, baseline on miss
- `PUT` / multipart: proxied to the overlay S3 only, the baseline is never modified
- `ListObjects` / `ListBuckets`: merged view of both stores, overlay keys win
- deletion is not supported and answered with `NotImplemented`

### Overlay key mapping

The overlay layer nests all client-visible buckets inside one physical
bucket under a configurable prefix: client bucket `b` with key `k` maps to

```
<overlay-bucket>/<overlay-prefix>/b/k
```

Client buckets are therefore just key namespaces in the overlay backend:
create-bucket writes a zero-byte directory marker at `prefix/b/`, bucket
existence is checked by prefix listing, and `ListBuckets` derives buckets
from the first-level prefixes under the mapping root.

The protocol layer (S3 XML, multipart flow, pagination, error codes) is
provided by [gofakes3](https://github.com/johannesboyne/gofakes3); signature
validation is enforced by a SigV4 middleware in front of it.

## Usage

```bash
go build -o overlay-s3 .
./overlay-s3 \
  -listen :8080 \
  -overlay-endpoint http://127.0.0.1:9000 \
  -overlay-region us-east-1 \
  -overlay-access-key <overlay-access-key> \
  -overlay-secret-key <overlay-secret-key> \
  -overlay-bucket overlay-data \
  -overlay-prefix gateway \
  -baseline-endpoint https://s3.amazonaws.com \
  -baseline-region us-east-1 \
  -baseline-access-key <baseline-access-key> \
  -baseline-secret-key <baseline-secret-key> \
  -auth-key <client-key> \
  -auth-secret <client-secret>
```

| flag | default | description |
| --- | --- | --- |
| `-listen` | `:8080` | HTTP listen address |
| `-overlay-endpoint` | (required) | overlay S3 endpoint receiving all writes |
| `-overlay-region` | `us-east-1` | overlay S3 region |
| `-overlay-access-key` / `-overlay-secret-key` | (required) | credentials for the overlay S3 |
| `-overlay-bucket` | (required) | physical bucket holding all overlay data |
| `-overlay-prefix` | (empty) | key prefix inside the overlay bucket; client `b/k` maps to `prefix/b/k` |
| `-overlay-path-style` | `true` | use path-style addressing for the overlay S3 endpoint (disable for AWS) |
| `-baseline-endpoint` | (AWS) | baseline S3 endpoint for read fallback, empty uses AWS |
| `-baseline-region` | `us-east-1` | baseline S3 region |
| `-baseline-access-key` / `-baseline-secret-key` | | credentials for the baseline S3 |
| `-baseline-path-style` | `true` | use path-style addressing for the baseline S3 endpoint (disable for AWS) |
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
[silo](https://github.com/pgsty/silo) 容器（S3 兼容后端）同时扮演两个角色：
`overlay-data` bucket 作为写层，同一实例里的其它 bucket 作为基线，得到开箱即用的演示环境。
真实使用时把 `-baseline-*` 换成已有的生产 S3 即可：

```yaml
services:
  overlay-s3:
    build: .
    ports:
      - "8080:8080"
    command:
      - -listen=:8080
      - -overlay-endpoint=http://silo:9000
      - -overlay-region=us-east-1
      - -overlay-access-key=minioadmin
      - -overlay-secret-key=minioadmin-secret
      - -overlay-bucket=overlay-data
      - -overlay-prefix=gateway
      - -baseline-endpoint=http://silo:9000
      - -baseline-region=us-east-1
      - -baseline-access-key=minioadmin
      - -baseline-secret-key=minioadmin-secret
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

写操作只落在 silo 的 `overlay-data` bucket 的 `gateway/` 前缀下，
基线 bucket 只提供读回退，不会被修改。

## Testing

Unit tests need no external services:

```bash
go test ./...
```

Integration tests exercise the overlay semantics (baseline fallback reads,
overlay shadowing writes, prefix mapping, merged listings, multipart) against
a real S3 backend. They run when `S3_TEST_ENDPOINT`, `S3_TEST_ACCESS_KEY` and
`S3_TEST_SECRET_KEY` are set, and are skipped otherwise. CI starts a
[silo](https://github.com/pgsty/silo) container and runs them automatically;
the same instance plays both roles, with the overlay mapped into a dedicated
bucket under a `gw/` prefix.

CI also boots the real gateway binary and drives it with the
[mc](https://min.io/docs/minio/linux/reference/minio-mc.html) client
(`ls`/`cp`/`cat`, including shadowing and multipart) for an end-to-end check
of client compatibility.

## Limitations

- object and bucket deletion are not supported (`NotImplemented`)
- `chunked` (streaming) payload signing and presigned URLs are not supported
- multipart object `GET`/`HEAD` ETags carry the combined digest without the
  `-N` part count suffix
- listings merge both stores in full; very large buckets make listing slow
- an unreachable overlay backend fails writes; an unreachable baseline fails
  reads that miss the overlay
