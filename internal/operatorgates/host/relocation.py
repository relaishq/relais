"""Stable AST receipts for mechanical source relocation.

Parser-added empty fields do not change Python semantics. Omit all empty fields
and source locations, and encode node kinds, field names and scalar values as
canonical JSON rather than relying on the version-specific ast.dump format.
"""
import ast
import json


def stable_ast(tree):
    def encode(value):
        if isinstance(value, ast.AST):
            return {"node": type(value).__name__, "fields": {
                name: encode(item) for name, item in ast.iter_fields(value)
                if item is not None and item != []
            }}
        if isinstance(value, list):
            return [encode(item) for item in value]
        if isinstance(value, bytes):
            return {"bytes": value.hex()}
        if isinstance(value, float):
            return {"float": value.hex()}
        if isinstance(value, complex):
            return {"complex": [value.real.hex(), value.imag.hex()]}
        if value is Ellipsis:
            return {"ellipsis": True}
        return value

    return json.dumps(encode(tree), sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode("ascii")
