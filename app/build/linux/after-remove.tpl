#!/bin/bash

# Delete the link to the binary
if type update-alternatives >/dev/null 2>&1; then
    update-alternatives --remove '${executable}' '/usr/bin/${executable}'
else
    rm -f '/usr/bin/${executable}'
fi

APPARMOR_PROFILE_DEST='/etc/apparmor.d/${executable}'
if [ -f "$APPARMOR_PROFILE_DEST" ]; then
    if hash apparmor_parser 2>/dev/null; then
        apparmor_parser --remove "$APPARMOR_PROFILE_DEST" >/dev/null 2>&1 || true
    fi
    rm -f "$APPARMOR_PROFILE_DEST"
fi
