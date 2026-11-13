#!/bin/sh
# Post-installation script for Snoopy (runs on install and upgrade).
#
# Neither the user service nor the snoopy-attest and snoopy-hub system
# services are enabled here: whether Snoopy records someone's screen, whether
# a desktop is attested to KProxy and whether screens are aggregated on a hub
# is the administrator's or user's decision.

# On upgrade, pick up the new unit files and restart snoopy-attest and
# snoopy-hub if (and only if) the administrator already has them running.
# Never fail the transaction.
systemctl daemon-reload >/dev/null 2>&1 || true
systemctl try-restart snoopy-attest.service >/dev/null 2>&1 || true
systemctl try-restart snoopy-hub.service >/dev/null 2>&1 || true

cat <<'MSG'
Snoopy has been installed. The service is not enabled automatically.
Enable it for your user (run as that user, not root):
  systemctl --user enable --now snoopy.service
Or enable it for every user on this machine:
  sudo systemctl --global enable snoopy.service
It binds 127.0.0.1 by default; see /usr/share/doc/snoopy/snoopy.conf.example
to expose it on the LAN for the mobile app.
Supervised desktops (KProxy): see /usr/share/doc/snoopy/attestation.md to set up snoopy-attest.service.
Web index of every screen on the LAN: see /usr/share/doc/snoopy/hub.md to set up snoopy-hub.service.
MSG
exit 0
