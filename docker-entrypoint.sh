#!/bin/sh
# Starts as root only long enough to hand the old volume to the service's user:
# Railway mounts volumes owned by root, and the first boot reads the old database
# from it to split it into shards. Once the volume is detached, none of this runs
# and the entrypoint can be dropped for USER in the Dockerfile.
set -e

if [ -n "$RAILWAY_VOLUME_MOUNT_PATH" ]; then
    chown -R steki:steki "$RAILWAY_VOLUME_MOUNT_PATH"
fi

exec su-exec steki ./web
