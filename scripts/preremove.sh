#!/bin/sh
# Pre-removal script for Snoopy.
#
# On upgrade the old package's pre-removal script also runs, so only disable
# the service when the package is really being removed:
#   RPM %preun: $1 is the number of instances left after the operation
#               (0 = erase, >=1 = upgrade).
#   deb prerm:  $1 is "remove" or "deconfigure" on removal, "upgrade" or
#               "failed-upgrade" on upgrade.
case "$1" in
    0|remove|deconfigure)
        # Undo a global enable by the administrator, if any (harmless if it
        # was never enabled). Never fail the package transaction if systemd
        # is unavailable.
        systemctl --global disable snoopy.service >/dev/null 2>&1 || true
        echo "Snoopy service has been disabled for all users."
        echo "If the service is running, stop it (and drop a per-user enable) with:"
        echo "  systemctl --user disable --now snoopy.service"
        ;;
    *)
        # Upgrade: keep the service enabled.
        ;;
esac
exit 0
