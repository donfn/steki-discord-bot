#!/bin/sh
# Starts as root only long enough to hand the data directory to the bot's user:
# Railway mounts volumes owned by root, and SQLite needs to create its file and
# journal there. The bot itself then runs unprivileged.
set -e

dir=""
if [ -n "$DATABASE_PATH" ]; then
    dir=$(dirname "$DATABASE_PATH")
elif [ -n "$RAILWAY_VOLUME_MOUNT_PATH" ]; then
    dir="$RAILWAY_VOLUME_MOUNT_PATH"
fi
if [ -n "$dir" ]; then
    mkdir -p "$dir"
    chown -R steki:steki "$dir"
fi

exec su-exec steki ./steki-bot
