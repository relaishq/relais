"""Architecture binding at the public inventory boundary, without host effects."""
import json
from pathlib import Path
import tempfile
import unittest
import preview_inventory as inventory

class ArchitectureTests(unittest.TestCase):
    def test_configured_architecture_is_required_before_units(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / 'etc/relais').mkdir(parents=True)
            (root / 'etc/relais/node.json').write_text(json.dumps(dict(architecture='x86_64', components=['worker'])))
            class ReachedUnit(Exception):
                pass
            def run(args):
                raise ReachedUnit()
            with self.assertRaises(ReachedUnit):
                inventory.collect('w1', [], root, run, architecture='x86_64')
            for arch in ['arm64', 'amd64', 'unknown']:
                with self.subTest(arch=arch), self.assertRaisesRegex(ValueError, 'node topology differs'):
                    inventory.collect('w1', [], root, run, architecture=arch)
