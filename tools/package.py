import argparse
import base64
import hashlib
import json
import zipfile
from pathlib import Path

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey

PUBLIC_KEY = "LPYLHgoVkZ4sCI1jDDlYXPkv8bErFM9mKFpN5/ngWjY="
PLUGIN_ID = "local.personal.openai-account-health"
KEY_ID = "personal-astra-transport-20260921"
ORIGINAL_HASH = "61ebe7d84f1f710e215f827eaddfc18c1058ade0715b92694e741f9284f277de"


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def encoded_json(value):
    return (json.dumps(value, ensure_ascii=True, indent=2, sort_keys=True) + "\n").encode()


def write_zip(path, files):
    temporary = path.with_name(path.name + ".tmp")
    with zipfile.ZipFile(temporary, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for name, raw in sorted(files.items()):
            info = zipfile.ZipInfo(name, (1980, 1, 1, 0, 0, 0))
            info.create_system = 3
            info.external_attr = (0o100755 if name.startswith("runtimes/") else 0o100644) << 16
            archive.writestr(info, raw, compress_type=zipfile.ZIP_DEFLATED, compresslevel=9)
    temporary.replace(path)


def verify(path):
    with zipfile.ZipFile(path) as archive:
        manifest_raw = archive.read("manifest.json")
        manifest = json.loads(manifest_raw)
        signature = json.loads(archive.read("signature.json"))
        if signature["algorithm"] != "ed25519" or signature["key_id"] != KEY_ID:
            raise ValueError("unexpected publisher")
        Ed25519PublicKey.from_public_bytes(base64.b64decode(PUBLIC_KEY)).verify(base64.b64decode(signature["signature"]), manifest_raw)
        expected_files = set(manifest["files"]) | {"manifest.json", "signature.json"}
        if set(archive.namelist()) != expected_files:
            raise ValueError("manifest file set mismatch")
        for name, expected in manifest["files"].items():
            if digest(archive.read(name)) != expected:
                raise ValueError("file hash mismatch: " + name)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--child", required=True, type=Path)
    parser.add_argument("--key", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    source = Path(__file__).resolve().parents[1]
    metadata = json.loads((source / "metadata.json").read_text(encoding="utf-8"))
    child = args.child.read_bytes()
    if digest(child) != ORIGINAL_HASH:
        raise ValueError("unexpected original transport hash")
    binary = args.binary.read_bytes()
    if binary[:5] != b"\x7fELF\x02" or binary[18:20] != b"\x3e\x00":
        raise ValueError("runtime must be Linux amd64 ELF")
    files = {
        "runtimes/linux-amd64/openai-account-health": binary,
        "runtimes/linux-amd64/openai-transport-original": child,
        "ui/index.html": (source / "ui/index.html").read_bytes(),
        "ui/assets/bridge-v1.js": (source / "ui/assets/bridge-v1.js").read_bytes(),
        "ui/assets/health.js": (source / "ui/assets/health.js").read_bytes(),
        "ui/assets/health.css": (source / "ui/assets/health.css").read_bytes(),
        "ui/assets/styles.css": (source / "ui/assets/styles.css").read_bytes(),
        "provenance/NOTICE.md": (source / "NOTICE.md").read_bytes(),
        "provenance/modeltrace_LICENSE": (source / "internal/modeltrace/modeltrace_LICENSE").read_bytes(),
        "provenance/codex-tool-catalog.json": (source / "provenance/codex-tool-catalog.json").read_bytes(),
        "provenance/openai-codex-LICENSE.txt": (source / "provenance/openai-codex-LICENSE.txt").read_bytes(),
    }
    source_hashes = {}
    for folder in ["cmd", "internal", "tools", "ui"]:
        for path in sorted((source / folder).rglob("*")):
            if path.is_file() and "__pycache__" not in path.parts:
                source_hashes[path.relative_to(source).as_posix()] = digest(path.read_bytes())
    files["provenance/source-manifest.json"] = encoded_json({"version": metadata["version"], "runtime": "linux-amd64", "source_sha256": source_hashes, "child_sha256": ORIGINAL_HASH})
    manifest = {
        "schema_version": 1,
        "id": PLUGIN_ID,
        "name": metadata["name"],
        "version": metadata["version"],
        "description": metadata["description"],
        "author": metadata["author"],
        "requires": {"sub2api": ">=0.2.7 <0.3.0", "recommended_sub2api_version": "0.2.7", "tested_sub2api_versions": ["0.2.7"], "plugin_protocol": 1, "transport_api": 1, "ui_bridge": 1},
        "capabilities": [{"id": "openai.oauth.outbound_transport.v1", "platform": "openai", "account_type": "oauth"}],
        "runtimes": {"linux-amd64": {"path": "runtimes/linux-amd64/openai-account-health"}},
        "ui": {"entrypoint": "ui/index.html"},
        "files": {name: digest(value) for name, value in sorted(files.items())},
    }
    key = serialization.load_pem_private_key(args.key.read_bytes(), password=None)
    if not isinstance(key, Ed25519PrivateKey):
        raise ValueError("signing key must be Ed25519")
    public = base64.b64encode(key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)).decode()
    if public != PUBLIC_KEY:
        raise ValueError("use the existing publisher key")
    manifest_raw = encoded_json(manifest)
    signature = {"algorithm": "ed25519", "key_id": KEY_ID, "signature": base64.b64encode(key.sign(manifest_raw)).decode()}
    files["manifest.json"] = manifest_raw
    files["signature.json"] = encoded_json(signature)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    write_zip(args.output, files)
    verify(args.output)
    publisher = args.output.parent / "trusted-publisher.yaml"
    publisher.write_text(f"plugins:\n  trusted_publishers:\n    {KEY_ID}: \"{public}\"\n", encoding="utf-8")
    print(json.dumps({"output": str(args.output), "sha256": digest(args.output.read_bytes()), "signature_verified": True, "files_verified": len(manifest["files"])}, ensure_ascii=True))


if __name__ == "__main__":
    main()
