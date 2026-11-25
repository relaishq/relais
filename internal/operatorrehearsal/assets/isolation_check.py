"""Check the installed isolation contract using nft's JSON expressions.

Readback uses structured prefix/range/set expressions, not the CLI's text
printer. Unknown expressions fail closed with the complete JSON in the caller.
"""
import ipaddress
import json
import sys


def members(value, sets):
    if isinstance(value, str) and value.startswith('@'):
        elements = sets[value[1:]]
        return members({'set': elements if isinstance(elements, list) else [elements]}, {})
    if isinstance(value, dict):
        if 'elem' in value:
            return members(value['elem']['val'], sets)
        if 'range' in value and all(isinstance(v, int) for v in value['range']):
            lo, hi = value['range']
            assert 0 <= hi - lo <= 65535, 'invalid numeric range'
            return list(range(lo, hi + 1))
        if 'set' in value:
            return [v for element in value['set'] for v in members(element, sets)]
    return [value]


def networks(value, sets):
    ranges = []
    for item in members(value, sets):
        if isinstance(item, dict) and 'range' in item:
            lo, hi = map(ipaddress.ip_address, item['range'])
            assert lo.version == hi.version and int(lo) <= int(hi)
        else:
            if isinstance(item, dict):
                prefix = item['prefix']
                item = str(prefix['addr']) + '/' + str(prefix['len'])
            net = ipaddress.ip_network(item, strict=False)
            lo, hi = net.network_address, net.broadcast_address
        ranges.append((lo.version, int(lo), int(hi)))
    # Adjacent set elements and one prefix describe the same address space.
    merged = []
    for version, lo, hi in sorted(ranges):
        if merged and merged[-1][0] == version and lo <= merged[-1][2] + 1:
            merged[-1] = (version, merged[-1][1], max(hi, merged[-1][2]))
        else:
            merged.append((version, lo, hi))
    return tuple(merged)


ALIASES = {
    ('meta', 'l4proto'): {'tcp': 6, 'udp': 17, 'ipv6-icmp': 58},
    ('meta', 'nfproto'): {'ipv4': 2, 'ipv6': 10},
    ('ct', 'direction'): {0: 'original', 1: 'reply'},
    ('ct', 'state'): {2: 'established'},
    ('icmpv6', 'type'): {'nd-router-solicit': 133, 'nd-router-advert': 134,
                        'nd-neighbor-solicit': 135, 'nd-neighbor-advert': 136},
}


def condition(match, sets):
    assert match['op'] in ('==', 'in'), 'unexpected comparison operator'
    left = match['left']
    if 'payload' in left:
        key = (left['payload']['protocol'], left['payload']['field'])
    else:
        kind, description = next(iter(left.items()))
        assert kind in ('ct', 'meta'), 'unexpected match expression'
        key = (kind, description['key'])
    if key[0] in ('ip', 'ip6') and key[1] in ('saddr', 'daddr'):
        values = networks(match['right'], sets)
    else:
        aliases = ALIASES.get(key, {})
        if key[0] in ('tcp', 'udp') and key[1] in ('sport', 'dport'):
            aliases = {'domain': 53, 'https': 443}
        values = frozenset(aliases.get(v, v) for v in members(match['right'], sets))
    return key, values


def rule_contract(rule, sets):
    conditions, verdicts = set(), []
    for statement in rule['expr']:
        if 'match' in statement:
            conditions.add(condition(statement['match'], sets))
        elif 'counter' in statement:
            continue  # Counters do not change packet admission.
        else:
            assert len(statement) == 1 and next(iter(statement)) in ('accept', 'drop'), 'unexpected statement'
            verdicts.append(next(iter(statement)))
    assert len(verdicts) == 1, 'missing or multiple verdicts'
    # nft may retain or omit these implied payload protocol dependencies.
    protocols = {'tcp': 6, 'udp': 17, 'icmpv6': 58}
    families = {'ip': 2, 'ip6': 10}
    for key, _ in list(conditions):
        if key[0] in protocols:
            conditions.discard((('meta', 'l4proto'), frozenset([protocols[key[0]]])))
        if key[0] in families:
            conditions.discard((('meta', 'nfproto'), frozenset([families[key[0]]])))
    return frozenset(conditions), verdicts[0]


def isolation_contract(document, table='relais_gate_isolation'):
    """Require all and only the allowed paths, followed by both-family drops."""
    objects = document['nftables']
    scoped = lambda row: row.get('family') == 'inet' and row.get('table') == table
    assert any(x.get('table', {}).get('family') == 'inet' and x['table'].get('name') == table for x in objects), 'isolation table missing'
    sets = {x['set']['name']: x['set'].get('elem', []) for x in objects if 'set' in x and scoped(x['set'])}
    # JSON commands may carry elements separately from their set declaration.
    for row in objects:
        if 'element' in row and scoped(row['element']):
            element = row['element']
            assert element['name'] in sets, 'elements without a scoped set'
            values = element['elem']
            sets[element['name']].extend(values if isinstance(values, list) else [values])
    timed = [x['set'] for x in objects if 'set' in x and scoped(x['set']) and x['set']['name'] == 'isolated']
    assert len(timed) == 1 and 'timeout' in timed[0].get('flags', []), 'timed isolation set missing'
    for chain in ('input', 'output'):
        chains = [x['chain'] for x in objects if 'chain' in x and scoped(x['chain']) and x['chain']['name'] == chain]
        assert len(chains) == 1 and chains[0].get('type') == 'filter' and chains[0].get('hook') == chain and chains[0].get('prio') == -10 and chains[0].get('policy') == 'accept', 'isolation base chain missing: ' + chain
        expected = {}
        add = lambda name, *terms: expected.update({frozenset(terms): name})
        term = lambda key, *values: (key, frozenset(values))
        inbound = chain == 'input'
        add('loopback', term(('meta', 'iifname' if inbound else 'oifname'), 'lo'))
        address = 'saddr' if inbound else 'daddr'
        add('IPv4 link-local', (('ip', address), networks('169.254.0.0/16', {})))
        add('IPv6 link-local', (('ip6', address), networks('fe80::/10', {})))
        add('ICMPv6 neighbour discovery', term(('icmpv6', 'type'), 133, 134, 135, 136))
        for protocol, port in (('udp', 53), ('tcp', 53), ('tcp', 443)):
            terms = [term((protocol, 'sport' if inbound else 'dport'), port), term(('ct', 'direction'), 'reply' if inbound else 'original')]
            if inbound:
                terms.append(term(('ct', 'state'), 'established'))
            add(protocol + ' port ' + str(port), *terms)
        rules = [x['rule'] for x in objects if 'rule' in x and scoped(x['rule']) and x['rule']['chain'] == chain]
        assert rules, 'isolation rules missing: ' + chain
        for rule in rules[:-1]:
            terms, verdict = rule_contract(rule, sets)
            assert verdict == 'accept' and terms in expected, 'unexpected or overbroad rule in ' + chain
            del expected[terms]
        assert not expected, 'missing allowed paths in ' + chain + ': ' + ', '.join(expected.values())
        terms, verdict = rule_contract(rules[-1], sets)
        assert verdict == 'drop' and terms == frozenset([term(('meta', 'nfproto'), 2, 10)]), 'both-family isolation drop missing: ' + chain


def check_isolation_json(raw):
    try:
        isolation_contract(json.loads(raw))
    except Exception:
        print('NFT_ISOLATION_TABLE_JSON_AT_CHECK: ' + raw, file=sys.stderr)
        raise
