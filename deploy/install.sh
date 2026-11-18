#!/bin/sh
set -eu

# Usage: sudo sh deploy/install.sh bin/watchdog-linux-amd64
# The binary is built elsewhere; nothing is downloaded or built as root.
[ "$(id -u)" -eq 0 ] || { echo 'Run as root.' >&2; exit 1; }
[ "$(uname -s)" = Linux ] || { echo 'Linux is required.' >&2; exit 1; }
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
binary=${1:?Usage: install.sh PATH_TO_LINUX_BINARY}
[ -f "$binary" ] || { echo 'Binary not found.' >&2; exit 1; }
if ! id watchdog >/dev/null 2>&1; then
    useradd --system --user-group --home-dir /var/lib/watchdog --shell /usr/sbin/nologin watchdog
fi
install -d -o root -g root -m 0755 /etc/watchdog
install -d -o watchdog -g watchdog -m 0700 /var/lib/watchdog
if [ ! -f /etc/watchdog/watchdog.env ]; then
    install -o root -g root -m 0600 "$script_dir/watchdog.env.example" /etc/watchdog/watchdog.env
fi
# Install by rename so replacing a running executable is safe.
install -o root -g root -m 0755 "$binary" /usr/local/bin/watchdog.new
mv -f /usr/local/bin/watchdog.new /usr/local/bin/watchdog
install -o root -g root -m 0644 "$script_dir/watchdog.service" /etc/systemd/system/watchdog.service
systemctl daemon-reload
echo 'Installed. Existing settings, database and environment file were preserved.'
echo 'Configure /etc/watchdog/watchdog.env, then: systemctl enable --now watchdog'
echo 'For an update of an active service: systemctl restart watchdog'
