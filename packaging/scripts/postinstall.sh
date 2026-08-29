#!/bin/sh
# Runs as root from both dpkg (postinst: "configure [old-version]") and rpm (%post: install count,
# 1 = fresh, >=2 = upgrade). Every action is idempotent so re-installs are safe.
set -e

SERVICE=kconmon-ng-agent.service

# Dedicated system account: the unit runs unprivileged and regains CAP_NET_RAW via
# AmbientCapabilities. -r allocates the GID dynamically, which is why the shipped sysctl file
# opens ping_group_range wide instead of pinning this group.
if ! getent group kconmon-ng >/dev/null; then
    groupadd -r kconmon-ng
fi
if ! getent passwd kconmon-ng >/dev/null; then
    useradd -r -g kconmon-ng -d /nonexistent -s /sbin/nologin \
        -c "kconmon-ng agent" kconmon-ng
fi

# Open the datagram-ICMP range now rather than at next boot; tolerate read-only /proc/sys
# (containers, chroots).
if command -v sysctl >/dev/null 2>&1; then
    sysctl -p /usr/lib/sysctl.d/50-kconmon-ng.conf >/dev/null 2>&1 || :
fi

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || :
    case "${1:-}" in
        configure)
            if [ -z "${2:-}" ]; then
                # Fresh dpkg install: preset, so distro policy decides enablement; never auto-start —
                # the shipped config still points at gateway.example.com.
                systemctl preset "$SERVICE" || :
            else
                # dpkg upgrade: pick up the new binary if the unit is running.
                systemctl try-restart "$SERVICE" || :
            fi
            ;;
        1)
            # Fresh rpm install.
            systemctl preset "$SERVICE" || :
            ;;
        *)
            # rpm upgrade (count >= 2).
            systemctl try-restart "$SERVICE" || :
            ;;
    esac
fi
