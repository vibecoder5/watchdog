#!/bin/sh
set -eu
umask 077

[ "$(id -u)" -eq 0 ] || { echo 'Run as root.' >&2; exit 1; }
upload_dir=${1:?Upload directory required}
domain=${2:?Domain required}
case "$upload_dir" in /tmp/watchdog-deploy.*) ;; *) echo 'Invalid upload directory.' >&2; exit 1 ;; esac
case "$domain" in ''|*[!a-zA-Z0-9.-]*|-*) echo 'Invalid domain syntax.' >&2; exit 1 ;; esac
[ -f "$upload_dir/watchdog.env" ] && [ -f "$upload_dir/watchdog" ] || { echo 'Missing upload files.' >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo 'Install curl first; it is required for the loopback health check.' >&2; exit 1; }

# Serialize deployments. Database and existing UI settings are never replaced.
exec 9>/run/watchdog-deploy.lock
flock -n 9 || { echo 'Another watchdog deployment is running.' >&2; exit 1; }

if ! command -v caddy >/dev/null 2>&1; then
    # Never install over an existing custom systemd deployment.
    if [ "$(systemctl show caddy.service -p LoadState --value)" != not-found ]; then
        echo 'Caddy service exists but caddy is not in PATH; inspect its custom installation.' >&2
        exit 1
    fi
    command -v apt-get >/dev/null 2>&1 || { echo 'Automatic Caddy installation requires Debian/Ubuntu with apt-get.' >&2; exit 1; }
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends caddy
fi

caddyfile=/etc/caddy/Caddyfile
site=/etc/caddy/watchdog.caddy
marker='# watchdog deployment: managed import'
if ! systemctl show caddy.service -p ExecStart --value | grep -Fq '/etc/caddy/Caddyfile'; then
    echo 'Expected Caddy systemd service using /etc/caddy/Caddyfile; custom Caddy installations need manual integration.' >&2
    exit 1
fi
[ -f "$caddyfile" ] || { echo 'Caddyfile not found.' >&2; exit 1; }
if [ -f "$site" ] && ! grep -Fqx '# Managed by watchdog deploy-remote.sh' "$site"; then
    echo '/etc/caddy/watchdog.caddy is not managed by this installer; refusing to replace it.' >&2
    exit 1
fi
# A site already present in another file needs explicit manual integration.
if grep -RFl -- "$domain" /etc/caddy 2>/dev/null | grep -vFx "$site" >/dev/null; then
    echo 'Domain already appears in another Caddy file; keep its existing configuration and integrate manually.' >&2
    exit 1
fi

backup=$(mktemp -d /etc/watchdog-deploy-backup.XXXXXXXXXX)
chmod 0700 "$backup"
cp -p "$caddyfile" "$backup/Caddyfile"
if [ -f "$site" ]; then cp -p "$site" "$backup/watchdog.caddy"; fi
if [ -f /etc/watchdog/watchdog.env ]; then cp -p /etc/watchdog/watchdog.env "$backup/watchdog.env"; fi
if [ -f /usr/local/bin/watchdog ]; then cp -p /usr/local/bin/watchdog "$backup/watchdog"; fi
if [ -f /etc/systemd/system/watchdog.service ]; then cp -p /etc/systemd/system/watchdog.service "$backup/watchdog.service"; fi
if [ -f /etc/watchdog/https.env ]; then cp -p /etc/watchdog/https.env "$backup/https.env"; fi
if [ -f /etc/systemd/system/watchdog.service.d/https.conf ]; then cp -p /etc/systemd/system/watchdog.service.d/https.conf "$backup/https.conf"; fi
was_active=false
if systemctl is-active --quiet watchdog; then was_active=true; fi
was_enabled=false
if systemctl is-enabled --quiet watchdog; then was_enabled=true; fi
restore_watchdog() {
    systemctl stop watchdog || true
    if [ "$was_enabled" != true ]; then systemctl disable watchdog || true; fi
    for name in watchdog.env https.env; do
        if [ -f "$backup/$name" ]; then cp -p "$backup/$name" "/etc/watchdog/$name"; else rm -f -- "/etc/watchdog/$name"; fi
    done
    if [ -f "$backup/watchdog" ]; then
        cp -p "$backup/watchdog" /usr/local/bin/watchdog.rollback
        mv -f /usr/local/bin/watchdog.rollback /usr/local/bin/watchdog
    fi
    if [ -f "$backup/watchdog.service" ]; then cp -p "$backup/watchdog.service" /etc/systemd/system/watchdog.service; else rm -f -- /etc/systemd/system/watchdog.service; fi
    if [ -f "$backup/https.conf" ]; then cp -p "$backup/https.conf" /etc/systemd/system/watchdog.service.d/https.conf; else rm -f -- /etc/systemd/system/watchdog.service.d/https.conf; fi
    systemctl daemon-reload
    if [ "$was_active" = true ]; then systemctl restart watchdog || true; fi
}

sh "$upload_dir/install.sh" "$upload_dir/watchdog"
install -o root -g root -m 0600 "$upload_dir/watchdog.env" /etc/watchdog/watchdog.env
install -d -o root -g root -m 0755 /etc/systemd/system/watchdog.service.d
cat > /etc/watchdog/https.env <<'EOF'
WATCHDOG_LISTEN=127.0.0.1:18082
WATCHDOG_SECURE_COOKIE=true
WATCHDOG_ALLOW_REMOTE=false
EOF
chmod 0600 /etc/watchdog/https.env
cat > /etc/systemd/system/watchdog.service.d/https.conf <<'EOF'
[Service]
# Loaded after the copied environment file; enforce the Caddy upstream and HTTPS cookies.
EnvironmentFile=/etc/watchdog/https.env
EOF
chmod 0644 /etc/systemd/system/watchdog.service.d/https.conf
systemctl daemon-reload
systemctl enable watchdog
if ! systemctl restart watchdog; then
    restore_watchdog
    echo "Watchdog restart failed; its previous configuration was restored. Backup: $backup" >&2
    exit 1
fi

# Probe only loopback. No DNS lookup or HTTP request to the domain is performed.
healthy=false
for attempt in 1 2 3 4 5 6 7 8 9 10; do
    if curl --silent --fail --max-time 3 http://127.0.0.1:18082/healthz >/dev/null; then healthy=true; break; fi
    sleep 2
done
if [ "$healthy" != true ]; then
    restore_watchdog
    echo "Watchdog failed its loopback health check; its previous configuration was restored. Backup: $backup. Inspect: journalctl -u watchdog" >&2
    exit 1
fi

# Change only our site and one import line; preserve every existing site.
restore_caddy() {
    cp -p "$backup/Caddyfile" "$caddyfile"
    if [ -f "$backup/watchdog.caddy" ]; then cp -p "$backup/watchdog.caddy" "$site"; else rm -f -- "$site"; fi
}
cat > "$site" <<EOF
# Managed by watchdog deploy-remote.sh
$domain {
    reverse_proxy 127.0.0.1:18082
}
EOF
chmod 0644 "$site"
if ! grep -Fqx "$marker" "$caddyfile"; then
    printf '\n%s\nimport /etc/caddy/watchdog.caddy\n' "$marker" >> "$caddyfile"
fi
if ! caddy validate --config "$caddyfile" --adapter caddyfile; then
    restore_caddy
    restore_watchdog
    echo "Caddy validation failed; its files were restored. Backup: $backup" >&2
    exit 1
fi
if systemctl is-active --quiet caddy; then
    if ! systemctl reload caddy; then
        restore_caddy
        restore_watchdog
        systemctl reload caddy || true
        echo "Caddy reload failed; its files were restored. Backup: $backup" >&2
        exit 1
    fi
else
    if ! systemctl start caddy; then
        restore_caddy
        restore_watchdog
        echo "Caddy start failed; its files were restored. Backup: $backup" >&2
        exit 1
    fi
fi
systemctl enable caddy
printf 'Deployed: https://%s\nSettings: /etc/watchdog/watchdog.env\nBackup: %s\n' "$domain" "$backup"
echo 'Domain reachability and certificate issuance were not checked.'
