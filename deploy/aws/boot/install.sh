#!/usr/bin/env bash
# Root-only, fail-closed initial boot. Services do not refresh credentials themselves.
set -euo pipefail
umask 077
root=${RELAIS_INSTALL_ROOT:-/}
boot="$root/opt/relais/boot"
config="$root/etc/relais/node.json"
# Public executable directories must remain traversable after systemd drops root.
install -d -m 0755 "$root/opt/relais" "$boot" "$root/opt/relais/bin"
# A single failure prevents any component from starting.
settings=()
while IFS= read -r value; do settings+=("$value"); done < <(python3 - "$config" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
print(c['version']); print(c['architecture']); print(c['sha256'])
print(' '.join(c['components'])); print(c['ssm'])
PY
)
"$boot/download.sh" "${settings[0]}" "${settings[1]}" "${settings[2]}" "$root/opt/relais/bin" "${settings[3]}"
python3 "$boot/prepare.py" "$config" "$root/run/relais"
"${RELAIS_TEST_SYSCTL:-sysctl}" -p "$root/etc/sysctl.d/60-relais-udp.conf"
manager=${RELAIS_TEST_SYSTEMCTL:-systemctl}
if [[ ${settings[4]} == false ]]; then
  "$manager" disable --now amazon-ssm-agent.service
  "$manager" mask amazon-ssm-agent.service
else
  "$manager" enable --now amazon-ssm-agent.service
fi
for component in ${settings[3]}; do
  if [[ -z ${RELAIS_INSTALL_ROOT:-} ]]; then
    id "relais-$component" >/dev/null 2>&1 || useradd --system --user-group --no-create-home --shell /usr/sbin/nologin "relais-$component"
  fi
done
"$manager" daemon-reload
for component in ${settings[3]}; do
  "$manager" enable --now "relais-$component.service"
  if [[ $component == relay ]]; then "$manager" enable --now relais-relay-standby.service; fi
done
for component in ${settings[3]}; do
  "$manager" is-active --quiet "relais-$component.service"
  if [[ $component == relay ]]; then "$manager" is-active --quiet relais-relay-standby.service; fi
done
if [[ -n ${RELAIS_TEST_READY:-} ]]; then
  "$RELAIS_TEST_READY" readiness
else
  python3 "$boot/ready.py" "$config" "$root/etc/relais/node.env"
fi
echo 'boot services started and ready'
