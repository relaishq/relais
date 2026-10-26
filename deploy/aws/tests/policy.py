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
EXPECTED_ACLS = {
    'Relay': """on -@all +ping +cluster|slots +command +time +scan (+eval +evalsha ~relais:preview:lease:*
        ~relais:preview:state:* ~relais:preview:record:* ~relais:preview:epoch:* ~relais:preview:routes:*
        ~relais:preview:ended:* ~relais:preview:setup:* ~relais:preview:checkpoint:*
        ~relais:preview:work:* ~relais:preview:history:* ~relais:preview:relay:*) (+hmget
        ~relais:preview:lease:* ~relais:preview:ended:* ~relais:preview:relay:*) (+hget
        ~relais:preview:lease:* ~relais:preview:routes:* ~relais:preview:relay:*) (+hgetall +pttl +hdel
        +pexpireat ~relais:preview:routes:*) (+hset ~relais:preview:routes:* ~relais:preview:relay:*)
        (+hincrby ~relais:preview:relay:*) (+del ~relais:preview:state:* ~relais:preview:record:*
        ~relais:preview:routes:* ~relais:preview:setup:* ~relais:preview:checkpoint:*
        ~relais:preview:work:* ~relais:preview:history:*)""".split(),
    'Worker': """on -@all +ping +cluster|slots +command +eval +evalsha +script|load +time +hmget +hget +hgetall +hset
        +hincrby +hdel +get +set +del +exists +incr +pttl +pexpire +pexpireat +zadd +zrange +zrem +scan
        ~relais:preview:* +type +hexists""".split(),
    'Control': """on -@all +ping +cluster|slots +command +eval +evalsha +script|load +time +hmget +hget +hgetall +hset
        +hincrby +hdel +get +set +del +exists +incr +pttl +pexpire +pexpireat +zadd +zrange +zrem +scan
        ~relais:preview:* +type +hexists +hlen""".split(),
}
RETAINED = {'Store', 'StoreUsers', 'RelayPassword', 'SnapshotKey', 'WorkerStoreUser', 'SignalToken', 'PrivateB', 'StoreSubnets', 'RelayStoreUser', 'ControlPassword', 'Vpc', 'RelayAddress', 'ControlStoreUser', 'PrivateA', 'StoreGroup', 'WorkerPassword'}

SSM_ACTIONS = {'ssmmessages:CreateControlChannel', 'ssmmessages:CreateDataChannel',
               'ssmmessages:OpenControlChannel', 'ssmmessages:OpenDataChannel'}
SECRET_SCOPE = {'Relay': {'RelayPassword'}, 'Worker': {'WorkerPassword', 'SnapshotKey'},
                'Control': {'ControlPassword', 'SnapshotKey'}, 'Signal': {'SignalToken'},
                'Shared': {'ControlPassword', 'SnapshotKey', 'SignalToken'}}


def check_component_ports(template, launcher):
    ports = {int(port) for port in re.findall(r'\$\{[A-Z_]+:-([0-9]+)\}', launcher)}
    assert ports, 'component defaults must declare explicit ports'
    for resource in template['Resources'].values():
        properties = resource.get('Properties', {})
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
    assert not re.search(r'RELAIS_(?:SESSIONSTORE_KEY|REDIS_PASSWORD)\s*=', encoded), 'literal credential assignment'
    resources = template['Resources']
    assert 'Transform' not in template, 'native CloudFormation only'
    assert len((ROOT / 'preview.yaml').read_bytes()) < 1024 * 1024
    import importlib.util
    spec = importlib.util.spec_from_file_location('embedded_files', ROOT / 'tests/sync-files.py')
    module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
    module.check(template)
    expected_roles = {name + 'Role' for name in SECRET_SCOPE}
    assert {n for n, r in resources.items() if r['Type'] == 'AWS::IAM::Role'} == expected_roles
    for name, resource in resources.items():
        kind = resource['Type']
        properties = resource.get('Properties', {})
        if kind in ('AWS::EC2::Instance', 'AWS::EC2::NetworkInterface', 'AWS::EC2::NatGateway',
                    'AWS::ElasticLoadBalancingV2::LoadBalancer', 'AWS::ElastiCache::ReplicationGroup'):
            assert set(resource['DependsOn']) >= {'RelayAddressReservation', 'WorkerOneReservation',
                'WorkerTwoReservation', 'WorkerThreeReservation', 'ControlAddressReservation', 'SignalAddressReservation'}
        if kind == 'AWS::EC2::Instance':
            assert 'BootFiles' in resource['DependsOn']
        if kind.startswith('AWS::IAM::'):

            assert kind in ('AWS::IAM::Role', 'AWS::IAM::InstanceProfile')
        if kind == 'AWS::IAM::Role':
            assert set(properties) == {'AssumeRolePolicyDocument', 'Policies', 'RoleName'}, 'role properties not allowed'
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
        if name in RETAINED:
            assert resource.get('DeletionPolicy') == 'RetainExceptOnCreate', name
            assert resource.get('UpdateReplacePolicy') == 'Retain', name
        else:
            assert 'DeletionPolicy' not in resource and 'UpdateReplacePolicy' not in resource, name + ' needlessly retained'
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
                    assert v['ImageId'] == 'TEST_VALUE'
                    assert instance['CreationPolicy'] == {'ResourceSignal': {'Count': 1, 'Timeout': 'PT15M'}}
                    assert set(instance['DependsOn']) >= {n for n,x in r.items() if x['Type'] == 'AWS::EC2::SubnetCidrReservation'}
                    assert len(v['UserData'].encode()) < 16384
                    assert '{{resolve:' not in v['UserData'] and 'SecretString' not in v['UserData']
                    assert v['UserData'] == (ROOT / 'boot/user-data.sh').read_text().replace('NODE_RESOURCE', name).replace('${AWS::Region}', 'us-east-1').replace('${AWS::StackName}', 'relais-offline')
                    assert set(instance['Metadata']['RelaisFiles']) == {'/etc/relais/node.json', '/etc/relais/node.env'}
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
                # ElastiCache refuses ReplicasPerNodeGroup together with NodeGroupConfiguration ReplicaCount.
                assert 'ReplicasPerNodeGroup' not in store
                assert store['NumNodeGroups'] == len(store['NodeGroupConfiguration']) == 1
                group = store['NodeGroupConfiguration'][0]
                assert group['ReplicaCount'] == len(group['ReplicaAvailabilityZones']) == 1
                assert group['PrimaryAvailabilityZone'] != group['ReplicaAvailabilityZones'][0]
                assert store['TransitEncryptionMode'] == 'required'
                for role, expected_secrets in SECRET_SCOPE.items():
                    statements = p[role + 'Role']['Policies'][0]['PolicyDocument']['Statement']
                    action_sets = {frozenset(st['Action']) for st in statements if st}
                    expected_actions = {frozenset([action]) for action in (
                        'secretsmanager:GetSecretValue', 'cloudformation:DescribeStackResource', 'cloudformation:SignalResource')}
                    if ssm == 'true':
                        expected_actions |= {frozenset(SSM_ACTIONS), frozenset(['ssm:UpdateInstanceInformation'])}
                    assert action_sets == expected_actions
                    for statement in filter(None, statements):
                        actions = statement['Action']
                        assert not any('*' in a or a.lower() == 'iam:passrole' for a in actions)
                        if statement['Resource'] == '*':
                            assert ssm == 'true' and set(actions) == SSM_ACTIONS
                        elif actions == ['ssm:UpdateInstanceInformation']:
                            assert ssm == 'true'
                            assert statement['Resource'] == 'arn:aws:ec2:us-east-1:ACCOUNT:instance/*'
                            assert statement['Condition'] == {'StringEquals': {'aws:ResourceTag/RelaisRole': role.lower(), 'aws:ResourceTag/RelaisStack': 'relais-offline'}}
                        elif any(a.startswith('cloudformation:') for a in actions):
                            assert actions in (['cloudformation:DescribeStackResource'], ['cloudformation:SignalResource'])
                            assert statement['Resource'] == 'arn:aws:cloudformation:us-east-1:ACCOUNT:stack/relais-offline/*'
                        else:
                            assert actions == ['secretsmanager:GetSecretValue']
                            assert set(statement['Resource']) == expected_secrets
                assert p['SignalListener']['Protocol'] == 'HTTPS' and p['SignalListener']['Port'] == 443
                assert p['SignalTargets']['Protocol'] == 'HTTPS'
                assert p['SignalTargets']['Targets'][0]['Id'] == ('SharedHost' if profile == 'lean' else 'SignalHost')
                assert p['SignalWafAssociation']['ResourceArn'] == 'SignalAlb'
                assert p['SignalWaf']['Rules'][0]['Statement']['RateBasedStatement']['AggregateKeyType'] == 'IP'
                assert store['UserGroupIds'] == ['StoreUsers']
                assert {n for n,x in r.items() if x['Type'] == 'AWS::ElastiCache::User'} == {role + 'StoreUser' for role in EXPECTED_ACLS}
                assert p['StoreUsers']['UserIds'] == [role + 'StoreUser' for role in EXPECTED_ACLS]
                assert p['SignalWaf']['Rules'][0]['Action'] == {'Block': {}}
                for role, tokens in EXPECTED_ACLS.items():
                    user = p[role + 'StoreUser']
                    assert user['UserName'] == role.lower() and user['AuthenticationMode']['Type'] == 'password'
                    assert user['AccessString'].split() == tokens, role + ' ACL drift'
                reservations = {n:x['Properties'] for n,x in r.items() if x['Type'] == 'AWS::EC2::SubnetCidrReservation'}
                assert {(x['SubnetId'],x['Cidr'],x['ReservationType']) for x in reservations.values()} == {
                    ('PublicA','10.42.0.10/32','explicit'),('PrivateA','10.42.10.11/32','explicit'),
                    ('PrivateB','10.42.11.12/32','explicit'),('PrivateA','10.42.10.13/32','explicit'),
                    ('PrivateA','10.42.10.20/32','explicit'),('PrivateB','10.42.11.21/32','explicit')}
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
    names = ['public_https_only','no_ssh','imds_v2_required','encrypted_root_volume','synchronous_store',
             'cluster_store','store_survives_deletion','role_secret_resource_scope','no_wildcard_action',
             'generated_secret_only','profile_worker_count','component_ingress_paths','no_inline_ingress',
             'store_tls','store_at_rest','store_multi_az','relay_address_survives_replacement',
             'single_inline_role_policy','no_secret_output','host_uses_own_role']
    mutations = list(zip(names, mutations))
    mutations += [
        ('no_managed_role_policy', lambda t: t['Resources']['WorkerRole']['Properties'].update(ManagedPolicyArns=['AdministratorAccess'])),
        ('store_role_group_attached', lambda t: t['Resources']['Store']['Properties'].pop('UserGroupIds')),
        ('no_extra_embedded_secret_file', lambda t: t['Resources']['BootFiles']['Metadata']['RelaisFiles'].update({'/etc/extra': {'content': 'RELAIS_SESSIONSTORE_KEY=literal', 'mode':'000600'}})),
        ('waf_blocks_excess', lambda t: t['Resources']['SignalWaf']['Properties']['Rules'][0].update(Action={'Count':{}})),
        ('no_default_valkey_user', lambda t: t['Resources'].update(DisabledDefaultUser={'Type':'AWS::ElastiCache::User','Properties':{'UserName':'default','AuthenticationMode':{'Type':'no-password-required'}}})),
        ('failed_create_cleans_store', lambda t: t['Resources']['Store'].update(DeletionPolicy='Retain')),
        ('fixed_addresses_reserved', lambda t: t['Resources'].pop('WorkerOneReservation')),
        ('interfaces_wait_for_reservations', lambda t: t['Resources']['Store'].pop('DependsOn')),
        ('hosts_wait_for_shared_metadata', lambda t: t['Resources']['RelayHost']['DependsOn'].remove('BootFiles')),
        ('boot_success_signal_required', lambda t: t['Resources']['RelayHost'].pop('CreationPolicy')),
    ]
    for token in ('allcommands','allkeys','%RW~*','~relais:*','+flushall'):
        mutations.append(('relay_rejects_' + token, lambda t, token=token: t['Resources']['RelayStoreUser']['Properties'].update(AccessString=t['Resources']['RelayStoreUser']['Properties']['AccessString']+' '+token)))
    for token in ('+@admin','+flushall +config'):
        mutations.append(('worker_rejects_' + token, lambda t, token=token: t['Resources']['WorkerStoreUser']['Properties'].update(AccessString=t['Resources']['WorkerStoreUser']['Properties']['AccessString']+' '+token)))
    for name, mutate in mutations:
        changed = copy.deepcopy(template)
        mutate(changed)
        try:
            check(changed)
        except (AssertionError, KeyError):
            continue
        raise AssertionError('policy mutation escaped detection: ' + name)
    print(f'PASS: {len(mutations)} unsafe template mutations refused')


if __name__ == '__main__':
    template = load()
    check(template)
    regression_tests(template)
    print('PASS: both profiles, both architectures, SSM off/on, security and durability')
