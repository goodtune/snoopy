#!/bin/sh
# Post-installation script for Snoopy (runs on install and upgrade).
#
# The user service is deliberately not enabled here: whether Snoopy records
# someone's screen is the administrator's or user's decision.

cat <<'MSG'
Snoopy has been installed. The service is not enabled automatically.
Enable it for your user (run as that user, not root):
  systemctl --user enable --now snoopy.service
Or enable it for every user on this machine:
  sudo systemctl --global enable snoopy.service
It binds 127.0.0.1 by default; see /usr/share/doc/snoopy/snoopy.conf.example
to expose it on the LAN for the mobile app.
MSG
exit 0
