#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
"""Host proof: source pins, unchanged manifests, wrapper, and pure consumer."""
from contextlib import ExitStack
from functools import partial
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import zipfile

sdk = Path(__file__).resolve().parents[1]
toolchain = Path(sys.argv[1]).resolve()

def run(*args, cwd=None, env=None):
    return subprocess.check_output([str(arg) for arg in args], cwd=cwd, env=env, text=True)

snapshot = json.loads(run('nix', 'flake', 'archive', '--json', '--no-write-lock-file', sdk))['path']
system = run('nix', 'eval', '--impure', '--raw', '--expr', 'builtins.currentSystem').strip()
with tempfile.TemporaryDirectory(prefix='go-composition-') as temporary, ExitStack() as cleanup:
    root = Path(temporary)
    producer = root / 'producer'
    consumer = root / 'consumer'
    producer.mkdir()
    consumer.mkdir()
    (producer / 'flake.nix').write_text('''{
      inputs.sdk.url = "path:SDK";
      outputs = { sdk, ... }: let
        pkgs = import sdk.inputs.nixpkgs {
          system = "SYSTEM";
          overlays = [ sdk.overlays.default ];
        };
        project = pkgs.go-purifier.buildGoProject {
          src = sdk.outPath + "/tests/fixtures/replace";
          modRoot = "app";
        };
      in { packages.SYSTEM.default = project.all; checks.SYSTEM.proxy = project.proxy; };
    }'''.replace('SDK', snapshot).replace('SYSTEM', system))
    run('nix', 'flake', 'lock', 'path:' + str(producer), cwd=producer)
    (consumer / 'go.mod').write_text('module example.com/consumer\n\ngo 1.24\nrequire example.com/greeter v0.99.0\n')
    (consumer / 'main.go').write_text('package main\nimport ("fmt"; "example.com/greeter")\nfunc main(){fmt.Println(greeter.Greeting("Nix"))}\n')
    original = (consumer / 'go.mod').read_bytes()
    proxy_build = json.loads(run('nix', 'build', '--json', '--no-link',
                                 'path:' + str(producer) + '#checks.' + system + '.proxy'))
    ordinary_proxy = root / 'ordinary-proxy'
    shutil.copytree(proxy_build[0]['outputs']['out'], ordinary_proxy)
    ordinary_proxy.chmod(0o755)
    for copied in ordinary_proxy.rglob('*'):
        copied.chmod(0o755 if copied.is_dir() else 0o644)
    uuid_proxy = ordinary_proxy / 'github.com/google/uuid/@v'
    shutil.copyfile(uuid_proxy / 'v1.6.0.mod', uuid_proxy / 'v1.6.1.mod')
    (uuid_proxy / 'v1.6.0.info').write_text(json.dumps({'Version': 'v1.6.0', 'Time': '2020-01-01T00:00:00Z'}))
    (uuid_proxy / 'v1.6.1.info').write_text(json.dumps({'Version': 'v1.6.1', 'Time': '2020-01-01T00:00:00Z'}))
    (uuid_proxy / 'list').write_text('v1.6.0\nv1.6.1\n')
    with zipfile.ZipFile(uuid_proxy / 'v1.6.0.zip') as source_zip:
        with zipfile.ZipFile(uuid_proxy / 'v1.6.1.zip', 'w', compression=zipfile.ZIP_DEFLATED) as target_zip:
            for entry in source_zip.infolist():
                target_zip.writestr(entry.filename.replace('@v1.6.0/', '@v1.6.1/'), source_zip.read(entry))
    module_dir = ordinary_proxy / 'example.com/greeter/@v'
    module_dir.mkdir(parents=True)
    regular_mod = 'module example.com/greeter\n\ngo 1.24\n'
    (module_dir / 'v0.99.0.mod').write_text(regular_mod)
    (module_dir / 'v0.99.0.info').write_text(json.dumps({'Version': 'v0.99.0', 'Time': '2020-01-01T00:00:00Z'}))
    (module_dir / 'list').write_text('v0.99.0\n')
    with zipfile.ZipFile(module_dir / 'v0.99.0.zip', 'w', compression=zipfile.ZIP_DEFLATED) as archive:
        archive.writestr('example.com/greeter@v0.99.0/go.mod', regular_mod)
        archive.writestr('example.com/greeter@v0.99.0/greeter.go',
                         'package greeter\nfunc Greeting(name string) string {return "regular " + name}\n')
    denied = []
    deny_requests = False
    class ProxyHandler(SimpleHTTPRequestHandler):
        def do_GET(self):
            if deny_requests:
                denied.append(self.path)
                self.send_error(503, 'network denied by composition test')
                return
            super().do_GET()
        def log_message(self, *args):
            pass
    server = ThreadingHTTPServer(('127.0.0.1', 0), partial(ProxyHandler, directory=str(ordinary_proxy)))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    cleanup.callback(server.server_close)
    cleanup.callback(server.shutdown)
    env = dict(os.environ, PATH=str(toolchain / 'bin') + ':' + os.environ['PATH'],
               GOPROXY='http://127.0.0.1:' + str(server.server_port), GOSUMDB='off',
               GOMODCACHE=str(root / 'module-cache'))
    caches = [root / 'module-cache', root / 'fresh-cache']
    for cache in caches:
        cleanup.callback(subprocess.run, [str(toolchain / 'bin/go'), 'clean', '-modcache'],
                         env=dict(env, GO_PURIFIER='0', GOWORK='off', GOMODCACHE=str(cache)), check=True)
    run(toolchain / 'bin/go-purifier', 'lock', cwd=consumer, env=env)
    assert run(toolchain / 'bin/go', 'run', '.', cwd=consumer, env=env) == 'regular Nix\n'
    assert 'example.com/greeter' in (consumer / 'go.sum').read_text()
    original = (consumer / 'go.mod').read_bytes()
    run(toolchain / 'bin/go-purifier', 'lock', '--input', 'path:' + str(producer), cwd=consumer, env=env)
    lock_path = consumer / 'go-purifier.lock.json'
    first = lock_path.read_bytes()
    lock = json.loads(first)
    assert not (consumer / 'go.sum').exists(), 'provided checksums retained'
    assert len(lock['projectInputs']) == 1
    assert lock['projectInputs'][0]['narHash']
    assert lock['files'] and all('example.com' not in key for key in lock['files']), lock
    assert not lock['checkouts']
    run(toolchain / 'bin/go-purifier', 'lock', cwd=consumer, env=env)
    assert lock_path.read_bytes() == first, 'relocking moved a pinned input'
    assert (consumer / 'go.mod').read_bytes() == original, ((consumer / 'go.mod').read_bytes(), original)
    expected = 'Hello, Nix (request 00000000-0000-0000-0000-000000000000)\n'
    assert run(toolchain / 'bin/go', 'run', '.', cwd=consumer, env=env) == expected
    run(toolchain / 'bin/go-purifier', 'prefetch', cwd=consumer, env=env)
    deny_requests = True
    try:
        assert run(toolchain / 'bin/go', 'run', '.', cwd=consumer,
                   env=dict(env, GOMODCACHE=str(root / 'fresh-cache'))) == expected
        assert not denied, denied
    finally:
        deny_requests = False
    assert run(toolchain / 'bin/go', 'list', '-f', '{{.Dir}}', 'example.com/greeter', cwd=consumer, env=env).strip().startswith('/nix/store/')
    print('PASS go.composition.empty-cache-no-network')

    (consumer / 'go.mod').write_text((consumer / 'go.mod').read_text() + '// working-tree edit\n')
    run(toolchain / 'bin/go', 'mod', 'tidy', cwd=consumer, env=env)
    assert 'v0.99.0' in (consumer / 'go.mod').read_text()
    assert 'replace' not in (consumer / 'go.mod').read_text()
    assert 'example.com' not in (consumer / 'go.sum').read_text()
    run(toolchain / 'bin/go-purifier', 'lock', cwd=consumer, env=env)
    run(toolchain / 'bin/go-purifier', 'update', 'github.com/google/uuid@v1.6.1', cwd=consumer, env=env)
    assert 'v0.99.0' in (consumer / 'go.mod').read_text()
    assert run(toolchain / 'bin/go', 'list', '-m', 'github.com/google/uuid', cwd=consumer, env=env).strip().endswith(' v1.6.1')
    print('PASS go.transition.dependency-bump')
    sibling = consumer / 'sibling'
    sibling.mkdir()
    (sibling / 'go.mod').write_text('module example.com/sibling\n\ngo 1.24\nrequire example.com/greeter v0.99.0\n')
    (sibling / 'main.go').write_text((consumer / 'main.go').read_text())
    (consumer / 'go.work').write_text('go 1.24\nuse (\n .\n ./sibling\n)\n')
    original_work = (consumer / 'go.work').read_bytes()
    run(toolchain / 'bin/go-purifier', 'lock', cwd=consumer, env=env)
    assert run(toolchain / 'bin/go', 'run', '.', cwd=sibling, env=env) == expected
    assert (consumer / 'go.work').read_bytes() == original_work
    run(toolchain / 'bin/go-purifier', 'update', 'github.com/google/uuid@v1.6.1', cwd=sibling, env=env)
    assert (consumer / 'go.work').read_bytes() == original_work
    original = (consumer / 'go.mod').read_bytes()
    (consumer / 'flake.nix').write_text('''{
      inputs.sdk.url = "path:SDK";
      outputs = { sdk, ... }: let
        pkgs = import sdk.inputs.nixpkgs {
          system = "SYSTEM";
          overlays = [ sdk.overlays.default ];
        };
        project = pkgs.go-purifier.buildGoProject { src = ./.; };
      in {
        packages.SYSTEM.default = project.all;
        checks.SYSTEM.repository = project.projections.go;
        checks.SYSTEM.proxy = project.proxy;
      };
    }'''.replace('SDK', snapshot).replace('SYSTEM', system))
    run('nix', 'flake', 'lock', 'path:' + str(consumer), cwd=consumer)
    outputs = json.loads(run('nix', 'build', '--json', '--no-link', 'path:' + str(consumer)))
    built = Path(outputs[0]['outputs']['out'])
    assert run(built / 'bin/consumer') == expected
    assert run(built / 'bin/sibling') == expected
    publication = json.loads(run('nix', 'build', '--json', '--no-link',
                                'path:' + str(consumer) + '#checks.' + system + '.repository'))
    repository = Path(publication[0]['outputs']['out'])
    catalog = json.loads((repository / '.catalog').read_text())
    version = next(item['version'] for item in catalog['modules'] if item['path'] == 'example.com/consumer')
    proxy = json.loads(run('nix', 'build', '--json', '--no-link',
                          'path:' + str(consumer) + '#checks.' + system + '.proxy'))[0]['outputs']['out']
    ordinary = dict(env, GO_PURIFIER='0', GOWORK='off', GOENV='off', GOSUMDB='off',
                    GOPROXY='file://' + str(repository) + ',file://' + proxy)
    assert run(toolchain / 'bin/go', 'run', 'example.com/consumer@' + version,
               cwd=root, env=ordinary) == expected
    assert (consumer / 'go.mod').read_bytes() == original, ((consumer / 'go.mod').read_bytes(), original)
    run(toolchain / 'bin/go-purifier', 'lock', '--no-inputs', cwd=consumer, env=env)
    regular_lock = json.loads(lock_path.read_text())
    assert not regular_lock.get('projectInputs')
    assert any('example.com/greeter' in key for key in regular_lock['files'])
    assert not (consumer / '.go-purifier/input.work').exists()
    assert not list((consumer / '.go-purifier/inputs').iterdir()), 'stale Nix input GC roots'
    assert run(toolchain / 'bin/go', 'run', '.', cwd=consumer, env=env) == 'regular Nix\n'
    regular_build = json.loads(run('nix', 'build', '--json', '--no-link', 'path:' + str(consumer)))
    assert run(Path(regular_build[0]['outputs']['out']) / 'bin/consumer') == 'regular Nix\n'
    last = lock_path.read_bytes()
    run(toolchain / 'bin/go-purifier', 'lock', cwd=consumer, env=env)
    assert lock_path.read_bytes() == last, 'regular transition is not idempotent'
    print('PASS go.transition.regular-nix-regular')
    run(toolchain / 'bin/go', 'mod', 'tidy', cwd=consumer, env=env)
    run(toolchain / 'bin/go', 'mod', 'tidy', cwd=sibling, env=env)
    run(toolchain / 'bin/go-purifier', 'lock', cwd=consumer, env=env)
    assert all('github.com/google/uuid' not in key for key in json.loads(lock_path.read_text())['files'])
    print('PASS go.transition.drop-transitive')
    (consumer / 'go.work').write_text('go 1.24\nuse .\n')
    shutil.rmtree(sibling)
    run(toolchain / 'bin/go-purifier', 'lock', cwd=consumer, env=env)
    assert [binary['name'] for binary in json.loads(lock_path.read_text())['binaries']] == ['consumer']
    dropped = json.loads(run('nix', 'build', '--json', '--no-link', 'path:' + str(consumer)))
    final_output = Path(dropped[0]['outputs']['out'])
    assert run(final_output / 'bin/consumer') == 'regular Nix\n'
    assert not (final_output / 'bin/sibling').exists()
    print('PASS go.transition.add-drop-buildable')

    print('Go composition: pinned inputs, native version defeat, wrapper, pure build passed')
