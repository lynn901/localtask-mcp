#!/usr/bin/env python3
"""Simple HTTP API server - zero dependencies, Python 3.6+ stdlib only."""

import json
import subprocess
import os
import platform
from http.server import HTTPServer, ThreadingHTTPServer, BaseHTTPRequestHandler
from datetime import datetime
from urllib.parse import urlparse, parse_qs

API_DOCS = {
    "exec": {"desc": "Run a shell command on the host", "params": {"command": "str (required)", "timeout": "int (default 30)"}},
    "read": {"desc": "Read a text file (utf-8)", "params": {"path": "str (required)"}},
    "write": {"desc": "Write text content to a file (utf-8, overwrites)", "params": {"path": "str (required)", "content": "str (required)"}},
    "list": {"desc": "List directory contents", "params": {"path": "str (default '.')"}},
    "info": {"desc": "Get host system info", "params": {}},
    "ps": {"desc": "List top processes by memory", "params": {"name": "str (optional, filter by process name)"}},
    "df": {"desc": "Disk usage", "params": {}},
    "mem": {"desc": "Memory usage", "params": {}},
    "k8s": {"desc": "Run a kubectl command", "params": {"command": "str (default 'kubectl get nodes')", "timeout": "int (default 30)"}},
}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _add_cors(self):
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, HEAD, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type, Authorization")

    def do_POST(self):
        content_length = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(content_length).decode('utf-8')
        try:
            data = json.loads(body)
        except json.JSONDecodeError:
            self._json(400, {"error": "Invalid JSON"})
            return

        action = data.get("action", "")
        params = data.get("params", {})

        handlers = {
            "exec": self.handle_exec,
            "read": self.handle_read,
            "write": self.handle_write,
            "list": self.handle_list,
            "info": self.handle_info,
            "ps": self.handle_ps,
            "df": self.handle_df,
            "mem": self.handle_mem,
            "k8s": self.handle_k8s,
        }

        handler = handlers.get(action)
        if handler:
            result = handler(params)
        else:
            result = {"error": f"Unknown action: {action}", "available": list(handlers.keys())}
        self._json(200, result)

    def do_PUT(self):
        parsed = urlparse(self.path)
        qs = parse_qs(parsed.query)
        dest = qs.get('path', [''])[0]

        if not dest:
            self._json(400, {"error": "Missing 'path' query parameter", "usage": "PUT /upload?path=/abs/dest/path"})
            return

        content_length = self.headers.get('Content-Length')
        try:
            parent = os.path.dirname(dest)
            if parent:
                os.makedirs(parent, exist_ok=True)
            written = 0
            with open(dest, 'wb') as f:
                if content_length is not None:
                    remaining = int(content_length)
                    while remaining > 0:
                        chunk = self.rfile.read(min(remaining, 65536))
                        if not chunk:
                            break
                        f.write(chunk)
                        remaining -= len(chunk)
                        written += len(chunk)
                else:
                    while True:
                        chunk = self.rfile.read(65536)
                        if not chunk:
                            break
                        f.write(chunk)
                        written += len(chunk)
            self._json(200, {"status": "ok", "path": dest, "size": written})
        except Exception as e:
            self._json(500, {"error": str(e)})

    def do_GET(self):
        self._json(200, {
            "status": "ok",
            "actions": list(API_DOCS.keys()),
            "upload": {"method": "PUT", "path": "/upload?path=<dest>", "body": "raw binary"},
            "docs": API_DOCS,
        })

    def do_OPTIONS(self):
        self.send_response(200)
        self._add_cors()
        self.end_headers()

    def _json(self, code, obj):
        self.send_response(code)
        self.send_header('Content-Type', 'application/json')
        self._add_cors()
        body = json.dumps(obj, ensure_ascii=False).encode('utf-8')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def handle_exec(self, params):
        cmd = params.get("command", "")
        if not cmd:
            return {"error": "Command cannot be empty"}
        timeout = params.get("timeout", 30)
        try:
            r = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=timeout)
            return {"stdout": r.stdout, "stderr": r.stderr, "returncode": r.returncode}
        except subprocess.TimeoutExpired:
            return {"error": f"Timeout after {timeout}s"}
        except Exception as e:
            return {"error": str(e)}

    def handle_read(self, params):
        path = params.get("path", "")
        try:
            with open(path, 'rb') as f:
                raw = f.read()
            try:
                return {"content": raw.decode('utf-8')}
            except UnicodeDecodeError:
                return {"error": "File is not valid UTF-8 (may be binary)"}
        except Exception as e:
            return {"error": str(e)}

    def handle_write(self, params):
        path = params.get("path", "")
        content = params.get("content", "")
        try:
            with open(path, 'w', encoding='utf-8') as f:
                f.write(content)
            return {"status": "ok"}
        except Exception as e:
            return {"error": str(e)}

    def handle_list(self, params):
        path = params.get("path", ".")
        try:
            items = []
            for name in os.listdir(path):
                full = os.path.join(path, name)
                st = os.stat(full)
                items.append({"name": name, "type": "dir" if os.path.isdir(full) else "file", "size": st.st_size})
            return {"items": items}
        except Exception as e:
            return {"error": str(e)}

    def handle_info(self, params):
        return {
            "hostname": platform.node(),
            "platform": platform.platform(),
            "python": platform.python_version(),
            "time": datetime.now().isoformat(),
            "user": os.getenv("USER", "unknown"),
            "cwd": os.getcwd(),
        }

    def handle_ps(self, params):
        name = params.get("name", "")
        if name:
            # Sanitize: only allow alphanumeric, dash, underscore, dot
            import re
            if not re.match(r'^[a-zA-Z0-9._-]+$', name):
                return {"error": "Invalid process name (alphanumeric, dash, underscore, dot only)"}
            cmd = f"ps aux | grep -i {name} | grep -v grep"
        else:
            cmd = "ps aux --sort=-%mem | head -20"
        r = subprocess.run(cmd, shell=True, capture_output=True, text=True)
        return {"output": r.stdout or "No matching processes"}

    def handle_df(self, params):
        r = subprocess.run("df -h", shell=True, capture_output=True, text=True)
        return {"output": r.stdout}

    def handle_mem(self, params):
        r = subprocess.run("free -h", shell=True, capture_output=True, text=True)
        return {"output": r.stdout}

    def handle_k8s(self, params):
        cmd = params.get("command", "kubectl get nodes")
        timeout = params.get("timeout", 30)
        try:
            r = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=timeout)
            return {"stdout": r.stdout, "stderr": r.stderr, "returncode": r.returncode}
        except subprocess.TimeoutExpired:
            return {"error": f"Timeout after {timeout}s"}
        except Exception as e:
            return {"error": str(e)}

    def log_message(self, fmt, *args):
        import sys
        sys.stderr.write("%s - - [%s] %s\n" % (self.address_string(), self.log_date_time_string(), fmt % args))


if __name__ == "__main__":
    host = "0.0.0.0"
    port = int(os.environ.get("PORT", 8000))
    # Write PID for remote restart
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), 'server.pid'), 'w') as f:
        f.write(str(os.getpid()))
    server = ThreadingHTTPServer((host, port), Handler)
    print(f"Server running on {host}:{port}")
    server.serve_forever()
