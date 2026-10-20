"""CloudFormation YAML loading and deterministic, offline condition evaluation."""
from pathlib import Path
import re
import yaml

ROOT = Path(__file__).resolve().parents[1]


class Loader(yaml.SafeLoader):
    pass


def tag(loader, suffix, node):
    if isinstance(node, yaml.ScalarNode):
        value = loader.construct_scalar(node)
    elif isinstance(node, yaml.SequenceNode):
        value = loader.construct_sequence(node)
    else:
        value = loader.construct_mapping(node)
    name = suffix if suffix in ('Ref', 'Condition') else 'Fn::' + suffix
    return {name: value.split('.') if suffix == 'GetAtt' and isinstance(value, str) else value}


Loader.add_multi_constructor('!', tag)


def load():
    return yaml.load((ROOT / 'preview.yaml').read_text(), Loader=Loader)


def resolve(template, profile='lean', architecture='arm64', ssm='false'):
    values = {k: v.get('Default', 'TEST_VALUE') for k, v in template['Parameters'].items()}
    values.update(Profile=profile, Architecture=architecture, EnableSessionManager=ssm)
    values.update({'AWS::Region': 'us-east-1', 'AWS::Partition': 'aws', 'AWS::AccountId': 'ACCOUNT',
                   'AWS::StackName': 'relais-offline', 'AWS::NoValue': None})
    conditions = {}

    def evaluate(value):
        if isinstance(value, list):
            return [evaluate(x) for x in value]
        if not isinstance(value, dict):
            return value
        if len(value) == 1:
            op, arg = next(iter(value.items()))
            if op == 'Ref':
                return values.get(arg, arg)
            if op == 'Fn::GetAtt':
                return '.'.join(arg)
            if op == 'Condition':
                return condition(arg)
            if op == 'Fn::Equals':
                return evaluate(arg[0]) == evaluate(arg[1])
            if op == 'Fn::Not':
                return not evaluate(arg[0])
            if op == 'Fn::And':
                return all(evaluate(x) for x in arg)
            if op == 'Fn::Or':
                return any(evaluate(x) for x in arg)
            if op == 'Fn::If':
                return evaluate(arg[1] if condition(arg[0]) else arg[2])
            if op == 'Fn::GetAZs':
                return ['test-az-a', 'test-az-b']
            if op == 'Fn::Select':
                return evaluate(arg[1])[int(arg[0])]
            if op == 'Fn::Base64':
                return evaluate(arg)
            if op == 'Fn::Sub':
                text, subs = (arg, {}) if isinstance(arg, str) else arg
                bindings = {**values, **{k: evaluate(v) for k, v in subs.items()}}
                return re.sub(r'\$\{([^}]+)\}', lambda m: str(bindings.get(m[1], m[1])), text)
        return {k: evaluate(v) for k, v in value.items()}

    def condition(name):
        if name not in conditions:
            conditions[name] = evaluate(template['Conditions'][name])
        return conditions[name]

    return {name: evaluate(resource) for name, resource in template['Resources'].items()
            if 'Condition' not in resource or condition(resource['Condition'])}
