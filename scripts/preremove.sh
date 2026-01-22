#!/bin/bash
# Pre-removal script for Snoopy

# Disable the service globally for all users
systemctl --global disable snoopy.service

echo "Snoopy service has been disabled globally."
echo "If the service is running, stop it with:"
echo "  systemctl --user stop snoopy.service"
