"""Extract P2 function invocations from a frozen language-neutral corpus.

Usage: python3 extract.py CORPUS_DIRECTORY
Only the input archive/manifest are read. Expected results are never made by Go.
"""
import gzip
import hashlib
import io
import json
from pathlib import Path
import sys

root = Path(sys.argv[1])
packed = (root / "corpus.json.gz").read_bytes()
raw = gzip.decompress(packed)
files = dict(json.loads(raw)["files"])
hashes = dict(line.split("  ", 1)[::-1] for line in (root / "SHA256SUMS").read_text().splitlines())
for name, data in [("corpus.json.gz", packed), ("corpus.json", raw)] + [(k, v.encode()) for k, v in files.items()]:
    assert hashlib.sha256(data).hexdigest() == hashes[name], name


def resolve(value):
    if isinstance(value, dict):
        if list(value) == ["blob"]:
            return resolve(json.loads(files["blobs/" + value["blob"] + ".json"]))
        return {k: resolve(v) for k, v in value.items()}
    if isinstance(value, list):
        return [resolve(v) for v in value]
    return value


cases = []
typed = []
operations = {"inventory.parseInventory", "clocks.verifySignalBracket", "clocks.previewClocks"}
for name, content in files.items():
    if name.startswith("blobs/"):
        continue
    fixture = resolve(json.loads(content))
    events = fixture["events"]
    if fixture["scenario"] == "control restart accepts omitted or false lost and never accepts a true lost takeover":
        seen = set()
        for event in events:
            if event.get("kind") != "result" or event.get("operation") != "processes.runProcessGate.arg2.step":
                continue
            data = event.get("value", {}).get("data", {})
            decoder = "control" if "calls" in data else "drain" if "moves" in data else "move" if data.get("kind") in ("move", "takeover") else None
            identity = json.dumps(data, sort_keys=True)
            if decoder and identity not in seen:
                seen.add(identity)
                typed.append(dict(source=name, sourceSHA256=hashes[name], decoder=decoder, result=event))
    for i, event in enumerate(events):
        if event.get("kind") != "request" or event["operation"] not in operations:
            continue
        end = next((j for j in range(i + 1, len(events)) if events[j].get("id") == event["id"] and events[j].get("kind") == "result"), None)
        if end is None:
            continue
        cases.append(dict(scenario=fixture["scenario"], source=name, sourceSHA256=hashes[name],
                          request=event, result=events[end],
                          hostResults=[e for e in events[i + 1:end] if e.get("kind") == "result" and e.get("operation") == event["operation"] + ".arg0.send" and e.get("certainty") == "succeeded"]))
out = dict(schemaVersion=1, sourceManifestSHA256=hashlib.sha256((root / "SHA256SUMS").read_bytes()).hexdigest(), cases=cases)
out["typedObservations"] = typed
data = (json.dumps(out, separators=(",", ":"), sort_keys=True) + "\n").encode()
destination = Path(__file__).parent / "observations.json.gz"
packed = io.BytesIO()
with gzip.GzipFile(fileobj=packed, mode="wb", filename="", mtime=0) as archive:
    archive.write(data)
destination.write_bytes(packed.getvalue())
(destination.parent / "observations.sha256").write_text(hashlib.sha256(destination.read_bytes()).hexdigest() + "\n")
print(f"{len(cases)} function invocations; {len(typed)} typed observations; {len(data)} logical bytes")
