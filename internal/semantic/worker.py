"""Offline model2vec worker. No hub access, downloads, or generated text."""
import hashlib
import json
import os
import sqlite3
import sys

os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["HF_HUB_DISABLE_TELEMETRY"] = "1"
os.environ["TOKENIZERS_PARALLELISM"] = "false"
import numpy as np
from model2vec import StaticModel

model_path, cache_path = sys.argv[1:]
# Fingerprint actual weights/config: cache remains correct if a local model changes.
fingerprint = hashlib.sha256()
for root, directories, files in os.walk(model_path):
    directories.sort()
    for name in sorted(files):
        if not name.endswith((".json", ".safetensors")):
            continue
        path = os.path.join(root, name)
        fingerprint.update(os.path.relpath(path, model_path).encode() + b"\0")
        with open(path, "rb") as f:
            for block in iter(lambda: f.read(1024 * 1024), b""):
                fingerprint.update(block)
model_key = fingerprint.hexdigest()
model = StaticModel.from_pretrained(model_path)
os.umask(0o077)
db = sqlite3.connect(cache_path)
db.execute("CREATE TABLE IF NOT EXISTS vectors (key TEXT PRIMARY KEY, vector BLOB NOT NULL)")

def encode(texts):
    vecs = np.asarray(model.encode(texts), dtype=np.float32)
    norms = np.linalg.norm(vecs, axis=1, keepdims=True)
    return vecs / np.maximum(norms, 1e-12)

for line in sys.stdin:
    try:
        request = json.loads(line)
        texts = request["texts"]
        vectors = [None] * len(texts)
        missing = []
        for i, text in enumerate(texts):
            key = hashlib.sha256((model_key + "\0" + text).encode()).hexdigest()
            row = db.execute("SELECT vector FROM vectors WHERE key=?", (key,)).fetchone()
            if row:
                vectors[i] = np.frombuffer(row[0], dtype=np.float32)
            else:
                missing.append((i, key))
        for start in range(0, len(missing), 128):
            batch = missing[start:start + 128]
            encoded = encode([texts[i] for i, _ in batch])
            for (i, key), vector in zip(batch, encoded):
                vectors[i] = vector
                db.execute("INSERT OR REPLACE INTO vectors VALUES (?,?)", (key, vector.tobytes()))
        db.commit()
        query = encode([request["query"]])[0]
        scores = (np.vstack(vectors) @ query).tolist() if vectors else []
        print(json.dumps({"scores": scores, "indexed": len(missing)}, allow_nan=False), flush=True)
    except Exception as exc:
        db.rollback()
        print(json.dumps({"error": type(exc).__name__ + ": " + str(exc)}), flush=True)
