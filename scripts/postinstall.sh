#!/bin/sh
# Post-installation script for Snoopy (runs on install and upgrade).

# Enable the user service globally for all users. Never fail the package
# transaction if systemd is unavailable (e.g. in containers or chroots).
systemctl --global enable snoopy.service >/dev/null 2>&1 || true

echo "Snoopy has been installed successfully."
echo "The service has been enabled globally. Start it with:"
echo "  systemctl --user start snoopy.service"
