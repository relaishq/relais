"""Keep the local topology defaults outside Linux's ephemeral port range."""
from pathlib import Path
import re
import unittest


class LocalPorts(unittest.TestCase):
    def test_default_topology_ports_stay_below_ephemeral_range(self):
        start = Path('deploy/local/start.sh').read_text()
        install = Path('scripts/install-test/run.sh').read_text()
        base = int(re.search(r'port_base=\$\{PORT_BASE:-(\d+)\}', start).group(1))
        install_base = int(re.search(r"get\('PORT_BASE', '(\d+)'\)", install).group(1))
        self.assertEqual(base, install_base)
        self.assertGreater(base, 1024)
        self.assertLess(base + 81, 32768)
        self.assertTrue(set(range(base, base + 82)).isdisjoint({6379, *range(7000, 7006)}))
