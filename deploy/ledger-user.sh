#!/bin/sh
# Manage login users with the service's configuration. Run as root:
#   ledger-user add alice
#   ledger-user passwd alice
#   ledger-user disable alice | enable alice | list
set -eu
ENV_FILE="${ENV_FILE:-/etc/ledger/ledger.env}"
[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }

# Load KEY=value lines like systemd's EnvironmentFile does, without letting
# the shell interpret them (URLs contain characters such as '&').
while IFS= read -r line || [ -n "$line" ]; do
	case "$line" in
	'' | '#'*) continue ;;
	*=*)
		key=${line%%=*}
		case "$key" in
		'' | [!A-Za-z_]* | *[!A-Za-z0-9_]*) echo "ignoring invalid line in $ENV_FILE: $key" >&2 ;;
		*) export "$key=${line#*=}" ;;
		esac
		;;
	esac
done <"$ENV_FILE"

exec runuser -u ledger -- /usr/local/bin/ledger user "$@"
