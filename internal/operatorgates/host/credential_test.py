"""Credential boundary and mechanical relocation checks; no host effects."""
import ast
import hashlib
import json
from pathlib import Path
import types
import unittest
from unittest.mock import patch
import preview_process as host
from relocation import stable_ast


class CredentialTests(unittest.TestCase):
    def test_missing_or_unreadable_credential_never_reads_cloud_or_store(self):
        for error in (FileNotFoundError(), PermissionError()):
            with patch.object(host.pathlib.Path, "stat", side_effect=error), patch.object(host.pathlib.Path, "read_text", side_effect=AssertionError("unexpected credential source read")), patch.object(host, "command", side_effect=AssertionError("external credential read")) as command, patch.object(host, "RedisRead") as redis:
                with self.assertRaises(type(error)):
                    host.ownership({})
                command.assert_not_called()
                redis.assert_not_called()
        def unreadable(path):
            self.assertEqual(str(path), "/run/relais/control.env")
            raise PermissionError()
        with patch.object(host.pathlib.Path, "stat", return_value=types.SimpleNamespace(st_uid=0, st_mode=0o100600)), patch.object(host.pathlib.Path, "read_text", unreadable), patch.object(host, "command", side_effect=AssertionError("external credential read")) as command, patch.object(host, "RedisRead") as redis:
            with self.assertRaises(PermissionError):
                host.ownership({})
            command.assert_not_called()
            redis.assert_not_called()

    def test_credential_is_root_only_data_and_not_shell(self):
        info = types.SimpleNamespace(st_uid=0, st_mode=0o100600)
        with patch.object(host.pathlib.Path, "stat", return_value=info), patch.object(host.pathlib.Path, "read_text", return_value="RELAIS_REDIS_PASSWORD=" + "A" * 64 + "\nRELAIS_SESSIONSTORE_KEY=unused\n"):
            self.assertEqual(host.control_password(), "A" * 64)
        for value in ("", "RELAIS_REDIS_PASSWORD=$(anything)\n", "RELAIS_REDIS_PASSWORD=" + "A" * 64 + "\nRELAIS_REDIS_PASSWORD=" + "B" * 64):
            with patch.object(host.pathlib.Path, "stat", return_value=info), patch.object(host.pathlib.Path, "read_text", return_value=value), self.assertRaises(ValueError):
                host.control_password()
        for uid, mode in ((1, 0o100600), (0, 0o100644)):
            with patch.object(host.pathlib.Path, "stat", return_value=types.SimpleNamespace(st_uid=uid, st_mode=mode)), self.assertRaises(ValueError):
                host.control_password()


class RelocationTests(unittest.TestCase):
    def test_ast_receipt_ignores_parser_added_empty_fields(self):
        tree = ast.parse("def example():\n    return False\n")
        before = stable_ast(tree)
        function = tree.body[0]
        # Python 3.12 added type_params; ast.dump's empty-field formatting
        # changed in 3.13. Simulate both changes on every supported interpreter.
        function._fields = tuple(name for name in function._fields if name != "type_params")
        without_field = stable_ast(tree)
        function._fields += ("type_params",)
        function.type_params = []
        self.assertEqual(before, without_field)
        self.assertEqual(before, stable_ast(tree))
        function.body[0].value.value = True
        self.assertNotEqual(before, stable_ast(tree))

    def test_mechanical_source_receipts(self):
        root = Path(__file__).parent
        receipt = json.loads((root / "relocation.json").read_text())
        for name, record in receipt.items():
            data = (root / name).read_bytes()
            if name == "clock.py":
                text = data.decode()
                data = text[text.index("def clock_evidence():"):].encode()
                expected = record["function_sha256"]
            elif name == "preview_process.py":
                tree = ast.parse(data)
                tree.body = [n for n in tree.body if not isinstance(n, ast.FunctionDef) or n.name != "control_password"]
                for n in tree.body:
                    if isinstance(n, ast.FunctionDef) and n.name == "ownership":
                        seed = next(i for i, x in enumerate(n.body) if isinstance(x, ast.Assign) and any(isinstance(t, ast.Name) and t.id == "seed" for t in x.targets))
                        self.assertEqual(seed, 1)
                        self.assertEqual(stable_ast(n.body[0]), stable_ast(ast.parse("password = control_password()").body[0]))
                        n.body = n.body[seed:]
                data = stable_ast(tree)
                expected = record["normalized_ast_sha256"]
            elif "comment_only_ast_sha256" in record:
                data = stable_ast(ast.parse(data))
                expected = record["comment_only_ast_sha256"]
            else:
                expected = record["sha256"]
            self.assertEqual(hashlib.sha256(data).hexdigest(), expected, name)
