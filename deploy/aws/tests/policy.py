#!/usr/bin/env python3
"""Fail closed on preview topology/security drift, without credentials or AWS APIs."""
import copy
import json
import re
from template import ROOT, load, resolve
from cfnlint.schema import PROVIDER_SCHEMA_MANAGER

EXPECTED_PATHS = {
    ('WorkerGroup', 'RelayGroup', 'udp', 24781), ('RelayGroup', 'WorkerGroup', 'udp', 24711),
    ('ControlGroup', 'WorkerGroup', 'tcp', 24701), ('WorkerGroup', 'ControlGroup', 'tcp', 24710),
    ('ControlGroup', 'RelayGroup', 'tcp', 24700), ('SignalGroup', 'ControlGroup', 'tcp', 24710),
    ('AlbGroup', 'SignalGroup', 'tcp', 24743),
    *((role + 'Group', 'StoreGroup', 'tcp', 6379) for role in ('Worker', 'Control', 'Relay')),
}
SSM_ACTIONS = {'ssmmessages:CreateControlChannel', 'ssmmessages:CreateDataChannel',
               'ssmmessages:OpenControlChannel', 'ssmmessages:OpenDataChannel'}
SECRET_SCOPE = {'Relay': {'RelayPassword'}, 'Worker': {'WorkerPassword', 'SnapshotKey'},
                'Control': {'ControlPassword', 'SnapshotKey'}, 'Signal': {'SignalToken'},
                'Shared': {'ControlPassword', 'SnapshotKey', 'SignalToken'}}


def check_component_ports(template, launcher):
    ports = {int(port) for port in re.findall(r'\$\{[A-Z_]+:-([0-9]+)\}', launcher)}
    assert ports, 'component defaults must declare explicit ports'
    for resource in template['Resources'].values():
        properties = resource['Properties']
        if properties.get('IpProtocol') in ('tcp', 'udp'):
            ports.update(properties[key] for key in ('FromPort', 'ToPort') if key in properties)
        if resource['Type'].startswith('AWS::ElasticLoadBalancingV2::') and 'Port' in properties:
            ports.add(properties['Port'])
    for port in ports:
        assert isinstance(port, int) and not 32768 <= port <= 60999, (
            'component port lies in unreserved Linux ephemeral range: ' + str(port))


def check(template):
    for role in ('relay', 'relay-standby', 'worker', 'control', 'signal'):
        text = (ROOT / 'units' / ('relais-' + role + '.service')).read_text()
        fields = [line for line in text.splitlines() if line and not line.startswith('[')]
        identity = 'relay' if role == 'relay-standby' else role
        required = {'User=relais-' + identity, 'Group=relais-' + identity,
                    'ExecStart=/opt/relais/boot/run.sh ' + role, 'Restart=no',
                    'EnvironmentFile=/etc/relais/node.env', 'NoNewPrivileges=yes',
                    'ProtectSystem=strict', 'ProtectHome=yes', 'CapabilityBoundingSet=',
                    'IPAddressDeny=169.254.169.254/32', 'UMask=0077'}
        assert required <= set(fields), role + ' unit weakened'
        for key in ('User', 'Group', 'ExecStart', 'Restart', 'NoNewPrivileges', 'ProtectSystem', 'ProtectHome', 'CapabilityBoundingSet', 'IPAddressDeny', 'UMask'):
            assert sum(line.startswith(key + '=') for line in fields) == 1, role + ' duplicate ' + key
        expected_files = ['EnvironmentFile=/etc/relais/node.env']
        if role != 'signal':
            expected_files.append('EnvironmentFile=/run/relais/' + identity + '.env')
        assert [line for line in fields if line.startswith('EnvironmentFile=')] == expected_files
        assert not any(line.startswith('Environment=') for line in fields)
        assert [line for line in fields if line.startswith('LoadCredential=')] == (
            ['LoadCredential=signal.token:/run/relais/signal.token'] if role == 'signal' else [])
        if role == 'signal':
            assert 'LoadCredential=signal.token:/run/relais/signal.token' in fields
        else:
            assert 'EnvironmentFile=/run/relais/' + identity + '.env' in fields
        assert 'PrivateUsers=yes' not in fields and 'ProtectProc=invisible' not in fields
    launcher = (ROOT / 'boot/run.sh').read_text()
    check_component_ports(template, launcher)
    assert not any(flag in launcher for flag in ('-demo-register', '-test-register-worker', 'SRTP_DIAGNOSTICS'))
    # Read only bundled, pinned schemas: no schema update/network request is made.
    properties = PROVIDER_SCHEMA_MANAGER.get_resource_schema('us-east-1', 'AWS::ElastiCache::ReplicationGroup').schema['properties']
    assert 'sync' in properties['Durability']['enumCaseInsensitive'], 'STOP: pinned schema lacks synchronous durability'
    encoded = json.dumps(template)
    assert not re.search(r'(?<![0-9])[0-9]{12}(?![0-9])|AKIA[A-Z0-9]{16}|-----BEGIN .*PRIVATE KEY', encoded)
    assert not re.search(r'(?i)(password|token|secret_access_key)\s*[:=]\s*["\']?[A-Za-z0-9+/]{24,}', encoded)
    resources = template['Resources']
    assert 'Transform' not in template, 'native CloudFormation only'
    assert len((ROOT / 'preview.yaml').read_bytes()) < 1024 * 1024
    expected_roles = {name + 'Role' for name in SECRET_SCOPE}
    assert {n for n, r in resources.items() if r['Type'] == 'AWS::IAM::Role'} == expected_roles
    for name, resource in resources.items():
        kind = resource['Type']
        properties = resource['Properties']
        if kind.startswith('AWS::IAM::'):
            assert kind in ('AWS::IAM::Role', 'AWS::IAM::InstanceProfile')
        if kind == 'AWS::IAM::Role':
            assert len(properties['Policies']) == 1
            assert properties['AssumeRolePolicyDocument'] == {'Version': '2012-10-17', 'Statement': [
                {'Effect': 'Allow', 'Principal': {'Service': 'ec2.amazonaws.com'}, 'Action': 'sts:AssumeRole'}]}
        if kind == 'AWS::EC2::SecurityGroup':
            assert properties.get('SecurityGroupIngress', []) == [], 'use asserted component rules'
            assert properties['SecurityGroupEgress'] == [{'IpProtocol': 'icmp', 'FromPort': 252, 'ToPort': 86,
                'CidrIp': '127.0.0.1/32', 'Description': 'Unusable rule suppresses the default allow-all egress'}]
        assert kind != 'AWS::CloudFormation::Stack', 'nested policy bypass'
    assert set(template['Outputs']) == {'RelayIPv4', 'SignalLoadBalancer', 'StoreConfigurationEndpoint'}
    assert template['Outputs']['RelayIPv4']['Value'] == {'Ref': 'RelayAddress'}
    assert template['Outputs']['SignalLoadBalancer']['Value'] == {'Fn::GetAtt': ['SignalAlb', 'DNSName']}
    assert template['Outputs']['StoreConfigurationEndpoint']['Value'] == {'Fn::GetAtt': ['Store', 'ConfigurationEndPoint.Address']}
    assert not any('AutoScaling' in r['Type'] or 'LifecycleHook' in r['Type'] for r in resources.values())
    for name, resource in resources.items():
        if resource['Type'] in ('AWS::SecretsManager::Secret', 'AWS::ElastiCache::ReplicationGroup',
                                'AWS::ElastiCache::User', 'AWS::ElastiCache::UserGroup', 'AWS::ElastiCache::SubnetGroup') or name in {'RelayAddress', 'Vpc', 'PrivateA', 'PrivateB', 'RelayGroup', 'WorkerGroup', 'ControlGroup', 'SignalGroup', 'StoreGroup'}:
            assert resource.get('DeletionPolicy') == resource.get('UpdateReplacePolicy') == 'Retain', name
        if resource['Type'] == 'AWS::SecretsManager::Secret':
            assert 'GenerateSecretString' in resource['Properties'] and 'SecretString' not in resource['Properties']
        if resource['Type'] == 'AWS::ElastiCache::User':
            for password in resource['Properties']['AuthenticationMode'].get('Passwords', []):
                assert list(password) == ['Fn::Sub']
                assert re.fullmatch(r'\{\{resolve:secretsmanager:\$\{(?:Relay|Worker|Control)Password\}:SecretString\}\}', password['Fn::Sub'])
    for profile in ('lean', 'resilience'):
        for architecture in ('arm64', 'x86_64'):
            for ssm in ('false', 'true'):
                r = resolve(template, profile, architecture, ssm)
                p = {n: v['Properties'] for n, v in r.items()}
                instances = {n: x for n, x in r.items() if x['Type'] == 'AWS::EC2::Instance'}
                expected = {'RelayHost', 'WorkerOne', 'WorkerTwo'} | ({'SharedHost'} if profile == 'lean' else {'WorkerThree', 'ControlHost', 'SignalHost'})
                assert instances.keys() == expected
                assert sum(x['Type'] == 'AWS::EC2::NatGateway' for x in r.values()) == (1 if profile == 'lean' else 2)
                assert sum(x['Type'] == 'AWS::EC2::Subnet' for x in r.values()) == 4
                assert p['RelayInterface']['GroupSet'] == ['RelayGroup']
                paths = set()
                egress_paths = set()
                for n, resource in r.items():
                    if resource['Type'] not in ('AWS::EC2::SecurityGroupIngress', 'AWS::EC2::SecurityGroupEgress'):
                        continue
                    rule = p[n]
                    assert not (rule.get('IpProtocol') in ('-1', -1) or rule.get('FromPort', 23) <= 22 <= rule.get('ToPort', 21)), n
                    assert rule.get('FromPort') == rule.get('ToPort'), n
                    if resource['Type'].endswith('Ingress'):
                        if 'SourceSecurityGroupId' in rule:
                            paths.add((rule['SourceSecurityGroupId'], rule['GroupId'], rule['IpProtocol'], rule['FromPort']))
                        else:
                            assert (n, rule['GroupId'], rule['IpProtocol'], rule['FromPort'], rule.get('CidrIp'), rule.get('CidrIpv6')) in {
                                ('PublicRelay', 'RelayGroup', 'udp', 24780, '0.0.0.0/0', None),
                                ('PublicHttps', 'AlbGroup', 'tcp', 443, '0.0.0.0/0', None)}, n
                    elif 'DestinationSecurityGroupId' in rule:
                        egress_paths.add((rule['GroupId'], rule['DestinationSecurityGroupId'], rule['IpProtocol'], rule['FromPort']))
                    else:
                        assert rule['IpProtocol'] == 'tcp' and rule['FromPort'] == 443 and rule['CidrIp'] == '0.0.0.0/0'
                assert paths == egress_paths == EXPECTED_PATHS
                for name, instance in instances.items():
                    v = instance['Properties']
                    assert 'KeyName' not in v and v['MetadataOptions']['HttpTokens'] == 'required'
                    assert v['MetadataOptions']['HttpPutResponseHopLimit'] == 1
                    assert name == 'RelayHost' or v['SubnetId'] in ('PrivateA', 'PrivateB')
                    assert all(not x.get('AssociatePublicIpAddress', False) for x in v.get('NetworkInterfaces', []))
                    assert all(b['Ebs']['Encrypted'] is True for b in v['BlockDeviceMappings'])
                    assert v['ImageId'] == template['Parameters']['ArmImage' if architecture == 'arm64' else 'X86Image']['Default']
                    assert len(v['UserData'].encode()) < 16384
                    assert '{{resolve:' not in v['UserData'] and 'SecretString' not in v['UserData']
                    assert not re.search(r'(?i)secret|password|token|RELAIS_', v['UserData'])
                    assert set(re.findall(r'\$\{([^}]+)\}', template['Resources'][name]['Properties']['UserData']['Fn::Base64']['Fn::Sub'])) == {'AWS::Region', 'AWS::StackName'}
                    node = json.loads(instance['Metadata']['RelaisFiles']['/etc/relais/node.json']['content'])
                    role = dict((t['Key'], t['Value']) for t in v['Tags'])['RelaisRole']
                    assert set(node['secrets'].values()) == SECRET_SCOPE[role.title()]
                    assert v['IamInstanceProfile'] == role.title() + 'InstanceProfile'
                    assert node['components'] == (['control', 'signal'] if role == 'shared' else [role])
                    groups = v.get('SecurityGroupIds', ['RelayGroup'])
                    assert set(groups) == ({'ControlGroup', 'SignalGroup'} if role == 'shared' else {role.title() + 'Group'})
                    env = instance['Metadata']['RelaisFiles']['/etc/relais/node.env']['content']
                    settings = dict(line.split('=', 1) for line in env.splitlines())
                    assert set(settings) == {'RELAIS_REDIS_ADDR', 'RELAIS_REDIS_CLUSTER', 'RELAIS_REDIS_TLS',
                        'RELAIS_REDIS_PREFIX', 'RELAIS_PRIVATE_NETS', 'RELAY_PRIVATE', 'RELAY_PUBLIC',
                        'CONTROL_PRIVATE', 'WORKER_ONE', 'WORKER_TWO', 'WORKER_THREE', 'WORKER_CALL_CAP',
                        'NODE_PRIVATE', 'WORKER_NAME'}
                    assert settings['WORKER_ONE'] == p['WorkerOne']['PrivateIpAddress']
                    assert settings['WORKER_TWO'] == p['WorkerTwo']['PrivateIpAddress']
                    assert 'WORKER_THREE=10.42.10.13' in env if profile == 'resilience' else 'WORKER_THREE=\n' in env
                    if name.startswith('Worker'):
                        assert v['PrivateIpAddress'] in env
                store = p['Store']
                for field in ('AutomaticFailoverEnabled', 'MultiAZEnabled', 'AtRestEncryptionEnabled', 'TransitEncryptionEnabled'):
                    assert store[field] is True, field
                assert store['Durability'] == 'sync' and store['ClusterMode'] == 'enabled'
                assert store['Engine'] == 'valkey' and store['EngineVersion'] == '9.0'
                assert store['NumNodeGroups'] == store['ReplicasPerNodeGroup'] == 1
                group = store['NodeGroupConfiguration'][0]
                assert group['PrimaryAvailabilityZone'] != group['ReplicaAvailabilityZones'][0]
                assert store['TransitEncryptionMode'] == 'required'
                for role, expected_secrets in SECRET_SCOPE.items():
                    statements = p[role + 'Role']['Policies'][0]['PolicyDocument']['Statement']
                    for statement in filter(None, statements):
                        actions = statement['Action']
                        assert not any('*' in a or a.lower() == 'iam:passrole' for a in actions)
                        if statement['Resource'] == '*':
                            assert ssm == 'true' and set(actions) == SSM_ACTIONS
                        elif actions == ['ssm:UpdateInstanceInformation']:
                            assert ssm == 'true'
                            assert statement['Resource'] == 'arn:aws:ec2:us-east-1:ACCOUNT:instance/*'
                            assert statement['Condition'] == {'StringEquals': {'aws:ResourceTag/RelaisRole': role.lower(), 'aws:ResourceTag/RelaisStack': 'relais-offline'}}
                        elif 'cloudformation:DescribeStackResource' in actions:
                            assert actions == ['cloudformation:DescribeStackResource']
                            assert statement['Resource'] == 'arn:aws:cloudformation:us-east-1:ACCOUNT:stack/relais-offline/*'
                        else:
                            assert actions == ['secretsmanager:GetSecretValue']
                            assert set(statement['Resource']) == expected_secrets
                assert p['SignalListener']['Protocol'] == 'HTTPS' and p['SignalListener']['Port'] == 443
                assert p['SignalTargets']['Protocol'] == 'HTTPS'
                assert p['SignalTargets']['Targets'][0]['Id'] == ('SharedHost' if profile == 'lean' else 'SignalHost')
                assert p['SignalWafAssociation']['ResourceArn'] == 'SignalAlb'
                assert p['SignalWaf']['Rules'][0]['Statement']['RateBasedStatement']['AggregateKeyType'] == 'IP'
                assert p['DisabledDefaultUser']['AccessString'] == 'off -@all'
                for role in ('Relay', 'Worker', 'Control'):
                    acl = p[role + 'StoreUser']['AccessString'].split()
                    assert '-@all' in acl and '+@all' not in acl and '~*' not in acl
                relay = p['RelayStoreUser']['AccessString'].split()
                assert '+get' not in relay and '+set' not in relay and '~relais:preview:*' not in relay
    assert not any(re.search(r'Secret|Password|SnapshotKey|SignalToken', json.dumps(v)) for v in template['Outputs'].values())


def regression_tests(template):
    # Each mutation targets a required boundary. A green test that misses one fails here.
    mutations = [
        lambda t: t['Resources']['PublicHttps']['Properties'].update(FromPort=80, ToPort=80),
        lambda t: t['Resources']['PublicRelay']['Properties'].update(FromPort=22, ToPort=22),
        lambda t: t['Resources']['WorkerOne']['Properties']['MetadataOptions'].update(HttpTokens='optional'),
        lambda t: t['Resources']['WorkerTwo']['Properties']['BlockDeviceMappings'][0]['Ebs'].update(Encrypted=False),
        lambda t: t['Resources']['Store']['Properties'].update(Durability='async'),
        lambda t: t['Resources']['Store']['Properties'].update(ClusterMode='disabled'),
        lambda t: t['Resources']['Store'].update(DeletionPolicy='Delete'),
        lambda t: t['Resources']['WorkerRole']['Properties']['Policies'][0]['PolicyDocument']['Statement'][0].update(Resource='*'),
        lambda t: t['Resources']['WorkerRole']['Properties']['Policies'][0]['PolicyDocument']['Statement'][0].update(Action=['secretsmanager:*']),
        lambda t: t['Resources']['SignalToken']['Properties'].update(SecretString='literal credential'),
        lambda t: t['Resources']['WorkerThree'].update(Condition='Lean'),
        lambda t: t['Resources']['WorkerControlApiIngress']['Properties'].update(SourceSecurityGroupId={'Ref': 'SignalGroup'}),
        lambda t: t['Resources']['WorkerGroup']['Properties'].update(SecurityGroupIngress=[{'IpProtocol': 'tcp', 'FromPort': 22, 'ToPort': 22, 'CidrIp': '0.0.0.0/0'}]),
        lambda t: t['Resources']['Store']['Properties'].update(TransitEncryptionEnabled=False),
        lambda t: t['Resources']['Store']['Properties'].update(AtRestEncryptionEnabled=False),
        lambda t: t['Resources']['Store']['Properties'].update(MultiAZEnabled=False),
        lambda t: t['Resources']['RelayAddress'].update(UpdateReplacePolicy='Delete'),
        lambda t: t['Resources']['WorkerRole']['Properties']['Policies'].append({'PolicyName': 'Unexpected', 'PolicyDocument': {'Statement': [{'Action': '*', 'Resource': '*'}]}}),
        lambda t: t['Outputs'].update(LeakedToken={'Value': 'literal credential'}),
        lambda t: t['Resources']['WorkerOne']['Properties'].update(IamInstanceProfile={'Ref': 'SharedInstanceProfile'}),
    ]
    for mutate in mutations:
        changed = copy.deepcopy(template)
        mutate(changed)
        try:
            check(changed)
        except AssertionError:
            continue
        raise AssertionError('policy mutation escaped detection')
    print(f'PASS: {len(mutations)} unsafe template mutations refused')


if __name__ == '__main__':
    template = load()
    check(template)
    regression_tests(template)
    print('PASS: both profiles, both architectures, SSM off/on, security and durability')
