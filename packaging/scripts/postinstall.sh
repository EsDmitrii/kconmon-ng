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
# (containers, chroots). Leave it alone wherever boot would: the admin sets the key in any file (as
# the shipped file invites), or shadows ours by name, a /dev/null mask included. systemd-sysctl
# resolves the name through /etc, /run and /usr/lib itself; `sysctl -p FILE` reads only FILE.
admin_sets_ping_range() {
    for f in /etc/sysctl.conf /etc/sysctl.d/*.conf /run/sysctl.d/*.conf; do
        [ -f "$f" ] && grep -qs '^[[:space:]]*-\{0,1\}net[./]ipv4[./]ping_group_range' "$f" && return 0
    done
    return 1
}
vendor_file_shadowed() {
    for f in /etc/sysctl.d/50-kconmon-ng.conf /run/sysctl.d/50-kconmon-ng.conf; do
        { [ -e "$f" ] || [ -L "$f" ]; } && return 0
    done
    return 1
}
if ! admin_sets_ping_range; then
    if [ -x /usr/lib/systemd/systemd-sysctl ]; then
        /usr/lib/systemd/systemd-sysctl 50-kconmon-ng.conf >/dev/null 2>&1 || :
    elif [ -x /lib/systemd/systemd-sysctl ]; then
        /lib/systemd/systemd-sysctl 50-kconmon-ng.conf >/dev/null 2>&1 || :
    elif command -v sysctl >/dev/null 2>&1 && ! vendor_file_shadowed; then
        sysctl -p /usr/lib/sysctl.d/50-kconmon-ng.conf >/dev/null 2>&1 || :
    fi
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
