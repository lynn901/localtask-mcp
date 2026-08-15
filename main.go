// Command localtask-mcp is a Go MCP server exposing host-management
// capabilities (shell exec, file read/write, directory listing, system info,
// process/disk/memory stats, kubectl) over the Model Context Protocol.
//
// It is a Go reimplementation of the original server.py, exposed as MCP tools
// instead of an ad-hoc HTTP JSON API. Supports stdio (default, for local MCP
// clients such as Claude Code) and streamable HTTP transports.
//
// HTTP transport features:
//   - Multi-key bearer auth (see keyStore): keys from MCP_TOKENS env or a
//     keys.json file via -keys.
//   - Optional TLS with auto-generated self-signed certificates (-tls-selfsigned)
//     or your own cert/key files (-tls, -cert, -key).
//   - Optional AES-256-GCM encryption of keys.json: build with
//     `-ldflags "-X main.embedKey=<hex>"`, encrypt the file with
//     `-encrypt-keys [-keep] <file> [out]` (the plaintext source is deleted
//     unless -keep is set), and at runtime -keys transparently decrypts it.
package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// embedKey is the AES-256 key (64 hex chars / 32 bytes) used to decrypt an
// encrypted keys.json. It is injected at build time via:
//
//	go build -ldflags "-X main.embedKey=<hex>" .
//
// When empty (no injection), encrypted keys.json files cannot be decrypted and
// the -encrypt-keys subcommand is refused. The key lives only in the binary
// (and the process memory at runtime); it is never read from disk or env.
var embedKey string

// encMagic identifies a localtask-mcp encrypted keys file:
// "LTM1" + 12-byte nonce + AES-256-GCM ciphertext+tag.
var encMagic = []byte("LTM1")

// toolError returns an MCP tool result that flags an error to the client
// (IsError=true) while still surfacing the message as text content.
func toolError(format string, args ...any) (*mcp.CallToolResult, any, error) {
	msg := fmt.Sprintf(format, args...)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}, nil, nil
}

// textResult returns a successful tool result whose text content is s.
func textResult(s string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: s}},
	}, nil, nil
}

// ----- Tool input types ----------------------------------------------------

type ExecInput struct {
	Command string `json:"command" jsonschema:"The shell command to run on the host"`
	Timeout int    `json:"timeout,omitempty" jsonschema:"Max seconds before the command is killed. Defaults to 30."`
}

type ReadInput struct {
	Path string `json:"path" jsonschema:"Absolute or relative path of the text file to read (UTF-8)"`
}

type WriteInput struct {
	Path    string `json:"path" jsonschema:"Destination file path (UTF-8, overwrites)"`
	Content string `json:"content" jsonschema:"Text content to write"`
}

type WriteBytesInput struct {
	Path        string `json:"path" jsonschema:"Destination file path (binary-safe, overwrites)"`
	ContentHex  string `json:"contentHex,omitempty" jsonschema:"Hex-encoded bytes to write. Exactly one of contentHex or contentBase64 must be set."`
	ContentB64  string `json:"contentBase64,omitempty" jsonschema:"Base64-encoded bytes to write (standard encoding)."`
}

type ListInput struct {
	Path string `json:"path,omitempty" jsonschema:"Directory to list. Defaults to the current working directory."`
}

type PSInput struct {
	Name string `json:"name,omitempty" jsonschema:"Filter processes by name (alphanumeric, dash, underscore, dot only). If omitted, lists top 20 by memory."`
}

type K8sInput struct {
	Command string `json:"command,omitempty" jsonschema:"kubectl command to run. Defaults to kubectl get nodes."`
	Timeout int    `json:"timeout,omitempty" jsonschema:"Max seconds before the command is killed. Defaults to 30."`
}

type DownloadInput struct {
	Path string `json:"path" jsonschema:"Path of the file to download as base64."`
}

// ----- Handlers ------------------------------------------------------------

func handleExec(ctx context.Context, _ *mcp.CallToolRequest, in ExecInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Command) == "" {
		return toolError("command cannot be empty")
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", in.Command)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	rc := cmd.ProcessState.ExitCode()
	if ctxErr := cctx.Err(); ctxErr == context.DeadlineExceeded {
		return toolError("timeout after %ds", timeout)
	}
	out := stdout.String()
	if err != nil && out == "" {
		// Non-zero exit with no stdout: surface stderr as an MCP error so the
		// client sees it clearly. Otherwise return combined output below.
		se := stderr.String()
		if se == "" {
			se = err.Error()
		}
		return toolError("command failed (exit %d): %s", rc, se)
	}
	return textResult(fmt.Sprintf("exit: %d\n--- stdout ---\n%s\n--- stderr ---\n%s", rc, out, stderr.String()))
}

func handleRead(_ context.Context, _ *mcp.CallToolRequest, in ReadInput) (*mcp.CallToolResult, any, error) {
	raw, err := os.ReadFile(in.Path)
	if err != nil {
		return toolError("read failed: %v", err)
	}
	return textResult(string(raw))
}

func handleWrite(_ context.Context, _ *mcp.CallToolRequest, in WriteInput) (*mcp.CallToolResult, any, error) {
	if err := os.MkdirAll(filepath.Dir(in.Path), 0o755); err != nil {
		return toolError("mkdir failed: %v", err)
	}
	if err := os.WriteFile(in.Path, []byte(in.Content), 0o644); err != nil {
		return toolError("write failed: %v", err)
	}
	return textResult(fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path))
}

func handleWriteBytes(_ context.Context, _ *mcp.CallToolRequest, in WriteBytesInput) (*mcp.CallToolResult, any, error) {
	if in.ContentHex == "" && in.ContentB64 == "" {
		return toolError("exactly one of contentHex or contentBase64 must be provided")
	}
	if in.ContentHex != "" && in.ContentB64 != "" {
		return toolError("contentHex and contentBase64 are mutually exclusive")
	}
	var data []byte
	var err error
	if in.ContentHex != "" {
		data, err = hex.DecodeString(in.ContentHex)
		if err != nil {
			return toolError("invalid hex: %v", err)
		}
	} else {
		data, err = base64.StdEncoding.DecodeString(in.ContentB64)
		if err != nil {
			return toolError("invalid base64: %v", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(in.Path), 0o755); err != nil {
		return toolError("mkdir failed: %v", err)
	}
	if err := os.WriteFile(in.Path, data, 0o644); err != nil {
		return toolError("write failed: %v", err)
	}
	return textResult(fmt.Sprintf("wrote %d bytes to %s", len(data), in.Path))
}

func handleList(_ context.Context, _ *mcp.CallToolRequest, in ListInput) (*mcp.CallToolResult, any, error) {
	dir := in.Path
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return toolError("list failed: %v", err)
	}
	var b strings.Builder
	for _, e := range entries {
		info, err := e.Info()
		typ := "dir"
		size := int64(0)
		if err == nil {
			size = info.Size()
			if !e.IsDir() {
				typ = "file"
			}
		}
		fmt.Fprintf(&b, "%s\t%s\t%d\n", typ, e.Name(), size)
	}
	if b.Len() == 0 {
		return textResult("(empty directory)")
	}
	return textResult(b.String())
}

type hostInfo struct {
	Hostname  string `json:"hostname"`
	Platform  string `json:"platform"`
	GoRuntime string `json:"go"`
	Time      string `json:"time"`
	User      string `json:"user"`
	Cwd       string `json:"cwd"`
}

func handleInfo(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	user := os.Getenv("USER")
	if user == "" {
		user = "unknown"
	}
	cwd, _ := os.Getwd()
	info := hostInfo{
		Hostname:  hostname(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		GoRuntime: runtime.Version(),
		Time:      time.Now().Format(time.RFC3339),
		User:      user,
		Cwd:       cwd,
	}
	b, _ := json.Marshal(info)
	return textResult(string(b))
}

var psNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func handlePS(_ context.Context, _ *mcp.CallToolRequest, in PSInput) (*mcp.CallToolResult, any, error) {
	var cmd *exec.Cmd
	if in.Name != "" {
		if !psNameRe.MatchString(in.Name) {
			return toolError("invalid process name (alphanumeric, dash, underscore, dot only)")
		}
		cmd = exec.Command("sh", "-c", fmt.Sprintf("ps aux | grep -i %s | grep -v grep", in.Name))
	} else {
		cmd = exec.Command("sh", "-c", "ps aux --sort=-%mem | head -20")
	}
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return toolError("ps failed: %v", err)
	}
	s := string(out)
	if s == "" {
		s = "No matching processes"
	}
	return textResult(s)
}

func handleDF(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	out, err := exec.Command("df", "-h").Output()
	if err != nil {
		return toolError("df failed: %v", err)
	}
	return textResult(string(out))
}

func handleMem(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	out, err := exec.Command("free", "-h").Output()
	if err != nil {
		return toolError("free failed: %v", err)
	}
	return textResult(string(out))
}

func handleK8s(ctx context.Context, _ *mcp.CallToolRequest, in K8sInput) (*mcp.CallToolResult, any, error) {
	cmd := in.Command
	if strings.TrimSpace(cmd) == "" {
		cmd = "kubectl get nodes"
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, "sh", "-c", cmd)
	var stdout, stderr strings.Builder
	c.Stdout = &stdout
	c.Stderr = &stderr
	_ = c.Run()
	if ctxErr := cctx.Err(); ctxErr == context.DeadlineExceeded {
		return toolError("timeout after %ds", timeout)
	}
	rc := -1
	if c.ProcessState != nil {
		rc = c.ProcessState.ExitCode()
	}
	return textResult(fmt.Sprintf("exit: %d\n--- stdout ---\n%s\n--- stderr ---\n%s", rc, stdout.String(), stderr.String()))
}

func handleDownload(_ context.Context, _ *mcp.CallToolRequest, in DownloadInput) (*mcp.CallToolResult, any, error) {
	raw, err := os.ReadFile(in.Path)
	if err != nil {
		return toolError("read failed: %v", err)
	}
	return textResult(base64.StdEncoding.EncodeToString(raw))
}

// ----- Server wiring -------------------------------------------------------

func newServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "localtask",
		Version: "v1.0.0",
	}, &mcp.ServerOptions{
		Instructions: "Host management server. Tools: exec (run shell commands), read/write/write_bytes (files), list (dirs), info (host), ps/df/mem (system stats), k8s (kubectl), download (file as base64). Use read/write for text, write_bytes/download for binary. All commands run as the server's OS user.",
	})

	// Host management tools (mirror of server.py).
	mcp.AddTool(s, &mcp.Tool{
		Name:        "exec",
		Description: "Run a shell command on the host (via sh -c). Returns exit code, stdout and stderr.",
	}, handleExec)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "read",
		Description: "Read a UTF-8 text file and return its contents.",
	}, handleRead)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "write",
		Description: "Write UTF-8 text content to a file (overwrites; creates parent dirs).",
	}, handleWrite)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "write_bytes",
		Description: "Write binary content to a file (overwrites; creates parent dirs). Provide bytes hex-encoded in contentHex or base64 in contentBase64.",
	}, handleWriteBytes)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list",
		Description: "List directory contents (type, name, size per line).",
	}, handleList)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "info",
		Description: "Return host system info (hostname, platform, time, user, cwd).",
	}, handleInfo)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ps",
		Description: "List top processes by memory, or filter by process name.",
	}, handlePS)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "df",
		Description: "Disk usage (df -h).",
	}, handleDF)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mem",
		Description: "Memory usage (free -h).",
	}, handleMem)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "k8s",
		Description: "Run a kubectl command. Defaults to 'kubectl get nodes'.",
	}, handleK8s)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "download",
		Description: "Read a file and return its raw bytes base64-encoded (binary-safe).",
	}, handleDownload)

	return s
}

func main() {
	// Subcommand: -encrypt-keys [-keep] <file> [out] encrypts a plaintext
	// keys.json to <out> (or <file> if one arg, in place) using the built-in
	// embedKey, then removes the plaintext source unless -keep is set. Run
	// before starting the server; keep your own backup of the plaintext.
	if len(os.Args) >= 2 && os.Args[1] == "-encrypt-keys" {
		runEncryptKeys(os.Args[2:])
		return
	}

	var (
		httpAddr  = flag.String("http", "", "If set, serve streamable HTTP on this address (e.g. ':8011' or '127.0.0.1:8443'). Defaults to stdio transport.")
		stateless = flag.Bool("stateless", false, "Run HTTP transport in stateless mode (no session IDs).")
		tokensEnv = flag.String("tokens", "", "Comma-separated bearer keys, each optionally 'key:label'. Falls back to MCP_TOKENS env. A legacy single MCP_TOKEN env is also accepted.")
		keysFile  = flag.String("keys", "", "Path to a keys.json file (array of {key,label?,revoked?}), or an encrypted keys.json.enc produced by -encrypt-keys. Loaded once at startup; no hot reload.")
		tlsSelf   = flag.Bool("tls-selfsigned", false, "Generate a self-signed TLS certificate at startup and serve HTTPS. Prints the SHA-256 fingerprint to stderr.")
		certFile  = flag.String("cert", "", "PEM cert path for TLS (requires -key).")
		keyFile   = flag.String("key", "", "PEM private key path for TLS (requires -cert).")
	)
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := newServer()

	if *httpAddr == "" {
		// stdio transport: the default for local MCP clients. No auth (the
		// client is a local process spawned by a trusted user).
		if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// HTTP transport: require at least one bearer key.
	ks := loadKeyStore(*tokensEnv, *keysFile)
	if ks.empty() {
		fmt.Fprintln(os.Stderr, "localtask-mcp: HTTP mode requires authentication. Set -tokens, MCP_TOKENS, or -keys <keys.json>.")
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "localtask-mcp: loaded %d active bearer key(s)\n", len(ks.keys))

	// Resolve TLS: self-signed (-tls-selfsigned), explicit files (-cert/-key),
	// or none (plain HTTP).
	tlsCfg, err := resolveTLS(*tlsSelf, *certFile, *keyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "localtask-mcp: TLS error: %v\n", err)
		os.Exit(2)
	}

	// Streamable HTTP transport. MaxRequestBodyBytes is raised so large file
	// writes / command outputs are not rejected with 413. A negative value
	// disables the cap (only suitable for trusted local use).
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, &mcp.StreamableHTTPOptions{
		Stateless:          *stateless,
		MaxRequestBodyBytes: -1,
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok","transport":"streamable-http","endpoint":"/mcp"}`)
	})

	// Wrap everything in multi-key bearer auth. stdio is not affected.
	authed := authMiddleware(mux, ks)

	scheme := "http"
	if tlsCfg != nil {
		scheme = "https"
	}
	fmt.Fprintf(os.Stderr, "localtask-mcp: listening on %s://%s/mcp (stateless=%v, auth=multi-key, tls=%v)\n", scheme, *httpAddr, *stateless, tlsCfg != nil)
	httpSrv := &http.Server{
		Addr:      *httpAddr,
		Handler:   authed,
		TLSConfig: tlsCfg,
	}
	if tlsCfg != nil {
		// Certificates are already in tlsCfg; pass empty file args so Go uses
		// the configured TLSConfig.Certificates.
		if err := httpSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

// loadKeyStore builds a keyStore from the -tokens flag (falling back to the
// MCP_TOKENS env var) and/or a -keys file. The -keys file may be either a
// plaintext keys.json (array of keyEntry) or an encrypted file produced by
// -encrypt-keys (recognized by the "LTM1" magic), transparently decrypted
// with the built-in embedKey. Entries are comma-separated, each "key" or
// "key:label". A legacy bare MCP_TOKEN env var is also accepted.
func loadKeyStore(tokensFlag, keysFile string) *keyStore {
	var entries []keyEntry

	if keysFile != "" {
		raw, err := os.ReadFile(keysFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "localtask-mcp: cannot read -keys file %s: %v\n", keysFile, err)
			os.Exit(2)
		}
		// Encrypted file? Decrypt with the built-in embedKey.
		if isEncryptedKeys(raw) {
			plain, derr := decryptKeys(raw)
			if derr != nil {
				fmt.Fprintf(os.Stderr, "localtask-mcp: cannot decrypt -keys file %s: %v\n", keysFile, derr)
				os.Exit(2)
			}
			raw = plain
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			fmt.Fprintf(os.Stderr, "localtask-mcp: invalid keys file: %v\n", err)
			os.Exit(2)
		}
	}

	toks := tokensFlag
	if toks == "" {
		toks = os.Getenv("MCP_TOKENS")
	}
	for _, t := range splitNonEmpty(toks, ",") {
		key, label := t, ""
		if i := strings.Index(t, ":"); i >= 0 {
			key, label = t[:i], t[i+1:]
		}
		if key != "" {
			entries = append(entries, keyEntry{Key: key, Label: label})
		}
	}
	// Legacy single-token fallback when nothing else was provided.
	if len(entries) == 0 {
		if legacy := os.Getenv("MCP_TOKEN"); legacy != "" {
			entries = append(entries, keyEntry{Key: legacy})
		}
	}
	return newKeyStore(entries)
}

// splitNonEmpty splits s on sep and drops empty/whitespace-only parts.
func splitNonEmpty(s, sep string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ----- Encrypted keys.json (AES-256-GCM, embedKey) ------------------------

// runEncryptKeys implements the -encrypt-keys subcommand.
//
// Usage:
//
//	localtask-mcp -encrypt-keys [-keep] <keys.json> [out.enc]
//
// One argument: the file is encrypted in place (overwritten with ciphertext).
// Two arguments: reads the plaintext <keys.json>, writes ciphertext to
// <out.enc>, then deletes the plaintext <keys.json>.
//
// In both forms the plaintext source is destroyed unless -keep is passed, so
// keep your own backup of keys.json elsewhere before running this. The
// ciphertext is verified to round-trip (decrypt back to the original) before
// the plaintext is removed, so a source is never deleted in favor of an
// undecryptable file.
//
// -keep keeps the plaintext source untouched (use it to refresh <out> from a
// master keys.json you do not want consumed).
func runEncryptKeys(args []string) {
	fs := flag.NewFlagSet("encrypt-keys", flag.ExitOnError)
	keep := fs.Bool("keep", false, "do not delete the plaintext source after encrypting")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fmt.Fprintln(os.Stderr, "usage: localtask-mcp -encrypt-keys [-keep] <keys.json> [out.enc]")
		fmt.Fprintln(os.Stderr, "encrypts keys.json with the built-in embedKey (AES-256-GCM).")
		fmt.Fprintln(os.Stderr, "one arg = encrypt in place; two args = read <in>, write <out>, delete <in>.")
		fmt.Fprintln(os.Stderr, "the plaintext source is destroyed unless -keep is set; keep your own backup elsewhere.")
		os.Exit(2)
	}
	inPath := fs.Arg(0)
	outPath := inPath // in-place by default
	if fs.NArg() == 2 {
		outPath = fs.Arg(1)
	}
	n, deleted, err := encryptKeysFiles(inPath, outPath, *keep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "localtask-mcp: %v\n", err)
		os.Exit(2)
	}
	verb := "in place"
	if inPath != outPath {
		verb = "→ " + outPath
	}
	fmt.Fprintf(os.Stderr, "localtask-mcp: encrypted %d bytes (%s, AES-256-GCM)\n", n, verb)
	if deleted {
		fmt.Fprintf(os.Stderr, "localtask-mcp: deleted plaintext source %s\n", inPath)
	}
	fmt.Fprintln(os.Stderr, "localtask-mcp: plaintext destroyed — keep your own backup of keys.json elsewhere; it is not recoverable from this directory.")
}

// encryptKeysFiles reads plaintext keys from inPath, writes AES-256-GCM
// ciphertext to outPath (atomically), verifies the written file round-trips
// to the original plaintext, and — unless inPath == outPath or keepSource is
// set — removes the plaintext source. It returns the ciphertext length and
// whether the plaintext source was removed. On any failure the plaintext
// source is left intact.
func encryptKeysFiles(inPath, outPath string, keepSource bool) (n int, deleted bool, err error) {
	if embedKey == "" {
		return 0, false, fmt.Errorf("-encrypt-keys requires the binary to be built with -ldflags \"-X main.embedKey=<hex>\"")
	}
	plain, err := os.ReadFile(inPath)
	if err != nil {
		return 0, false, fmt.Errorf("read %s: %w", inPath, err)
	}
	// Refuse to "encrypt" an already-encrypted file (it's not valid JSON).
	if isEncryptedKeys(plain) {
		return 0, false, fmt.Errorf("%s is already encrypted (LTM1); pass a plaintext keys.json", inPath)
	}
	// Validate it is real JSON before encrypting, so a bad input doesn't get
	// sealed (and later fail opaquely at decrypt time).
	var check []keyEntry
	if err := json.Unmarshal(plain, &check); err != nil {
		return 0, false, fmt.Errorf("%s is not valid keys.json: %w", inPath, err)
	}
	enc, err := encryptKeys(plain)
	if err != nil {
		return 0, false, fmt.Errorf("encrypt: %w", err)
	}
	// Write to a temp file in the same dir, then atomically rename to the
	// target so we never leave a half-written file.
	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, enc, 0o600); err != nil {
		return 0, false, fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return 0, false, fmt.Errorf("rename: %w", err)
	}
	// Verify the written ciphertext round-trips before destroying the
	// plaintext source, so we never delete a source whose ciphertext is
	// unrecoverable (e.g. a filesystem error or a bug).
	written, err := os.ReadFile(outPath)
	if err != nil {
		return 0, false, fmt.Errorf("verify read %s: %w", outPath, err)
	}
	rt, err := decryptKeys(written)
	if err != nil {
		return 0, false, fmt.Errorf("verify decrypt %s: %w", outPath, err)
	}
	if string(rt) != string(plain) {
		return 0, false, fmt.Errorf("verify %s: ciphertext does not round-trip to the original plaintext", outPath)
	}
	// Zero the in-memory plaintext buffer (best-effort).
	for i := range plain {
		plain[i] = 0
	}
	if keepSource || inPath == outPath {
		return len(enc), false, nil
	}
	if err := os.Remove(inPath); err != nil {
		return len(enc), false, fmt.Errorf("wrote %s but failed to remove plaintext %s: %w", outPath, inPath, err)
	}
	return len(enc), true, nil
}

// isEncryptedKeys reports whether raw begins with the "LTM1" magic.
func isEncryptedKeys(raw []byte) bool {
	return len(raw) >= len(encMagic) && string(raw[:len(encMagic)]) == string(encMagic)
}

// encryptKeys seals plaintext under AES-256-GCM with the built-in embedKey.
// Output: encMagic || nonce(12B) || ciphertext+tag.
func encryptKeys(plain []byte) ([]byte, error) {
	gcm, err := aesGCM()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plain, encMagic) //AAD = magic, binds header
	out := make([]byte, 0, len(encMagic)+len(nonce)+len(ct))
	out = append(out, encMagic...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// decryptKeys reverses encryptKeys. Returns the plaintext keys.json bytes.
func decryptKeys(raw []byte) ([]byte, error) {
	if !isEncryptedKeys(raw) {
		return nil, fmt.Errorf("not an encrypted keys file (missing %q magic)", encMagic)
	}
	gcm, err := aesGCM()
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(raw) < len(encMagic)+ns {
		return nil, fmt.Errorf("truncated ciphertext")
	}
	nonce := raw[len(encMagic) : len(encMagic)+ns]
	ct := raw[len(encMagic)+ns:]
	plain, err := gcm.Open(nil, nonce, ct, encMagic) //AAD = magic
	if err != nil {
		return nil, fmt.Errorf("decrypt (wrong embedKey or corrupted): %w", err)
	}
	return plain, nil
}

// aesGCM builds an AES-256-GCM cipher from the hex embedKey.
func aesGCM() (cipher.AEAD, error) {
	if embedKey == "" {
		return nil, fmt.Errorf("no embedKey: binary not built with -ldflags \"-X main.embedKey=<hex>\"")
	}
	key, err := hex.DecodeString(embedKey)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("embedKey must be 64 hex chars (32 bytes)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ----- TLS ----------------------------------------------------------------

// resolveTLS returns a *tls.Config when TLS is requested, or nil for plain HTTP.
// -tls-selfsigned generates a fresh self-signed cert and prints its SHA-256
// fingerprint. -cert/-key loads those PEM files. If neither is set, returns nil.
func resolveTLS(tlsSelf bool, certFile, keyFile string) (*tls.Config, error) {
	if tlsSelf {
		// Self-signed takes precedence even if -cert/-key are also set.
		cfg, fingerprint, err := selfSignedTLSConfig()
		if err != nil {
			return nil, fmt.Errorf("generate self-signed cert: %w", err)
		}
		fmt.Fprintf(os.Stderr, "localtask-mcp: TLS self-signed cert SHA-256 fingerprint:\n  %s\nTrust this fingerprint in your client (pinning).\n", fingerprint)
		return cfg, nil
	}
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, fmt.Errorf("TLS with cert files requires both -cert and -key (or use -tls-selfsigned)")
		}
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load cert/key: %w", err)
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
	}
	return nil, nil // plain HTTP
}

// selfSignedTLSConfig generates an ECDSA (P-256) self-signed certificate valid
// for 1 year, suitable for 127.0.0.1 / localhost. Returns the tls.Config and
// the lower-hex SHA-256 fingerprint of the DER certificate.
func selfSignedTLSConfig() (*tls.Config, string, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, "", fmt.Errorf("generate serial: %w", err)
	}
	host, _ := os.Hostname()
	tpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"localtask-mcp"},
			CommonName:   host,
		},
		DNSNames:           []string{"localhost"},
		IPAddresses:        []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:          time.Now().Add(-time.Hour),
		NotAfter:           time.Now().AddDate(1, 0, 0),
		KeyUsage:           x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:        []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, "", fmt.Errorf("create cert: %w", err)
	}
	tlsCert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	}
	return cfg, sha256Hex(der), nil
}

// sha256Hex returns the lower-hex SHA-256 digest of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ----- Multi-key bearer auth ------------------------------------------------

// keyEntry is one accepted bearer key. Label is for logs/identification only.
type keyEntry struct {
	Key     string `json:"key"`
	Label   string `json:"label,omitempty"`
	Revoked bool   `json:"revoked,omitempty"`
}

// keyStore holds a set of accepted bearer tokens. All lookups are constant-time
// so that the number/identity of keys is not leaked via timing.
type keyStore struct {
	// keys is the raw secret for each active (non-revoked) entry. We compare
	// the incoming token against every entry with constant-time compare, so an
	// attacker cannot tell which key matched (or how many keys exist) by timing.
	keys []string
	// labels parallels keys for logging on a match.
	labels []string
}

func newKeyStore(entries []keyEntry) *keyStore {
	ks := &keyStore{}
	for _, e := range entries {
		if e.Revoked || e.Key == "" {
			continue
		}
		ks.keys = append(ks.keys, e.Key)
		label := e.Label
		if label == "" {
			label = e.Key[:min(6, len(e.Key))]
		}
		ks.labels = append(ks.labels, label)
	}
	return ks
}

// empty reports whether the store has any active key.
func (ks *keyStore) empty() bool { return len(ks.keys) == 0 }

// authorize returns the label of the matching key for token (without the
// "Bearer " prefix), or "" if none match. All comparisons are constant-time.
func (ks *keyStore) authorize(token string) string {
	match := 0
	idx := -1
	for i, k := range ks.keys {
		// Constant-time compare; accumulate which index matched without
		// short-circuiting.
		eq := subtle.ConstantTimeCompare([]byte(token), []byte(k))
		match |= eq
		if eq == 1 {
			idx = i
		}
	}
	if match == 1 && idx >= 0 {
		return ks.labels[idx]
	}
	return ""
}

// authMiddleware rejects requests whose Authorization: Bearer <token> header
// does not match any key in the store. stdio is unaffected.
func authMiddleware(next http.Handler, ks *keyStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		const prefix = "Bearer "
		token := ""
		if strings.HasPrefix(authz, prefix) {
			token = authz[len(prefix):]
		}
		label := ks.authorize(token)
		if label == "" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", `Bearer realm="localtask-mcp", error="invalid_token"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"unauthorized","detail":"missing or invalid bearer token"}`)
			return
		}
		// Stamp the matched label onto the request context for logging/audit.
		ctx := context.WithValue(r.Context(), authLabelKey{}, label)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authLabelKey is the context key for the matched key label.
type authLabelKey struct{}

// ----- Small helpers ------------------------------------------------------

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
