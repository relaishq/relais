#!/bin/bash
# CloudFormation substitutes only the three named placeholders below.
set -eo pipefail
umask 077
root=$RELAIS_INSTALL_ROOT
if [[ -z $root ]]; then root=/; fi
# shellcheck disable=SC2016 # CloudFormation placeholders, substituted before execution.
stack='${AWS::StackName}'
# shellcheck disable=SC2016
region='${AWS::Region}'
resource='NODE_RESOURCE'
finish() {
  outcome=$?
  trap - EXIT
  "$root/opt/aws/bin/cfn-signal" -e "$outcome" --stack "$stack" --resource "$resource" --region "$region"
  exit "$outcome"
}
trap finish EXIT
install -d -m 0755 "$root/opt/relais" "$root/opt/relais/boot" "$root/opt/relais/bin"
install -d -m 0700 "$root/etc/relais"
aws cloudformation describe-stack-resource --stack-name "$stack" --logical-resource-id BootFiles --region "$region" --query StackResourceDetail.Metadata --output text > "$root/etc/relais/common.json"
aws cloudformation describe-stack-resource --stack-name "$stack" --logical-resource-id "$resource" --region "$region" --query StackResourceDetail.Metadata --output text > "$root/etc/relais/host.json"
python3 - "$root" <<'PY'
import json, os, sys
from pathlib import Path
root = Path(sys.argv[1])
entry = json.loads((root / 'etc/relais/common.json').read_text())['RelaisFiles']['/opt/relais/boot/load.py']
p = root / 'opt/relais/boot/load.py'
p.write_text(entry['content']); p.chmod(0o755)
PY
python3 "$root/opt/relais/boot/load.py" "$root"
"$root/opt/relais/boot/install.sh"
