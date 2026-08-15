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

## 工具

| 工具 | 说明 | 参数 |
|------|------|------|
| `exec` | `sh -c` 执行,返回 exit/stdout/stderr | `command`(必填),`timeout`(默认 30) |
| `read` | 读 UTF-8 文本文件 | `path`(必填) |
| `write` | 写 UTF-8 文本(覆盖,自动建目录) | `path`(必填),`content`(必填) |
| `write_bytes` | 写二进制(覆盖,自动建目录) | `path`(必填),`contentHex` 或 `contentBase64` |
| `list` | 列目录(`类型\t名称\t大小` 每行) | `path`(默认 `.`) |
| `info` | 主机信息:hostname/platform/time/user/cwd | — |
| `ps` | 内存 top 20 进程,或按名过滤 | `name`(可选,白名单过滤) |
| `df` | 磁盘占用(`df -h`) | — |
| `mem` | 内存占用(`free -h`) | — |
| `k8s` | kubectl(默认 `kubectl get nodes`) | `command`(默认),`timeout`(默认 30) |
| `download` | 读文件为 base64(二进制安全) | `path`(必填) |

字段必填/可选由 Go 结构标签决定:无 `omitempty` 的字段**必填**,SDK 在 handler 前校验。`ps` 的 `name` 用 `^[a-zA-Z0-9._-]+$` 白名单,防 shell 注入。

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

`keys.json` 示例(一个有效,一个撤销):

```json
[
  {"key":"a1b2...","label":"alice"},
  {"key":"c3d4...","label":"ci-bot","revoked":true}
]
```

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

## systemd 部署(HTTP,无 TLS,加密 key)

user 级单元跑在回环 HTTP,从加密 `keys.json.enc` 加载。无需 sudo。unit 文件**在项目内**(`localtask-mcp.service`,单一来源),部署处用**符号链接**指向它,编辑项目文件 + `daemon-reload` 即可生效。

```ini
[Unit]
Description=localtask MCP server (HTTP, multi-key bearer, optional TLS)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%h/localtask
ExecStart=%h/localtask/localtask-mcp -http 0.0.0.0:8011 -keys %h/localtask/keys.json.enc
Restart=on-failure
RestartSec=3s

[Install]
WantedBy=default.target
```

`%h` = user 实例所属用户的家目录,故可跨主机/用户移植。

```bash
mkdir -p ~/.config/systemd/user
ln -sf "$HOME/localtask/localtask-mcp.service" ~/.config/systemd/user/localtask-mcp.service
systemctl --user daemon-reload
systemctl --user enable --now localtask-mcp
systemctl --user status localtask-mcp
journalctl --user -u localtask-mcp -f
```

注意:
- 二进制必须带 embedKey 编译过;否则解不开 `keys.json.enc`,服务起不来。
- 未登录也跑(开机自启)需 `loginctl enable-linger $USER`(可能需权限)。
- 监听 `0.0.0.0` + 无 TLS:key 在链路明文,仅可信网络用;不可信网络要加 TLS。

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

两种:
1. **自签(自动生成)**:`-tls-selfsigned`,启动时生成 ECDSA(P-256)自签证书(1 年有效,localhost+127.0.0.1),往 stderr 打 **SHA-256 指纹**,客户端 pin 该指纹。适合本地/可信内网。
2. **自己证书**:`-cert <pem>` + `-key <pem>`(如 Let's Encrypt)。有真域名 + CA 证书用这个。

无 `-tls-selfsigned`/`-cert`/`-key` → 明文 HTTP。最低 TLS 1.2。

> 注意:Claude Code 默认严格校验 TLS,**不支持指纹 pin**,只支持 `NODE_EXTRA_CA_CERTS=<pem>` 追加信任。`-tls-selfsigned` 证书只在内存、每次启动换新,故 Claude Code 连不上;要用 HTTPS 接 Claude Code 需固定 PEM(`-cert/-key`)+ `NODE_EXTRA_CA_CERTS` 指向它。

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

### 与 server.py 的映射

| server.py action | MCP 工具 | 说明 |
|---|---|---|
| exec | `exec` | 同语义,`sh -c` + timeout |
| read | `read` | UTF-8;二进制用 `download` |
| write | `write` | 现在自动建父目录 |
| PUT /upload | `write_bytes` | hex/base64 上传 |
| list | `list` | tab 分隔输出 |
| info | `info` | platform 现为 `os/arch` |
| ps | `ps` | 同名白名单 |
| df/mem | `df`/`mem` | 相同 |
| k8s | `k8s` | 同默认+timeout |
| — | `download` | 二进制安全读为 base64 |

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
