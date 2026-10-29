set -eu
umask 077
unit='relais-caller-run-fixture-rollout-survivor-1.service'
work='/opt/caller/work'/"$unit"
remaining=$(({{DEADLINE}} - $(date +%s)))
[ "$remaining" -gt 0 ]
mkdir "$work"
cp '/opt/caller/bridge' "$work/bridge"
cp '/opt/caller/load' "$work/relais-load"
cp '/opt/caller/config.json' "$work/config.json"
[ "$(sha256sum "$work/bridge" | cut -d ' ' -f 1)" = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' ]
[ "$(sha256sum "$work/relais-load" | cut -d ' ' -f 1)" = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' ]
[ "$(sha256sum "$work/config.json" | cut -d ' ' -f 1)" = 'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc' ]
chmod 700 "$work/bridge" "$work/relais-load"
systemd-run --quiet --service-type=exec --unit="$unit" --property="RuntimeMaxSec=$remaining" --property=TimeoutStopSec=180 --property=KillMode=control-group --property=UMask=0077 --property="WorkingDirectory=$work" --setenv=GOMAXPROCS=2 "$work/bridge" caller -config "$work/config.json" -mailbox '/opt/caller/mailbox' -load "$work/relais-load" -identity '{"runId":"run-fixture","gate":"rollout","phase":"survivor","githubRun":"1","attempt":"1","publicSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}'
printf 'caller-started:%s\n' "$unit"
