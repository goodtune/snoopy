#!/bin/sh
# Post-installation script for Snoopy (runs on install and upgrade).
#
# Neither the user service nor the snoopy-attest system service is enabled
# here: whether Snoopy records someone's screen, and whether a desktop is
# attested to KProxy, is the administrator's or user's decision.

# On upgrade, pick up the new unit file and restart snoopy-attest if (and only
# if) the administrator already has it running. Never fail the transaction.
systemctl daemon-reload >/dev/null 2>&1 || true
systemctl try-restart snoopy-attest.service >/dev/null 2>&1 || true

cat <<'MSG'
Snoopy has been installed. The service is not enabled automatically.
Enable it for your user (run as that user, not root):
  systemctl --user enable --now snoopy.service
Or enable it for every user on this machine:
  sudo systemctl --global enable snoopy.service
It binds 127.0.0.1 by default; see /usr/share/doc/snoopy/snoopy.conf.example
to expose it on the LAN for the mobile app.
Supervised desktops (KProxy): see /usr/share/doc/snoopy/attestation.md to set up snoopy-attest.service.
MSG
exit 0
