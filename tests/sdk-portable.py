#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
"""Copy only a declared SDK reference closure, then evaluate it in a cold store."""
import json
from pathlib import Path
import subprocess
import tempfile


def output(*args):
    return subprocess.check_output(args, text=True).strip()


carrier = output('nix', 'build', '--impure', '--no-link', '--print-out-paths',
                 '--expr', '(import ./tests/sdk-runtime.nix {}).carrier')
data = json.loads(output('nix', 'eval', '--impure', '--json', '--expr',
    'let d=import ./tests/sdk-runtime.nix {};in {inherit(d) descriptor expectedOutput stringReference runtimeShell;}'))
# Check the real double-quoted-shell representation, including nested JSON
# quoting for a contextual SDK string, rather than a hand-written decoder.
reference = data['stringReference']
roundtrip = output(data['runtimeShell'], '-c', 'value="' + reference['shell'] + '"; printf "%s" "$value"')
assert roundtrip == reference['expression'], 'SDK shell serialization changed the Nix expression'
with tempfile.TemporaryDirectory(prefix='sdk-runtime-copy-') as temporary:
    # Nix refuses a store under a symlink, such as macOS's /var.
    root = Path(temporary).resolve()
    cache = 'file://' + str(root / 'cache')
    cold = 'local?root=' + str(root / 'cold')
    subprocess.run(['nix', 'copy', '--to', cache, carrier], check=True)
    subprocess.run(['nix', 'copy', '--from', cache, '--to', cold, '--no-check-sigs', carrier], check=True)
    physical = root / 'cold/nix/store'
    assert (physical / Path(data['descriptor']).name).is_file(), 'runtime closure lost the SDK recipe'
    assert not (physical / Path(data['expectedOutput']).name).exists(), 'SDK output was unexpectedly built/copied'
    expression = 'let sdk = ' + Path(carrier).read_text() + '; in {path=toString sdk; selected=sdk.outputName;}'
    restored = json.loads(output('nix', 'eval', '--store', cold, '--offline', '--impure',
                                '--option', 'allow-import-from-derivation', 'false', '--json', '--expr', expression))
    assert restored == {'path': data['expectedOutput'], 'selected': 'secondary'}, restored
print('PASS SDK recipe retained by ordinary copy; cold-store restore needs no SDK build or IFD')
