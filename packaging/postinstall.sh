#!/bin/sh
# RPM postinstall for localtask-mcp.
# Notes:
#   - Runs WITHOUT the keys.json.enc by default: the admin must place an
#     encrypted keys file (encrypted with the embedKey baked into the binary)
#     at /etc/localtask/keys.json.enc before/after enabling the service.
#   - Does NOT enable the service automatically; the admin enables it once
#     keys are in place.

# Reload systemd if present (so the freshly-installed unit is seen).
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

cat <<'EOF'

localtask-mcp installed.

Next steps:
  1) Create your bearer keys (each grants FULL host control):
       echo -n '[' > /etc/localtask/keys.json
       echo   ' {"key":"<256-bit-hex>","label":"alice"}' >> /etc/localtask/keys.json
       echo   ']' >> /etc/localtask/keys.json
     (use: openssl rand -hex 32  to generate a key)
     Restrict it:
       chmod 640 /etc/localtask/keys.json ; chgrp root /etc/localtask/keys.json

  2) Encrypt it with the package binary (embedKey is baked in):
       /usr/local/bin/localtask-mcp -encrypt-keys /etc/localtask/keys.json /etc/localtask/keys.json.enc
     (the plaintext source is deleted after the ciphertext is verified)

  3) (Optional) Edit the listen address/port/TLS in:
       /usr/lib/systemd/system/localtask-mcp.service
     Defaults: bind 0.0.0.0:8011, plain HTTP (trusted network only).
     WARNING: bearer keys travel in cleartext over plain HTTP — do NOT expose
     0.0.0.0 on an untrusted network without TLS (-tls-selfsigned or -cert/-key).

  4) Enable + start:
       sudo systemctl daemon-reload
       sudo systemctl enable --now localtask-mcp
       sudo journalctl -u localtask-mcp -f

  5) (RHEL firewall) open the port if you bind off-loopback:
       sudo firewall-cmd --permanent --add-port=8011/tcp && sudo firewall-cmd --reload
       (SELinux: if the service fails to start, check AVC denials with
        `sudo ausearch -m AVC -ts recent`; use audit2allow, not setenforce 0.)

EOF
