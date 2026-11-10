# overlay-s3

S3-compatible gateway that layers a write overlay S3 on top of an existing
baseline S3. Every client bucket is configured separately: reads prefer its
overlay store and fall back to its baseline, writes land only in the overlay
store, and listings merge both stores with overlay keys shadowing baseline
ones.

Useful when you want to keep existing S3 buckets as read-only baselines while
writes go to a separate S3 — no deletion, no propagation, just a merged view.
Both layers are ordinary S3 endpoints, so no local storage or multipart
bookkeeping is needed: multipart upload state is owned by the overlay backend
itself.

The gateway serves exactly the buckets declared in its config file; anything
else answers `NoSuchBucket`. Buckets are independent, so each one may point at
its own overlay and baseline endpoints, or share an overlay bucket with other
buckets through distinct key prefixes.

## How it works

```
client ──▶ overlay-s3 (S3 API, SigV4) ──▶ overlay S3 (writes, reads win)
                                     └──▶ baseline S3 (read fallback)
```

- `GET` / `HEAD`: overlay first, baseline on miss
- ranged `GET`: the byte range is resolved against the object size and passed
  through to the backend
- `PUT` / multipart: proxied to the overlay S3 only, the baseline is never modified
- `ListObjects`: merged view of both stores, overlay keys win
- `ListBuckets`: the buckets declared in the config; an unconfigured bucket
  answers `NoSuchBucket`
- deletion is not supported and answered with `NotImplemented`
- `GET /_overlay-s3`: serves this README as markdown, so the running gateway
  documents its own configuration; it needs no credentials and is the only
  non-S3 path (bucket names cannot contain `_`, so nothing can collide with it)

### Overlay key mapping

The overlay layer nests a client-visible bucket inside the physical bucket
configured for it, under that bucket's prefix: client bucket `b` with key `k`
maps to

```
<overlay-bucket>/<overlay-prefix>/b/k
```

Client buckets are therefore just key namespaces in the overlay backend:
create-bucket writes a zero-byte directory marker at `prefix/b/`, while
existence and the bucket list itself come from the config.

The protocol layer (S3 XML, multipart flow, pagination, error codes) is
provided by [gofakes3](https://github.com/johannesboyne/gofakes3); signature
validation is enforced by a SigV4 middleware in front of it.

## Usage

Everything is configured by a TOML file (`-config` defaults to
`config.toml`); the listener address, the client credentials and one entry per
client bucket:

```toml
listen = ":8080"
auth_key = "client-key"
auth_secret = "client-secret"

[buckets.demo.overlay]
endpoint = "http://127.0.0.1:9000"
region = "us-east-1"
access_key = "overlay-access-key"
secret_key = "overlay-secret-key"
bucket = "overlay-data"
prefix = "gateway"

[buckets.demo.baseline]
endpoint = "https://s3.amazonaws.com"
region = "us-east-1"
access_key = "baseline-access-key"
secret_key = "baseline-secret-key"
path_style = false
```

```bash
go build -o overlay-s3 .
./overlay-s3 -config overlay-s3.toml
```

| key | default | description |
| --- | --- | --- |
| `listen` | `:8080` | HTTP listen address |
| `auth_key` / `auth_secret` | (disabled) | key pair clients must sign with; an empty `auth_key` disables signature checks |
| `buckets.<name>` | (required, at least one) | one section per client-visible bucket; `<name>` is the bucket clients address |
| `buckets.<name>.overlay.endpoint` | (required) | overlay S3 endpoint receiving all writes of this bucket |
| `buckets.<name>.overlay.region` | `us-east-1` | overlay S3 region |
| `buckets.<name>.overlay.access_key` / `.secret_key` | (required) | credentials for the overlay S3 |
| `buckets.<name>.overlay.bucket` | (required) | physical bucket holding the overlay data of this client bucket |
| `buckets.<name>.overlay.prefix` | (empty) | key prefix inside the physical bucket; client `b/k` maps to `prefix/b/k` |
| `buckets.<name>.overlay.path_style` | `true` | path-style addressing for the overlay endpoint (disable for AWS) |
| `buckets.<name>.baseline.endpoint` | (required) | baseline S3 endpoint for read fallback |
| `buckets.<name>.baseline.region` | `us-east-1` | baseline S3 region |
| `buckets.<name>.baseline.access_key` / `.secret_key` | (required) | credentials for the baseline S3 |
| `buckets.<name>.baseline.path_style` | `true` | path-style addressing for the baseline endpoint (disable for AWS) |

Unknown keys are rejected so a typo cannot silently disable an option, and a
missing required key fails at startup. The baseline has no bucket of its own:
client bucket `demo` reads its fallback objects from bucket `demo` on the
baseline endpoint, which therefore has to exist there. Several client buckets
may share one physical overlay bucket as long as their `prefix` values differ.

Clients (aws cli, SDKs, rclone) connect to the gateway and sign with
`auth_key`/`auth_secret`:

```bash
aws --endpoint-url http://127.0.0.1:8080 s3api create-bucket --bucket demo
aws --endpoint-url http://127.0.0.1:8080 s3 cp local-file s3://demo/key
aws --endpoint-url http://127.0.0.1:8080 s3 ls s3://demo/
```

## Docker Compose

也可以直接用 Docker 跑。下面这个 compose 文件构建镜像，并起一个
[silo](https://github.com/pgsty/silo) 容器（S3 兼容后端）同时扮演两个角色：
`overlay-data` bucket 作为写层，同一实例里的其它 bucket 作为基线，得到开箱即用的演示环境。
真实使用时把 `[buckets.<name>.baseline]` 换成已有的生产 S3 即可：

`overlay-s3.toml`：

```toml
listen = ":8080"
auth_key = "demo"
auth_secret = "demo-secret"

[buckets.demo.overlay]
endpoint = "http://silo:9000"
region = "us-east-1"
access_key = "minioadmin"
secret_key = "minioadmin-secret"
bucket = "overlay-data"
prefix = "gateway"

[buckets.demo.baseline]
endpoint = "http://silo:9000"
region = "us-east-1"
access_key = "minioadmin"
secret_key = "minioadmin-secret"

# 每个要对外提供的 bucket 都要有一份自己的 overlay/baseline 配置；
# 共用同一个 overlay bucket 时用不同的 prefix 区分
[buckets.baseline-data.overlay]
endpoint = "http://silo:9000"
region = "us-east-1"
access_key = "minioadmin"
secret_key = "minioadmin-secret"
bucket = "overlay-data"
prefix = "gateway"

[buckets.baseline-data.baseline]
endpoint = "http://silo:9000"
region = "us-east-1"
access_key = "minioadmin"
secret_key = "minioadmin-secret"
```

`compose.yaml`：

```yaml
services:
  overlay-s3:
    build: .
    ports:
      - "8080:8080"
    command: ["-config", "/etc/overlay-s3/config.toml"]
    volumes:
      - ./overlay-s3.toml:/etc/overlay-s3/config.toml:ro
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
- multi-part `Range` requests (`bytes=0-1,5-9`) are not served as
  `multipart/byteranges`: the header is ignored and the whole object returned
- `chunked` (streaming) payload signing and presigned URLs are not supported
- multipart object `GET`/`HEAD` ETags carry the combined digest without the
  `-N` part count suffix
- listings merge both stores in full; very large buckets make listing slow
- an unreachable overlay backend fails writes; an unreachable baseline fails
  reads that miss the overlay
