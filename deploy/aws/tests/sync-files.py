#!/usr/bin/env python3
"""Refresh only boot-file copies in the native template; never generate topology."""
import sys
import yaml
from template import ROOT, load


def copies():
    for source in sorted((ROOT / 'boot').iterdir()):
        if source.is_file() and source.name != 'user-data.sh':
            yield ('/etc/sysctl.d/60-relais-udp.conf' if source.suffix == '.conf' else '/opt/relais/boot/' + source.name), source.read_text(), ('000644' if source.suffix == '.conf' else '000755')
    for source in sorted((ROOT / 'units').glob('*.service')):
        yield '/etc/systemd/system/' + source.name, source.read_text(), '000644'


def check(template):
    files = template['Resources']['BootFiles']['Metadata']['RelaisFiles']
    assert set(files) == {path for path, _, _ in copies()}, 'unexpected common metadata file'
    for path, content, mode in copies():
        assert files[path] == {'content': content, 'mode': mode}, 'stale ' + path
    for name, resource in template['Resources'].items():
        if resource['Type'] == 'AWS::EC2::Instance':
            assert set(resource['Metadata']['RelaisFiles']) == {'/etc/relais/node.json', '/etc/relais/node.env'}
            assert resource['Properties']['UserData']['Fn::Base64']['Fn::Sub'] == (ROOT / 'boot/user-data.sh').read_text().replace('NODE_RESOURCE', name)


if __name__ == '__main__':
    data = load()
    if sys.argv[1:] == ['--write']:
        data['Resources']['BootFiles']['Metadata']['RelaisFiles'] = {
            path: {'content': content, 'mode': mode} for path, content, mode in copies()}
        for name, resource in data['Resources'].items():
            if resource['Type'] == 'AWS::EC2::Instance':
                resource['Properties']['UserData']['Fn::Base64']['Fn::Sub'] = (ROOT / 'boot/user-data.sh').read_text().replace('NODE_RESOURCE', name)
        class Dumper(yaml.SafeDumper):
            def ignore_aliases(self, value):
                return True
        def string(dumper, value):
            return dumper.represent_scalar('tag:yaml.org,2002:str', value, style='|' if '\n' in value else None)
        Dumper.add_representer(str, string)
        (ROOT / 'preview.yaml').write_text('# Native CloudFormation; boot copies are maintained by tests/sync-files.py --write.\n' +
                                           yaml.dump(data, Dumper=Dumper, sort_keys=False, width=120))
    check(data)
    print('PASS: embedded boot scripts and systemd units match source files')
