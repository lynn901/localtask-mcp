# localtask-mcp

Go 实现的 MCP(Model Context Protocol)服务器,把主机管理能力(shell 执行、文件读写、目录列表、系统信息、进程/磁盘/内存、kubectl)暴露为 MCP 工具。是原 `server.py`(零依赖 HTTP JSON API)的 MCP 重写,可被任何 MCP 客户端(Claude Code、Claude Desktop 等)调用。

- **语言**:Go 1.21+(在 1.26 构建),除官方 MCP SDK 外零非测试依赖
- **SDK**:[`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) v1.7
- **传输**:stdio(默认,本地客户端)+ 流式 HTTP(`-http`)

## 构建

```bash
go build -o localtask-mcp .
./localtask-mcp -h   # 验证
```

测试(集成测试经 stdio 启动并调用每个工具):

```bash
go test -v ./...
```

## 使用说明

从零到连通的完整流程。两种用法:本机用 **stdio**(无需 key、最简),或多客户端用 **HTTP + 多 key**。

### 快速开始(stdio,本机)

```bash
go build -o localtask-mcp .
# Claude Code 配置(.mcp.json):
#   {"mcpServers":{"localtask":{"command":"/abs/path/to/localtask-mcp"}}}
```

无认证(信任启动它的本地进程)、无网络、无监听 socket。本机单用户用优先这个。

### 快速开始(HTTP,多客户端)

1. 生成 key、写 `keys.json`(格式见下)。
2. 起 HTTP:`./localtask-mcp -http 0.0.0.0:8011 -keys keys.json`(或加密后用 `keys.json.enc`,见 [加密](#加密-keysjsonembed-key))。
3. 客户端连:`http://<host>:8011/mcp`,`Authorization: Bearer <key>`。

```bash
# 生成两个 key
openssl rand -hex 32   # yuan 用
openssl rand -hex 32   # zhao 用

# 起(明文 HTTP,仅可信网络)
./localtask-mcp -http 0.0.0.0:8011 -keys keys.json

# 验证
curl -H "Authorization: Bearer <yuan的key>" http://127.0.0.1:8011/    # 应 200
curl -H "Authorization: Bearer wrong"      http://127.0.0.1:8011/    # 应 401
```

### keys.json 格式

JSON 数组,每个对象一个 key。字段:

| 字段 | 必填 | 说明 |
|------|------|------|
| `key` | 是 | 256-bit hex(用 `openssl rand -hex 32` 生成)。比对常量时间,不泄露数量/身份 |
| `label` | 否 | 标签(如用户名),仅日志/辨识用 |
| `revoked` | 否 | `true` 则加载时跳过(key 失效但记录保留,便于审计/恢复) |

例子(两个有效 key + 一个已撤销):

```json
[
  {"key":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","label":"yuan"},
  {"key":"fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210","label":"zhao"},
  {"key":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef","label":"node1","revoked":true}
]
```

> 上面的 key 值是**示例占位**——别用真实 key 生成后再贴回文档。每个 key 授予**完整主机控制权**(任意 shell + 任意文件读写,以服务运行用户身份),当 root 凭据对待。

**轮换 key**:编辑 `keys.json`(或加密后的 `.enc`)→ 重启服务(无热加载)。撤销直接加 `"revoked":true` + 重启,不必删条目。

## 工具

| 工具 | 说明 | 参数 |
|------|------|------|
| `exec` | `sh -c` 执行,返回 exit/stdout/stderr | `command`(必填),`timeout`(默认 30) |
| `read` | 读 UTF-8 文本文件 | `path`(必填) |
| `write` | 写 UTF-8 文本(覆盖,自动建目录) | `path`(必填),`content`(必填) |
| `write_bytes` | 写二进制(覆盖,自动建目录) | `path`(必填),`contentHex` 或 `contentBase64` |
| `read_bytes` | 读文件为 base64(二进制安全,与 `write_bytes` 配对) | `path`(必填) |
| `list` | 列目录(`类型\t名称\t大小` 每行) | `path`(默认 `.`) |
| `info` | 主机信息:hostname/platform/time/user/cwd | — |
| `ps` | 内存 top 20 进程,或按名过滤 | `name`(可选,白名单过滤) |
| `df` | 磁盘占用(`df -h`) | — |
| `mem` | 内存占用(`free -h`) | — |
| `k8s` | kubectl(默认 `kubectl get nodes`) | `command`(默认),`timeout`(默认 30) |

字段必填/可选由 Go 结构标签决定:无 `omitempty` 的字段**必填**,SDK 在 handler 前校验。`ps` 的 `name` 用 `^[a-zA-Z0-9._-]+$` 白名单,防 shell 注入。

**文本 vs 二进制通道:** `read`/`write` 走 MCP 文本通道,只能忠实往返合法 UTF-8 文本;含 NUL(`\x00`)或非法 UTF-8 字节的内容经文本通道会被损坏。二进制内容(配置、密钥、压缩文件等)请用 `read_bytes`/`write_bytes`,它们以 base64 编码传输,任意字节可逐字节往返(`read_bytes` 与 `write_bytes` 的 `contentBase64` 对称配对)。**大文件走 HTTP `/download`/`/upload` 端点(见下),流式、不进模型上下文。**

## 运行

### stdio(默认,本地客户端)

```bash
./localtask-mcp
```

从 stdin 读 JSON-RPC,往 stdout 写,日志到 stderr。MCP 客户端自动以此模式启动。

### 流式 HTTP

HTTP 模式需**多 key bearer 认证**(见下),可选 **TLS**。

```bash
# 明文 HTTP,两 key 带标签
./localtask-mcp -http 127.0.0.1:8011 -tokens "$(openssl rand -hex 32):alice,$(openssl rand -hex 32):bob"

# HTTPS 自签证书(启动时生成,打印 SHA-256 指纹)
./localtask-mcp -http 127.0.0.1:8443 -tls-selfsigned -tokens "$TOKEN:alice"

# HTTPS 用自己证书
./localtask-mcp -http 127.0.0.1:8443 -cert fullchain.pem -key privkey.pem -tokens "$TOKEN:alice"
```

MCP 端点:`POST /mcp`(`Authorization: Bearer <key>`,`Accept: application/json, text/event-stream`)。健康/身份:`GET /`(也要有效 key)。

### 文件传输(HTTP:`/download`、`/upload`)

> 仅 HTTP 模式可用。stdio 模式无 HTTP 端点——小文件用 MCP 工具 `read_bytes`/`write_bytes`(内容进模型上下文)。

`/download`、`/upload` 提供**流式**文件上传下载,绕开模型上下文,支持 GB 级大文件、断点续传、sha256 校验。模型可用 `exec` 工具跑 `curl` 调用。所有请求带 `Authorization: Bearer <key>`,错误体统一 `application/json` `{"error","detail"}`。

**下载 `GET/HEAD /download?path=<abs>`**

- `GET`:标准 HTTP Range(断点续传、多段),`206` + `Content-Range`,`200` 全量。
- `HEAD`:返回 `X-File-Size`、`X-File-Sha256`(全文件,流式计算)、`ETag`(sha256)、`Accept-Ranges: bytes`。

```bash
KEY=...; BASE=http://127.0.0.1:8011
# 全量下载
curl -f -H "Authorization: Bearer $KEY" -o /tmp/big.tar "$BASE/download?path=/data/big.tar"
# 断点续传(中断后接着下)
curl -f -C - -H "Authorization: Bearer $KEY" -o /tmp/big.tar "$BASE/download?path=/data/big.tar"
# 分片拉取
curl -f -H "Authorization: Bearer $KEY" -H "Range: bytes=0-4194303" "$BASE/download?path=/data/big.tar" -o part0
# 校验:拉前/拉后取 sha256 核对
curl -fsS -I -H "Authorization: Bearer $KEY" "$BASE/download?path=/data/big.tar" | grep -i X-File-Sha256
```

**上传 `PUT/HEAD /upload?path=<abs>[&mode=<octal>]`**

分片续传协议(无会话状态,幂等拼接):每次 `PUT` 带 `Content-Range: bytes <start>-<end>/<total>`,body 是该切片;写临时文件 `<path>.ltmp`,末片(`end+1==total`)时全文件算 sha256,通过则原子 `rename` 到目标。

- 非末片:`206` `{"received":N,"total":T}`,临时文件保留。
- 末片:`200` `{"path","sha256","size"}`;带 `X-Expected-Sha256` 请求头则 server 比对,不符 `500` 并删临时文件。
- `HEAD /upload?path=<abs>`:返回 `X-Received`(临时文件当前长度),用于续传定位。

```bash
KEY=...; BASE=http://127.0.0.1:8011; FILE=/data/big.tar
# 整文件上传(一次)
SIZE=$(stat -c%s "$FILE"); END=$((SIZE-1))
curl -f -H "Authorization: Bearer $KEY" \
  -H "Content-Range: bytes 0-$END/$SIZE" \
  -H "X-Expected-Sha256: $(sha256sum "$FILE" | cut -d' ' -f1)" \
  --data-binary @"$FILE" "$BASE/upload?path=$FILE"

# 分片续传(每片 4MiB);中断后从 HEAD 查到的 offset 续
CHUNK=4194304
for off in $(seq 0 $CHUNK $((SIZE-1))); do
  end=$((off+CHUNK-1)); [ $end -ge $SIZE ] && end=$((SIZE-1))
  dd if="$FILE" bs=1 skip=$off count=$((end-off+1)) 2>/dev/null | \
  curl -f -H "Authorization: Bearer $KEY" \
    -H "Content-Range: bytes $off-$end/$SIZE" \
    --data-binary @- "$BASE/upload?path=$FILE"
done
# 续传定位(中断后接着传)
RCV=$(curl -fsS -I -H "Authorization: Bearer $KEY" "$BASE/upload?path=$FILE" | grep -i X-Received | tr -dc '0-9')
echo "server already has $RCV bytes"
```

**小文件 vs 大文件:** 走 MCP 工具(`read_bytes`/`write_bytes`)省事但内容进模型上下文,仅适合 <~1 MiB;走 HTTP 端点流式、不占上下文,适合大文件。

## 认证

| 传输 | 认证 |
|------|------|
| stdio | **无** — 信任启动它的本地进程 |
| HTTP | **多 key bearer**(必需) |

HTTP 请求须带 `Authorization: Bearer <key>`。key 启动时从以下来源加载(后者追加,都被接受):

| 来源 | flag/环境变量 | 格式 |
|------|------------|------|
| 内联 | `-tokens` 或 `MCP_TOKENS` | 逗号分隔 `key` 或 `key:label` |
| 文件 | `-keys <path>` | JSON 数组 `{"key","label"?,"revoked"?}` |
| 兼容 | `MCP_TOKEN`(仅当无其它时) | 单个裸 token |

比对常量时间(`crypto/subtle`,每请求比所有 key,不泄露数量/身份)。缺失/无效→`401` 带 `WWW-Authenticate: Bearer`。撤销 key(`"revoked":true`)加载时跳过。HTTP 模式启动时若零有效 key → 拒启(exit 2)。

```bash
openssl rand -hex 32   # 每个 key 256-bit hex
```

`keys.json` 格式与示例见 [使用说明](#keysjson-格式)。

> 每个 key 授予**完整主机控制权**(任意 shell + 任意文件读写,以服务运行用户身份)。当 root 凭据对待。轮换:编辑 `keys.json` + 重启(无热加载)。

## 加密 keys.json(embed key)

为避免明文 bearer key 留在文件里,可用 **AES-256-GCM** 加密 `keys.json`,解密密钥在构建时烧入二进制(不在磁盘、不在环境变量)。**密文经解密验证能还原原文后,明文源文件即被删除**——加密前先自己备份 `keys.json`。

**威胁模型与局限**:只防"拿到文件但没拿到二进制"(备份/git 泄露/同用户进程读文件);**不防**拿到二进制+文件(embedKey 可被 `strings`/反汇编提取),也不防 root/内存转储。更强模型请用 TLS 反代 + 密钥管理服务。

### 三步配置

```bash
# 1. 带 embedKey 编译二进制
EK="$(openssl rand -hex 32)"   # 64 hex = 32 字节 AES-256
echo "embedKey=$EK(留着重编译用)"  # 不存任何地方
go build -ldflags "-X main.embedKey=$EK" -o localtask-mcp .

# 2. 加密 keys.json → keys.json.enc(明文源随后被删)
./localtask-mcp -encrypt-keys keys.json keys.json.enc

# 3. 运行;-keys 用烧入的 key 透明解密
./localtask-mcp -http 127.0.0.1:8443 -tls-selfsigned -keys keys.json.enc
```

`-encrypt-keys` 形式:
- `-encrypt-keys <in>` — **原地**加密(路径不变,内容变密文)
- `-encrypt-keys <in> <out>` — 读 `<in>` 写 `<out>`,验证 `<out>` 能解密还原后**删除** `<in>`
- `-keep` — 双参数时不删 `<in>`(从自己保留的 master keys.json 刷新密文用)
- 对已加密文件再加密会被拒

无 embedKey 编译的二进制拒绝解密/拒绝加密;embedKey 不同则 GCM tag 校验失败(=解密错误)。

### 轮换 key(不改 embedKey)

```bash
./localtask-mcp -encrypt-keys keys.json keys.json.enc   # 用自存明文副本,明文被删
systemctl --user restart localtask-mcp
```

### 换 embedKey

```bash
EK2="$(openssl rand -hex 32)"
go build -ldflags "-X main.embedKey=$EK2" -o localtask-mcp .
./localtask-mcp -encrypt-keys keys.json keys.json.enc   # 用新二进制重加密
```

## systemd 部署

两种形态,按场景选:

- **user 级**(开发机/单用户):unit 文件在项目内 `localtask-mcp.service`,部署处用**符号链接**指向它(单一来源,改项目文件 + `daemon-reload` 即生效)。路径用 `%h`(用户家目录)跨主机/用户移植。默认登录后才跑;开机自启需 `loginctl enable-linger $USER`。
- **system 级**(服务器):RPM 装的 `localtask-mcp.system.service` → `/usr/lib/systemd/system/`,开机自启、不依赖登录。见下文 [RPM 打包](#rpm-打包rhel-系服务器部署)。

两个 unit 默认都监听 `0.0.0.0:8011`、无 TLS、从加密 `keys.json.enc` 加载 key。`0.0.0.0` = 所有网卡(内网可达),改 `127.0.0.1` 则只本机。

user 级 unit(项目内 `localtask-mcp.service`):

```ini
[Unit]
Description=localtask MCP server (HTTP, multi-key bearer, optional TLS)
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=10

[Service]
Type=simple
WorkingDirectory=%h/localtask
ExecStart=%h/localtask/localtask-mcp -http 0.0.0.0:8011 -keys %h/localtask/keys.json.enc
Restart=on-failure
RestartSec=3s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%h/localtask

[Install]
WantedBy=default.target
```

安装 user 级(无需 sudo):

```bash
mkdir -p ~/.config/systemd/user
ln -sf "$HOME/localtask/localtask-mcp.service" ~/.config/systemd/user/localtask-mcp.service
systemctl --user daemon-reload
systemctl --user enable --now localtask-mcp
journalctl --user -u localtask-mcp -f
```

注意:
- 二进制必须带 embedKey 编译过;否则解不开 `keys.json.enc`,服务起不来(失败循环重启,不暴露服务)。
- `0.0.0.0` + 无 TLS:key 在链路明文,仅可信网络用;不可信网络要加 TLS(见 [TLS](#tls))。
- exec/write 工具要跑任意 shell/写任意路径,故 unit 不做更广文件系统沙箱(会破坏工具);现有硬化项已是上限。

## RPM 打包(RHEL 系,服务器部署)

用 [nfpm](https://github.com/goreleaser/nfpm) 打 x86_64 + aarch64 RPM。包内二进制带**固定 embedKey**(经 `-ldflags` 烧入);装这个包的所有主机共享该 embedKey,各自在 `/etc/localtask` 放自己的 `keys.json.enc`(用同 embedKey 加密)。RPM 装 **system 级** unit(`/usr/lib/systemd/system/`),开机自启,不依赖登录。

```bash
# EMBED_KEY 须与加密 keys.json.enc 用的一致(64 hex)
EMBED_KEY=<你的-embed-key> ./packaging/build-rpm.sh
# → dist/localtask-mcp-<ver>-1.x86_64.rpm
#   dist/localtask-mcp-<ver>-1.aarch64.rpm
```

目标机安装:

```bash
sudo rpm -ivh --nosignature dist/localtask-mcp-<ver>-1.<arch>.rpm   # 内部自打包无签名,--nosignature 跳过

# 1) 建 key(每个 = 完整主机控制权)
echo -n '[{"key":"'$(openssl rand -hex 32)'","label":"alice"}]' | sudo tee /etc/localtask/keys.json
sudo chmod 640 /etc/localtask/keys.json

# 2) 用包内二进制加密(embedKey 已烧入,明文源随后被删)
sudo /usr/local/bin/localtask-mcp -encrypt-keys /etc/localtask/keys.json /etc/localtask/keys.json.enc

# 3) 起
sudo systemctl daemon-reload
sudo systemctl enable --now localtask-mcp
sudo journalctl -u localtask-mcp -f

# 4) RHEL 防火墙
sudo firewall-cmd --permanent --add-port=8011/tcp && sudo firewall-cmd --reload
```

SELinux:服务以 root 跑任意 shell + 任意写,enforcing 可能拦 AVC。起不来查 `sudo ausearch -m AVC -ts recent`,用 `audit2allow` 生成策略,**不要** `setenforce 0`。卸载:`sudo rpm -e localtask-mcp`(pre-remove 停+disable 服务);`/etc/localtask` 配置保留,需手动删。

## TLS

默认**明文 HTTP**(无 `-tls-selfsigned`/`-cert`/`-key`)。明文下 bearer key 在链路明文传输——仅可信内网/本机用;要暴露到不可信网络须开 TLS。最低 TLS 1.2。

三种启用方式,按场景选:

1. **固定自签 PEM(内网、Claude Code 等需固定证书的客户端)**——推荐用于可信内网要加密的场景。用 `openssl` 生成**一次**证书长期复用,服务用 `-cert/-key` 加载:
   ```bash
   openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
     -keyout key.pem -out cert.pem -days 3650 -nodes \
     -subj "/O=localtask-mcp" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
   chmod 600 key.pem cert.pem
   ./localtask-mcp -http 0.0.0.0:8443 -cert cert.pem -key key.pem -keys keys.json.enc
   ```
   Claude Code 客户端设 `NODE_EXTRA_CA_CERTS=/path/to/cert.pem`(追加信任该 PEM)即可连。比 `-tls-selfsigned` 适合 Claude Code:证书固定,不随重启变。

2. **自签自动生成(`-tls-selfsigned`)**——仅适合能关 TLS 校验或支持指纹 pin 的自定义客户端(curl `-k`、自定义 Go 客户端等)。启动时生成 ECDSA(P-256)自签证书(1 年,localhost+127.0.0.1),只存在内存、**每次启动换新**,往 stderr 打 SHA-256 指纹供 pin。证书不落盘、会变,故**不支持** `NODE_EXTRA_CA_CERTS`(无稳定 PEM),Claude Code 连不上。
   ```bash
   ./localtask-mcp -http 0.0.0.0:8443 -tls-selfsigned -keys keys.json.enc
   # 客户端:curl -k ... 或 pin 打印的指纹
   ```

3. **CA 证书(`-cert/-key`,Let's Encrypt 等)**——有真域名 + CA 证书用这个,Claude Code 等标准客户端自动信任,无需额外配置。

> Claude Code 默认严格校验 TLS(系统 CA + 内置 Mozilla CA),**不支持指纹 pin**,只支持 `NODE_EXTRA_CA_CERTS=<pem>` 追加信任。故 Claude Code 接 HTTPS 自签,用方式 1(固定 PEM)。

## 客户端配置

### Claude Code

stdio(本地主机管理,无需 key):

```json
{"mcpServers":{"localtask":{"command":"/abs/path/to/localtask-mcp"}}}
```

流式 HTTP(key 作 header):

```json
{
  "mcpServers": {
    "localtask": {
      "url": "http://127.0.0.1:8011/mcp",
      "headers": {"Authorization": "Bearer <your-key>"}
    }
  }
}
```

或 CLI:

```bash
claude mcp add localtask /abs/path/to/localtask-mcp                       # stdio
claude mcp add --transport http localtask http://127.0.0.1:8011/mcp \
  --header "Authorization: Bearer <your-key>"                               # HTTP
```

## 安全

服务以 OS 用户身份运行**任意 shell**、读写**任意可达文件**。专为可信本地/单用户场景(个人自动化桥接)。每个 key = 完整主机控制权,当 root 凭据对待。

- **HTTP**:多 key bearer + 可选 TLS,比原 server.py(无认证无 TLS)强。优先 loopback(`127.0.0.1`);`0.0.0.0` 仅在可信网络 + 配 TLS。
- **stdio**:无认证(信任启动进程)。本地用优先这个,无监听 socket。
- SDK 默认开 DNS-rebinding / 跨源保护(`DisableLocalhostProtection` 保留开)。

## 安装 Go(若无)

```bash
VER=1.26.6  # https://go.dev/dl 最新版
curl -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/go$VER.linux-amd64.tar.gz"
mkdir -p ~/.local && tar -C ~/.local -xzf /tmp/go.tar.gz
export PATH="$HOME/.local/go/bin:$PATH"
go version
```
