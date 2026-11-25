"""Characterize the default host inventory before optional host admission."""
import unittest
import preview_inventory as inventory

class FourHostTests(unittest.TestCase):
    def test_four_host_inventory_contract(self):
        self.assertEqual({slot: rows for slot, rows in inventory.UNITS.items() if slot in ("relay", "w1", "w2", "shared")}, {
            "relay": [("relay-primary", "relay"), ("relay-standby", "relay")],
            "w1": [("w1", "worker")], "w2": [("w2", "worker")],
            "shared": [("control", "control"), ("signal", "signal")]})
        for message, code in (("node topology differs", "topology"), ("stable target process not running", "not-running"), ("unreviewed unit start", "unit-start")):
            self.assertEqual(inventory.failure_category(ValueError(message)), code)

