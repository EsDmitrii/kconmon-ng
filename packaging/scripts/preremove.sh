#!/bin/sh
# Runs as root from both dpkg (prerm: "remove"/"upgrade") and rpm (%preun: 0 = erase, 1 = upgrade).
# Only a real removal stops the unit — stopping on upgrade would leave the host dark between
# packages. The kconmon-ng account is left behind on purpose (files may still be owned by it).
set -e

SERVICE=kconmon-ng-agent.service

case "${1:-}" in
    remove|0)
        if [ -d /run/systemd/system ]; then
            systemctl --no-reload disable "$SERVICE" || :
            systemctl stop "$SERVICE" || :
        fi
        ;;
esac
