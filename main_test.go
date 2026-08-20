// Integration test: launches localtask-mcp over stdio and calls every tool.
// Run: go test -v
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func startServer(t *testing.T) (*mcp.ClientSession, context.Context, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	bin := os.Getenv("SERVER_BIN")
	if bin == "" {
		bin = "./localtask-mcp"
	}
	cmd := exec.Command(bin)
	t.Logf("starting %s", cmd.Path)
	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	return session, ctx, func() {
		_ = session.Close()
		cancel()
	}
}

func call(t *testing.T, ctx context.Context, s *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if res.IsError {
		t.Logf("tool %s returned IsError (may be expected): %s", name, sb.String())
	}
	return sb.String()
}

func TestTools(t *testing.T) {
	s, ctx, done := startServer(t)
	defer done()

	// List tools and ensure the expected set is present.
	tools, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := []string{"exec", "read", "write", "write_bytes", "read_bytes", "list", "info", "ps", "df", "mem", "k8s"}
	got := map[string]bool{}
	for _, tl := range tools.Tools {
		got[tl.Name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing tool %q", w)
		}
	}

	if out := call(t, ctx, s, "info", nil); !strings.Contains(out, "hostname") {
		t.Errorf("info unexpected: %q", out)
	}
	if out := call(t, ctx, s, "df", nil); !strings.Contains(out, "Filesystem") && !strings.Contains(out, "filesystem") {
		t.Errorf("df unexpected: %q", out)
	}
	if out := call(t, ctx, s, "mem", nil); !strings.Contains(out, "total") && !strings.Contains(out, "Mem") {
		t.Errorf("mem unexpected: %q", out)
	}

	// Write + read roundtrip.
	dir := t.TempDir()
	tmp := dir + "/hello.txt"
	if out := call(t, ctx, s, "write", map[string]any{"path": tmp, "content": "héllo wörld"}); !strings.Contains(out, "wrote") {
		t.Fatalf("write: %q", out)
	}
	if out := call(t, ctx, s, "read", map[string]any{"path": tmp}); out != "héllo wörld" {
		t.Errorf("read roundtrip: %q", out)
	}

	// Binary write + read_bytes roundtrip. read_bytes returns base64 over the
	// text channel, so it must survive bytes that read (raw text) cannot, such
	// as NUL and invalid-UTF-8 bytes present in `raw` below.
	bin := t.TempDir() + "/bin.dat"
	raw := []byte{0, 1, 2, 255, 0, 128}
	if out := call(t, ctx, s, "write_bytes", map[string]any{"path": bin, "contentHex": "000102ff0080"}); !strings.Contains(out, "wrote") {
		t.Fatalf("write_bytes: %q", out)
	}
	b64rb := call(t, ctx, s, "read_bytes", map[string]any{"path": bin})
	decr, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64rb))
	if err != nil {
		t.Fatalf("read_bytes decode: %v (raw %q)", err, b64rb)
	}
	if string(decr) != string(raw) {
		t.Errorf("read_bytes roundtrip mismatch: got %v want %v", decr, raw)
	}

	// exec.
	if out := call(t, ctx, s, "exec", map[string]any{"command": "echo mcp-ok", "timeout": 5}); !strings.Contains(out, "mcp-ok") {
		t.Errorf("exec: %q", out)
	}

	// list on the temp dir that holds hello.txt.
	if out := call(t, ctx, s, "list", map[string]any{"path": dir}); !strings.Contains(out, "hello.txt") {
		t.Errorf("list: %q", out)
	}

	// ps name validation rejects shell metacharacters.
	if out := call(t, ctx, s, "ps", map[string]any{"name": "foo;rm -rf /"}); !strings.Contains(out, "invalid") {
		t.Errorf("ps validation should reject metachar: %q", out)
	}
}

// httpTestServer launches the binary with the given extra args, waits for it to
// come up, and returns the base URL plus a client (insecure TLS if https).
// The caller need not kill the process (t.Cleanup handles it).
func httpTestServer(t *testing.T, args ...string) (base string, client *http.Client) {
	t.Helper()
	bin := os.Getenv("SERVER_BIN")
	if bin == "" {
		bin = "./localtask-mcp"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	full := append([]string{"-http", addr}, args...)
	cmd := exec.Command(bin, full...)
	out, err := os.CreateTemp(t.TempDir(), "server-*.log")
	if err != nil {
		t.Fatalf("temp log: %v", err)
	}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	scheme := "http"
	for _, a := range args {
		if a == "-tls-selfsigned" || strings.HasPrefix(a, "-tls") {
			scheme = "https"
		}
	}
	base = scheme + "://" + addr + "/"
	tr := &http.Transport{}
	if scheme == "https" {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // self-signed in tests
	}
	client = &http.Client{Timeout: 5 * time.Second, Transport: tr}

	// Wait for the listener (poll until a request succeeds).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", base, nil)
		req.Header.Set("Authorization", "Bearer ") // any header to avoid 401-loop noise
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return base, client
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server did not come up on %s (log:\n%s)", base, readFile(t, out.Name()))
	return base, client
}

func readFile(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(read %s: %v)", path, err)
	}
	return string(b)
}

// httpGet performs a GET with the given Authorization header and returns the
// status code + body.
func httpGet(t *testing.T, client *http.Client, base, auth string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", base, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestMultiKeyAuth covers multi-key loading, per-key auth, revocation, and the
// legacy single-token fallback.
func TestMultiKeyAuth(t *testing.T) {
	// Two valid keys with labels.
	base, client := httpTestServer(t, "-tokens", "key-one:alice,key-two:bob")

	if code, _ := httpGet(t, client, base, ""); code != http.StatusUnauthorized {
		t.Errorf("missing token: got %d, want 401", code)
	}
	if code, _ := httpGet(t, client, base, "Bearer wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", code)
	}
	if code, _ := httpGet(t, client, base, "Bearer key-one"); code != http.StatusOK {
		t.Errorf("key-one: got %d, want 200", code)
	}
	if code, _ := httpGet(t, client, base, "Bearer key-two"); code != http.StatusOK {
		t.Errorf("key-two: got %d, want 200", code)
	}
}

// TestRevokedKey covers that a revoked key in keys.json is rejected.
func TestRevokedKey(t *testing.T) {
	keysFile := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(keysFile, []byte(`[
		{"key":"good","label":"valid"},
		{"key":"bad","label":"gone","revoked":true}
	]`), 0o644); err != nil {
		t.Fatalf("write keys.json: %v", err)
	}
	base, client := httpTestServer(t, "-keys", keysFile)

	if code, _ := httpGet(t, client, base, "Bearer good"); code != http.StatusOK {
		t.Errorf("good key: got %d, want 200", code)
	}
	if code, _ := httpGet(t, client, base, "Bearer bad"); code != http.StatusUnauthorized {
		t.Errorf("revoked key: got %d, want 401", code)
	}
}

// TestLegacyToken covers backward compatibility with a bare MCP_TOKEN env var.
func TestLegacyToken(t *testing.T) {
	bin := os.Getenv("SERVER_BIN")
	if bin == "" {
		bin = "./localtask-mcp"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cmd := exec.Command(bin, "-http", addr)
	cmd.Env = append(os.Environ(), "MCP_TOKEN=legacy-only")
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	base := "http://" + addr + "/"
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(base); err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code, _ := httpGet(t, client, base, "Bearer legacy-only"); code != http.StatusOK {
		t.Errorf("legacy token: got %d, want 200", code)
	}
}

// TestTLSSelfSigned verifies HTTPS with an auto-generated self-signed cert
// works end-to-end (incl. an MCP initialize over TLS).
func TestTLSSelfSigned(t *testing.T) {
	base, client := httpTestServer(t, "-tls-selfsigned", "-tokens", "tlstoken")

	if code, _ := httpGet(t, client, base, "Bearer tlstoken"); code != http.StatusOK {
		t.Errorf("https correct token: got %d, want 200", code)
	}
	if code, _ := httpGet(t, client, base, "Bearer wrong"); code != http.StatusUnauthorized {
		t.Errorf("https wrong token: got %d, want 401", code)
	}

	// MCP initialize over TLS.
	req, _ := http.NewRequest("POST", base+"mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
	req.Header.Set("Authorization", "Bearer tlstoken")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("mcp over tls: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mcp initialize over tls: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"serverInfo":{"name":"localtask"`) {
		t.Errorf("mcp over tls: unexpected body %q", body)
	}
}

// TestEncryptKeysRoundtrip exercises the AES-256-GCM encrypt/decrypt helpers
// directly (no subprocess). embedKey is a package-level var, so the test sets it.
func TestEncryptKeysRoundtrip(t *testing.T) {
	prev := embedKey
	t.Cleanup(func() { embedKey = prev }) // restore for other tests
	embedKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

	plain := []byte(`[{"key":"abc","label":"x"},{"key":"def","label":"y","revoked":true}]`)
	enc, err := encryptKeys(plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !isEncryptedKeys(enc) {
		t.Errorf("isEncryptedKeys: ciphertext not recognized")
	}
	if string(enc[:4]) != "LTM1" {
		t.Errorf("magic: got %q, want LTM1", enc[:4])
	}

	got, err := decryptKeys(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(got) != string(plain) {
		t.Errorf("roundtrip mismatch:\n got %q\nwant %q", got, plain)
	}
}

// TestEncryptedKeysNoEmbedKey verifies that decrypting with no embedKey fails.
func TestEncryptedKeysNoEmbedKey(t *testing.T) {
	prev := embedKey
	t.Cleanup(func() { embedKey = prev })
	embedKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	enc, _ := encryptKeys([]byte(`[{"key":"x"}]`))

	embedKey = "" // simulate a binary built without -ldflags
	if _, err := decryptKeys(enc); err == nil {
		t.Errorf("decrypt with no embedKey: want error, got nil")
	}
}

// TestEncryptedKeysWrongEmbedKey verifies a different embedKey fails (GCM tag).
func TestEncryptedKeysWrongEmbedKey(t *testing.T) {
	prev := embedKey
	t.Cleanup(func() { embedKey = prev })
	embedKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	enc, _ := encryptKeys([]byte(`[{"key":"x"}]`))

	embedKey = "ff112233445566778899aabbccddeeff00112233445566778899aabbccddeeff" // wrong
	if _, err := decryptKeys(enc); err == nil {
		t.Errorf("decrypt with wrong embedKey: want error, got nil")
	}
}

// TestEncryptKeysFilesDelete covers encryptKeysFiles: by default it removes the
// plaintext source after verifying the ciphertext round-trips; -keep preserves it.
func TestEncryptKeysFilesDelete(t *testing.T) {
	prev := embedKey
	t.Cleanup(func() { embedKey = prev })
	embedKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

	plain := `[{"key":"abc","label":"x"}]`

	t.Run("deletes_source", func(t *testing.T) {
		dir := t.TempDir()
		inPath := filepath.Join(dir, "keys.json")
		outPath := filepath.Join(dir, "keys.json.enc")
		if err := os.WriteFile(inPath, []byte(plain), 0o600); err != nil {
			t.Fatalf("write in: %v", err)
		}
		n, deleted, err := encryptKeysFiles(inPath, outPath, false)
		if err != nil {
			t.Fatalf("encryptKeysFiles: %v", err)
		}
		if !deleted {
			t.Errorf("deleted=false, want true")
		}
		if _, err := os.Stat(inPath); !os.IsNotExist(err) {
			t.Errorf("plaintext source still exists after encrypt (stat err=%v)", err)
		}
		if n == 0 {
			t.Errorf("returned ciphertext length 0")
		}
		// The written ciphertext must still decrypt back to the original.
		enc, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("read out: %v", err)
		}
		got, err := decryptKeys(enc)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if string(got) != plain {
			t.Errorf("roundtrip: got %q want %q", got, plain)
		}
	})

	t.Run("keep_preserves_source", func(t *testing.T) {
		dir := t.TempDir()
		inPath := filepath.Join(dir, "keys.json")
		outPath := filepath.Join(dir, "keys.json.enc")
		if err := os.WriteFile(inPath, []byte(plain), 0o600); err != nil {
			t.Fatalf("write in: %v", err)
		}
		_, deleted, err := encryptKeysFiles(inPath, outPath, true)
		if err != nil {
			t.Fatalf("encryptKeysFiles: %v", err)
		}
		if deleted {
			t.Errorf("deleted=true with -keep, want false")
		}
		if _, err := os.Stat(inPath); err != nil {
			t.Errorf("plaintext source removed despite -keep: %v", err)
		}
	})

	t.Run("in_place_keeps_path_no_delete", func(t *testing.T) {
		// in == out: the path is overwritten in place, not removed.
		dir := t.TempDir()
		inPath := filepath.Join(dir, "keys.json")
		if err := os.WriteFile(inPath, []byte(plain), 0o600); err != nil {
			t.Fatalf("write in: %v", err)
		}
		_, deleted, err := encryptKeysFiles(inPath, inPath, false)
		if err != nil {
			t.Fatalf("encryptKeysFiles: %v", err)
		}
		if deleted {
			t.Errorf("deleted=true for in-place, want false")
		}
		if _, err := os.Stat(inPath); err != nil {
			t.Errorf("in-place path missing after encrypt: %v", err)
		}
		// Should now hold ciphertext, not plaintext.
		raw, err := os.ReadFile(inPath)
		if err != nil {
			t.Fatalf("read in: %v", err)
		}
		if !isEncryptedKeys(raw) {
			t.Errorf("in-place file is not ciphertext")
		}
	})
}

// TestHTTPFileTransfer covers the streaming /download and /upload endpoints:
// whole-file upload roundtrip (binary-safe + sha256), chunked resume upload
// (206/206→200, HEAD X-Received), download Range resume, checksum mismatch,
// and bearer auth. All via httpTestServer (HTTP form, -tokens auth).
func TestHTTPFileTransfer(t *testing.T) {
	base, client := httpTestServer(t, "-tokens", "k:alice")
	const auth = "Bearer k"

	// A payload with bytes that the read (text) path corrupts: NUL and invalid
	// UTF-8 (0xff 0xfe), plus a trailing newline.
	payload := []byte{0x48, 0x69, 0xff, 0xfe, 0xe4, 0xbd, 0xad, 0x00, 0x0a}
	wantSHA := sha256.Sum256(payload)
	wantSHAHex := fmt.Sprintf("%x", wantSHA[:])

	doReq := func(method, url string, body io.Reader, hdr map[string]string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, url, body)
		if err != nil {
			t.Fatalf("newreq %s %s: %v", method, url, err)
		}
		req.Header.Set("Authorization", auth)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do %s %s: %v", method, url, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	t.Run("auth_required", func(t *testing.T) {
		// No bearer → 401.
		req, _ := http.NewRequest("GET", base+"download?path=/etc/hosts", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no-auth download: got %d, want 401", resp.StatusCode)
		}
	})

	t.Run("upload_whole_then_download", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "whole.bin")
		// Single-shot upload: Content-Range bytes 0-(n-1)/n.
		cr := fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload))
		resp, body := doReq("PUT", base+"upload?path="+urlPath(dst), bytes.NewReader(payload), map[string]string{
			"Content-Range":     cr,
			"X-Expected-Sha256": wantSHAHex,
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("upload: got %d %s, want 200", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), wantSHAHex) {
			t.Errorf("upload body missing sha256: %s", body)
		}
		// After a completed upload the temp file is renamed into place, so
		// HEAD /download (not /upload) reports the final size + sha256.
		resp, _ = doReq("HEAD", base+"download?path="+urlPath(dst), nil, nil)
		if got := resp.Header.Get("X-File-Size"); got != strconv.Itoa(len(payload)) {
			t.Errorf("HEAD download X-File-Size=%q, want %d", got, len(payload))
		}
		if got := resp.Header.Get("X-File-Sha256"); got != wantSHAHex {
			t.Errorf("HEAD download sha256=%q, want %s", got, wantSHAHex)
		}
		// GET /download returns the exact bytes.
		resp, body = doReq("GET", base+"download?path="+urlPath(dst), nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("download: got %d, want 200", resp.StatusCode)
		}
		if !bytes.Equal(body, payload) {
			t.Errorf("download roundtrip mismatch: got %x, want %x", body, payload)
		}
	})

	t.Run("upload_chunked_resume", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "chunked.bin")
		// First half → 206.
		mid := len(payload) / 2
		resp, body := doReq("PUT", base+"upload?path="+urlPath(dst), bytes.NewReader(payload[:mid]), map[string]string{
			"Content-Range": fmt.Sprintf("bytes 0-%d/%d", mid-1, len(payload)),
		})
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("chunk1: got %d %s, want 206", resp.StatusCode, body)
		}
		// HEAD shows received so far.
		resp, _ = doReq("HEAD", base+"upload?path="+urlPath(dst), nil, nil)
		if got := resp.Header.Get("X-Received"); got != strconv.Itoa(mid) {
			t.Errorf("HEAD after chunk1 X-Received=%q, want %d", got, mid)
		}
		// Second half (final) → 200 + sha256.
		resp, body = doReq("PUT", base+"upload?path="+urlPath(dst), bytes.NewReader(payload[mid:]), map[string]string{
			"Content-Range":      fmt.Sprintf("bytes %d-%d/%d", mid, len(payload)-1, len(payload)),
			"X-Expected-Sha256":  wantSHAHex,
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("chunk2: got %d %s, want 200", resp.StatusCode, body)
		}
		// Verify assembled file on disk matches.
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read dst: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("assembled mismatch: got %x, want %x", got, payload)
		}
	})

	t.Run("download_range_resume", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "range.bin")
		// Seed via upload.
		_, _ = doReq("PUT", base+"upload?path="+urlPath(dst), bytes.NewReader(payload), map[string]string{
			"Content-Range": fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload)),
		})
		// Range: bytes=2- → suffix from offset 2.
		resp, body := doReq("GET", base+"download?path="+urlPath(dst), nil, map[string]string{
			"Range": "bytes=2-",
		})
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("range: got %d, want 206", resp.StatusCode)
		}
		if !bytes.Equal(body, payload[2:]) {
			t.Errorf("range mismatch: got %x, want %x", body, payload[2:])
		}
	})

	t.Run("upload_checksum_mismatch", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "bad.bin")
		resp, body := doReq("PUT", base+"upload?path="+urlPath(dst), bytes.NewReader(payload), map[string]string{
			"Content-Range":      fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload)),
			"X-Expected-Sha256":  "deadbeef" + strings.Repeat("0", 56), // wrong
		})
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("mismatch: got %d %s, want 500", resp.StatusCode, body)
		}
		// Temp file should be cleaned up: target must not exist.
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("target should not exist after mismatch: %v", err)
		}
	})
}

// urlPath percent-encodes a path for use in a query string.
func urlPath(p string) string {
	var b strings.Builder
	for _, c := range []byte(p) {
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '/' || c == '.' || c == '_' || c == '-':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
