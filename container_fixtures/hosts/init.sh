#!/bin/bash
#
# Shared init script for every host-simulation fixture container.
#
# Required environment variable:
#   TRENTO_AGENT_ID  RFC4122 UUID used for both alloy generation and the agent
#                    --force-agent-id flag. Pass via `docker run -e ...`.
#
# Optional behavior:
#   /etc/soappatrol.toml  if present after the overlay copy, soappatrol is
#                         started against /tmp/.sapstream50013 (HANA hosts have
#                         it; the majority maker does not).
#
# In k8s the per-host fixture filesystem is staged at /fixture by an init
# container (the per-host files image) which copies its contents into a
# shared emptyDir mounted here read-only. We can't bind the overlay directly
# over /etc and /usr because they're already populated by the agent image
# (zypper packages, CA certs, etc.); so we overlay at startup instead.

set -m

: "${TRENTO_AGENT_ID:?TRENTO_AGENT_ID environment variable is required}"

# Apply the per-host fixture overlay onto the agent's own root.
# Source is a read-only emptyDir staged by the "files" init container.
if [ -d /fixture/etc ]; then
    cp -a /fixture/etc/. /etc/
fi
if [ -d /fixture/usr ]; then
    cp -a /fixture/usr/. /usr/
fi

# Update CA certificates
update-ca-certificates

# Start the SAP NetWeaver SOAP control mock if this host has a SAP profile.
if [ -f /etc/soappatrol.toml ]; then
    /usr/bin/soappatrol /tmp/.sapstream50013 /etc/soappatrol.toml &
fi

# Generate alloy configuration from the trento agent and install it
mkdir -p /etc/alloy
/usr/bin/trento-agent generate alloy \
    --force-agent-id="$TRENTO_AGENT_ID" \
    > /etc/alloy/config.alloy

# Start Alloy
/usr/bin/alloy run /etc/alloy/config.alloy --storage.path=/var/lib/alloy/data &

# Start the trento agent
/usr/bin/trento-agent start --force-agent-id="$TRENTO_AGENT_ID" &

# Wait for any process to exit
wait -n

# Exit with status of process that exited first
exit $?
