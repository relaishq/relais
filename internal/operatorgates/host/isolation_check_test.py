"""Semantic proof fixtures using libnftables-json(5) expression shapes."""
import copy
import contextlib
import io
import json
import importlib.util
from pathlib import Path
import unittest

path = Path(__file__).resolve().parents[2] / 'operatorrehearsal/assets/isolation_check.py'
spec = importlib.util.spec_from_file_location('isolation_check', path)
proof = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proof)
TABLE = 'relais_gate_isolation'


def fixture(address_form='prefix', enum_form='numbers'):
    rows = [{'metainfo': {'json_schema_version': 1}}, {'table': {'family': 'inet', 'name': TABLE}}]
    def scoped(kind, **fields):
        rows.append({kind: dict(family='inet', table=TABLE, **fields)})
    def match(kind, field, value):
        left = {'payload': {'protocol': kind, 'field': field}} if kind not in ('ct', 'meta') else {kind: {'key': field}}
        return {'match': {'op': 'in' if isinstance(value, dict) and 'set' in value else '==', 'left': left, 'right': value}}
    scoped('set', name='isolated', type='nf_proto', flags=['timeout'], elem=[{'elem': {'val': 'ipv4', 'timeout': 15, 'expires': 14}}, {'elem': {'val': 'ipv6', 'timeout': 15, 'expires': 14}}])
    v4 = {'prefix': {'addr': '169.254.0.0', 'len': 16}}
    v6 = {'prefix': {'addr': 'fe80::', 'len': 10}}
    if address_form == 'cidr':
        v4, v6 = '169.254.0.0/16', 'fe80::/10'
    elif address_form == 'range':
        v4 = {'range': ['169.254.0.0', '169.254.255.255']}
        v6 = {'range': ['fe80::', 'febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff']}
    elif address_form == 'set':
        v4, v6 = {'set': [v4]}, {'set': [v6]}
    elif address_form == 'named-set':
        scoped('set', name='clock4', type='ipv4_addr', flags=['interval'], elem=[{'prefix': {'addr': '169.254.0.0', 'len': 17}}, {'prefix': {'addr': '169.254.128.0', 'len': 17}}])
        scoped('set', name='clock6', type='ipv6_addr', flags=['interval'], elem=[v6])
        v4, v6 = '@clock4', '@clock6'
    nd = {'set': [133, 134, 135, 136]}
    state = 'established'
    if enum_form == 'names':
        nd = {'set': ['nd-router-solicit', 'nd-router-advert', 'nd-neighbor-solicit', 'nd-neighbor-advert']}
        state = {'set': ['established']}
    elif enum_form == 'range':
        nd = {'range': [133, 136]}
        state = 2
    for chain in ('input', 'output'):
        scoped('chain', name=chain, type='filter', hook=chain, prio=-10, policy='accept')
        def rule(*conditions, verdict='accept'):
            scoped('rule', chain=chain, expr=[*conditions, {verdict: None}])
        inbound = chain == 'input'
        rule(match('meta', 'iifname' if inbound else 'oifname', 'lo'))
        address = 'saddr' if inbound else 'daddr'
        rule(match('meta', 'nfproto', 'ipv4'), match('ip', address, v4))
        rule(match('meta', 'nfproto', 'ipv6'), match('ip6', address, v6))
        rule(match('meta', 'l4proto', 'ipv6-icmp'), match('icmpv6', 'type', nd))
        for protocol, port in (('udp', 53), ('tcp', 53), ('tcp', 443)):
            service = ('domain' if port == 53 else 'https') if enum_form == 'names' else port
            terms = [match('meta', 'l4proto', protocol), match(protocol, 'sport' if inbound else 'dport', service), match('ct', 'direction', 'reply' if inbound else 'original')]
            if inbound:
                terms.append(match('ct', 'state', state))
            rule(*terms)
        rule(match('meta', 'nfproto', '@isolated'), verdict='drop')
    if address_form == 'element-objects':
        for row in list(rows):
            if 'set' in row:
                definition = row['set']
                values = definition.pop('elem')
                scoped('element', name=definition['name'], elem=values)
    return {'nftables': rows}


class IsolationReadbackTests(unittest.TestCase):
    def test_documented_prefix_range_and_set_forms(self):
        for form in ('prefix', 'cidr', 'range', 'set', 'named-set', 'element-objects'):
            for enums in ('numbers', 'names', 'range'):
                with self.subTest(addresses=form, enums=enums):
                    proof.isolation_contract(fixture(form, enums))

    def test_every_allowed_rule_is_required_in_its_chain(self):
        original = fixture()
        for i, row in enumerate(original['nftables']):
            if 'rule' not in row:
                continue
            doc = copy.deepcopy(original)
            del doc['nftables'][i]
            with self.subTest(rule=i), self.assertRaises(AssertionError):
                proof.isolation_contract(doc)

    def test_scope_prefix_protocol_state_and_direction_are_semantic(self):
        changes = (
            lambda r: r['table'].update(family='ip') if 'table' in r else None,
            lambda r: r['chain'].update(hook='forward') if 'chain' in r else None,
            lambda r: r['rule'].update(table='other') if 'rule' in r else None,
        )
        for edit in changes:
            doc = fixture()
            for row in doc['nftables']:
                edit(row)
            with self.assertRaises(AssertionError):
                proof.isolation_contract(doc)
        for field, replacement in (('prefix', {'addr': '169.0.0.0', 'len': 8}), ('direction', 'original'), ('state', {'set': ['established', 'related']})):
            doc = fixture()
            for row in doc['nftables']:
                rule = row.get('rule', {})
                if rule.get('chain') != 'input':
                    continue
                for expression in rule['expr']:
                    value = expression.get('match', {})
                    if field == 'prefix' and isinstance(value.get('right'), dict) and 'prefix' in value['right']:
                        value['right'] = {'prefix': replacement}
                    if value.get('left') == {'ct': {'key': field}}:
                        value['right'] = replacement
            with self.subTest(field=field), self.assertRaises(AssertionError):
                proof.isolation_contract(doc)

    def test_empty_table_broad_established_and_early_drop_refuse(self):
        doc = fixture()
        with self.assertRaises(AssertionError):
            proof.isolation_contract({'nftables': doc['nftables'][:2]})
        rules = [r['rule'] for r in doc['nftables'] if r.get('rule', {}).get('chain') == 'input']
        rules[0]['expr'] = [{'match': {'op': '==', 'left': {'ct': {'key': 'state'}}, 'right': 'established'}}, {'accept': None}]
        with self.assertRaises(AssertionError):
            proof.isolation_contract(doc)
        doc = fixture()
        rules = [r['rule'] for r in doc['nftables'] if r.get('rule', {}).get('chain') == 'output']
        rules[0]['expr'], rules[-1]['expr'] = rules[-1]['expr'], rules[0]['expr']
        with self.assertRaises(AssertionError):
            proof.isolation_contract(doc)

    def test_failure_prints_the_exact_json_readback(self):
        doc = fixture()
        doc['nftables'] = doc['nftables'][:2]
        raw = json.dumps(doc)
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr), self.assertRaises(AssertionError):
            proof.check_isolation_json(raw)
        self.assertEqual(stderr.getvalue(), 'NFT_ISOLATION_TABLE_JSON_AT_CHECK: ' + raw + '\n')
