#!/usr/bin/env python3
"""Refresh only boot-file copies in the native template; never generate topology."""
import sys
import yaml
from template import ROOT, load


def copies():
    for source in sorted((ROOT / 'boot').iterdir()):
        if source.is_file():
            yield '/opt/relais/boot/' + source.name, source.read_text(), '000755'
    for source in sorted((ROOT / 'units').glob('*.service')):
        yield '/etc/systemd/system/' + source.name, source.read_text(), '000644'


def check(template):
    for name, resource in template['Resources'].items():
        if resource['Type'] != 'AWS::EC2::Instance':
            continue
        files = resource['Metadata']['RelaisFiles']
        for path, content, mode in copies():
            assert files[path] == {'content': content, 'mode': mode}, f'{name}: stale {path}'


if __name__ == '__main__':
    data = load()
    if sys.argv[1:] == ['--write']:
        for resource in data['Resources'].values():
            if resource['Type'] == 'AWS::EC2::Instance':
                for path, content, mode in copies():
                    resource['Metadata']['RelaisFiles'][path] = {'content': content, 'mode': mode}
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
