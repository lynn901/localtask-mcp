#!/bin/sh
# RPM preremove for localtask-mcp. Stops the service before removal.
if [ "$1" = "0" ]; then
    # Removal (not upgrade). Stop + disable.
    if command -v systemctl >/dev/null 2>&1; then
        systemctl --no-reload disable --now localtask-mcp >/dev/null 2>&1 || true
    fi
fi
