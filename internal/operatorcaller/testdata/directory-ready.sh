set -eu
step=architecture
trap 'status=$?; if [ "$status" -ne 0 ]; then printf "caller-ready-%s-failed\n" "$step" >&2; fi' EXIT
[ "$(uname -m)" = 'x86_64' ]
step=ffmpeg
command -v ffmpeg >/dev/null
step=systemd
command -v systemd-run >/dev/null
systemctl show --property=Version --value >/dev/null
step=bridge-hash
[ "$(sha256sum '/opt/caller/bridge' | cut -d ' ' -f 1)" = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' ]
step=load-hash
[ "$(sha256sum '/opt/caller/load' | cut -d ' ' -f 1)" = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' ]
step=config-hash
[ "$(sha256sum '/opt/caller/config.json' | cut -d ' ' -f 1)" = 'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc' ]
step=host
'/opt/caller/bridge' caller-host >/dev/null
step=clock
chronyc tracking | grep -Eq '^Leap status[[:space:]]*:[[:space:]]*Normal$'
printf 'caller-ready\n'
