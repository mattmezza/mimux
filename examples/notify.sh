#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# New-mail desktop notification — `mimux mail webhooks listen` exec mode.
#
# Run:  mimux mail webhooks listen -events message.received -execute ./notify.sh
#
# The listener runs this once per event: the JSON payload arrives on stdin,
# the event type and delivery id in $MIMUX_EVENT and $MIMUX_DELIVERY_ID.
# Needs jq and notify-send (libnotify). Nothing is queued while the listener
# is not connected — this is a live notifier, not a log.

payload=$(cat)
from=$(printf '%s' "$payload" | jq -r '.data.from.name // .data.from.address')
subject=$(printf '%s' "$payload" | jq -r '.data.subject // "(no subject)"')
notify-send "New mail from ${from:-someone}" "$subject"
