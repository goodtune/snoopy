#!/bin/bash
# Post-installation script for Snoopy

# Enable the service globally for all users
systemctl --global enable snoopy.service

echo "Snoopy has been installed successfully."
echo "The service has been enabled globally. Start it with:"
echo "  systemctl --user start snoopy.service"
