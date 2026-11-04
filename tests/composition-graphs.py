#!/usr/bin/env python3
"""Native MVS and root-only replacements across purifier/ordinary sandwiches."""
import argparse
from functools import partial
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import subprocess
import threading
import zipfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('toolchain', type=Path)
    parser.add_argument('--state-root', type=Path, required=True)
    parser.add_argument('--require-strict-publication', action='store_true',
                        help='fail for the explicitly reported native Go MVS source-constraint limitation')
    args = parser.parse_args()
    sdk = Path(__file__).resolve().parents[1]
    tools = args.toolchain.resolve()
    root = args.state_root.resolve()
    root.mkdir(parents=True, exist_ok=False)
    for name in ('tmp', 'cache', 'config', 'state', 'gopath', 'modules'):
        (root / name).mkdir()
    env = dict(os.environ, TMPDIR=str(root / 'tmp'), GOCACHE=str(root / 'cache/go-build'),
               GOMODCACHE=str(root / 'modules'), GOPATH=str(root / 'gopath'), GOENV='off',
               XDG_CACHE_HOME=str(root / 'cache'), XDG_CONFIG_HOME=str(root / 'config'),
               XDG_STATE_HOME=str(root / 'state'), GOSUMDB='off', GONOPROXY='', GOPRIVATE='',
               GOTOOLCHAIN='local', GOWORK='off', GOTELEMETRY='off')
    plain = dict(env, GO_PURIFIER='0')
    expected = ['native-root-replace', 'dependency-replace-ignored', 'mvs-higher-transitive',
                'mvs-retained-indirect-pin', 'mvs-explicit-downgrade', 'nix-input-lock',
                'nix-input-runtime', 'nix-input-origin', 'pure-consumer', 'ordinary-consumer',
                'ordinary-middle-root-replace', 'ordinary-middle-publication',
                'return-purifier', 'return-pure-build', 'final-ordinary-consumer',
                'ordinary-explicit-native-override', 'purifier-rebind-after-ordinary-override']
    report = {'version': 1, 'scope': 'Go native MVS and adopter sandwiches',
              'nativeSDK': str(tools), 'results': {name: {'status': 'not-run'} for name in expected}}
    def save():
        report['selectedCasesPassed'] = all(case['status'] == 'passed' for case in report['results'].values())
        report['supportedCasesPassed'] = all(case['status'] == 'passed' for name, case in report['results'].items()
                                            if name != 'ordinary-explicit-native-override')
        (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
    save()
    def passed(name):
        report['results'][name] = {'status': 'passed'}
        save()
        print('PASS go.graph.' + name, flush=True)
    def run(*arguments, cwd=None, environment=env):
        result = subprocess.run([str(value) for value in arguments], cwd=cwd, env=environment,
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        with (root / 'commands.log').open('a') as log:
            log.write(json.dumps([str(value) for value in arguments]) + '\n' + result.stdout + result.stderr + '\n')
        if result.returncode:
            raise RuntimeError(result.stderr[-5000:] + result.stdout[-2000:])
        return result.stdout.strip()
    # The SDK is a real checkout: Git filtering excludes the fixture state
    # and build-result links even when CI puts them beside the source.
    metadata = json.loads(run('nix', 'flake', 'metadata', '--json', '--no-write-lock-file', str(sdk)))
    snapshot = metadata['path']
    system = run('nix', 'eval', '--impure', '--raw', '--expr', 'builtins.currentSystem')
    report['source'] = {'narHash': metadata['locked']['narHash'], 'path': snapshot,
                        'revision': run('git', '-C', sdk, 'rev-parse', 'HEAD')}
    report['system'] = system
    save()
    proxy = root / 'proxy'
    proxy.mkdir()
    def publish(path, version, files):
        module = proxy / path / '@v'
        module.mkdir(parents=True, exist_ok=True)
        (module / (version + '.mod')).write_text(files['go.mod'])
        (module / (version + '.info')).write_text(json.dumps({'Version': version, 'Time': '2020-01-01T00:00:00Z'}))
        versions = set((module / 'list').read_text().splitlines()) if (module / 'list').exists() else set()
        (module / 'list').write_text('\n'.join(sorted(versions | {version})) + '\n')
        with zipfile.ZipFile(module / (version + '.zip'), 'w', compression=zipfile.ZIP_DEFLATED) as archive:
            for name, text in sorted(files.items()):
                info = zipfile.ZipInfo(path + '@' + version + '/' + name, (2020, 1, 1, 0, 0, 0))
                info.external_attr = 0o100644 << 16
                archive.writestr(info, text)
    def definition(path, requirements=(), replacement=''):
        return ('module ' + path + '\n\ngo 1.24\n\nrequire (\n'
                + ''.join(name + ' ' + version + '\n' for name, version in requirements) + ')\n' + replacement)
    for version, value in [('v1.0.0', '1'), ('v1.1.0', '2')]:
        publish('graph.example/leaf', version, {'go.mod': definition('graph.example/leaf'),
            'leaf.go': 'package leaf\nfunc Value() string {return "' + value + '"}\n'})
    for name, leaf in [('parent', 'v1.0.0'), ('other', 'v1.1.0')]:
        publish('graph.example/' + name, 'v1.0.0', {
            'go.mod': definition('graph.example/' + name, [('graph.example/leaf', leaf)]),
            name + '.go': 'package ' + name + '\nimport "graph.example/leaf"\nfunc Value() string {return leaf.Value()}\n'})
    fork = root / 'fork'
    fork.mkdir()
    (fork / 'go.mod').write_text(definition('graph.example/leaf'))
    (fork / 'leaf.go').write_text('package leaf\nfunc Value() string {return "fork"}\n')
    publisher_files = {
        'go.mod': definition('graph.example/published', [('graph.example/parent', 'v1.0.0')],
                             'replace graph.example/leaf => ../fork\n'),
        'lib.go': 'package published\nimport "graph.example/parent"\nfunc Value() string {return "ordinary:"+parent.Value()}\n',
        'cmd/published/main.go': 'package main\nimport("fmt"; "graph.example/published")\nfunc main(){fmt.Println(published.Value())}\n'}
    publish('graph.example/published', 'v1.0.0', publisher_files)
    publish('graph.example/nix', 'v0.99.0', {'go.mod': definition('graph.example/nix'),
        'lib.go': 'package nix\nfunc Value() string {return "REGULAR"}\n'})
    class Handler(SimpleHTTPRequestHandler):
        def log_message(self, *arguments):
            pass
    server = ThreadingHTTPServer(('127.0.0.1', 0), partial(Handler, directory=str(proxy)))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    env['GOPROXY'] = plain['GOPROXY'] = 'http://127.0.0.1:' + str(server.server_port)
    def write(directory, files):
        for name, text in files.items():
            target = directory / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text)
    def go(directory, *arguments, environment=plain):
        return run(tools / 'bin/go', *arguments, cwd=directory, environment=environment)
    def selected(directory, path, environment=plain):
        return json.loads(go(directory, 'list', '-m', '-json', path, environment=environment))
    def flake(directory):
        (directory / 'flake.nix').write_text('''{
          inputs.sdk.url = "path:SDK";
          outputs = {sdk,...}: let
            pkgs = import sdk.inputs.nixpkgs { system="SYSTEM"; overlays=[sdk.overlays.default]; };
            project = pkgs.go-purifier.buildGoProject {src=./.;};
          in {packages.SYSTEM.default=project.all;
              checks.SYSTEM.repository=project.projections.go;
              checks.SYSTEM.proxy=project.proxy;};
        }'''.replace('SDK', snapshot).replace('SYSTEM', system))
        run('nix', 'flake', 'lock', 'path:' + str(directory), cwd=directory)
    def build(directory, selector=''):
        values = json.loads(run('nix', 'build', '--json', '--no-link', 'path:' + str(directory) + selector))
        return Path(values[0]['outputs']['out'])
    def catalog(repository, path):
        return next(item['version'] for item in json.loads((repository / '.catalog').read_text())['modules']
                    if item['path'] == path)
    def stage(repository):
        # Publish protocol files, never an entire developer module cache.
        # A real ordinary registry must serve the middle publication's
        # transitive publications as well as the middle module itself.
        for source in repository.rglob('*'):
            if not source.is_file() or source.parent.name != '@v':
                continue
            target = proxy / source.relative_to(repository)
            target.parent.mkdir(parents=True, exist_ok=True)
            if source.name == 'list':
                versions = set(target.read_text().splitlines()) if target.exists() else set()
                target.write_text('\n'.join(sorted(versions | set(source.read_text().splitlines()))) + '\n')
            elif source.suffix in {'.mod', '.info', '.zip'}:
                if target.exists():
                    if source.suffix == '.info':
                        old, new = json.loads(target.read_text()), json.loads(source.read_text())
                        assert old['Version'] == new['Version'], (old, new)
                        if 'Time' in old and 'Time' in new:
                            assert old['Time'] == new['Time'], (old, new)
                    else:
                        assert target.read_bytes() == source.read_bytes(), 'conflicting ordinary publication: ' + str(target)
                else:
                    target.write_bytes(source.read_bytes())
    try:
        publisher = root / 'publisher'
        write(publisher, publisher_files)
        go(publisher, 'mod', 'tidy')
        assert go(publisher, 'run', './cmd/published') == 'ordinary:fork'
        passed('native-root-replace')
        baseline = root / 'baseline'
        write(baseline, {'go.mod': definition('graph.example/baseline', [('graph.example/published', 'v1.0.0')]),
            'main.go': 'package main\nimport("fmt"; "graph.example/published")\nfunc main(){fmt.Println(published.Value())}\n'})
        go(baseline, 'mod', 'tidy')
        assert go(baseline, 'run', '.') == 'ordinary:1'
        passed('dependency-replace-ignored')
        (baseline / 'main.go').write_text('package main\nimport("fmt"; "graph.example/published"; "graph.example/other")\nfunc main(){fmt.Println(published.Value()+"|"+other.Value())}\n')
        go(baseline, 'mod', 'edit', '-require=graph.example/other@v1.0.0')
        go(baseline, 'mod', 'tidy')
        assert go(baseline, 'run', '.') == 'ordinary:2|2'
        assert selected(baseline, 'graph.example/leaf')['Version'] == 'v1.1.0'
        passed('mvs-higher-transitive')
        (baseline / 'main.go').write_text('package main\nimport("fmt"; "graph.example/published")\nfunc main(){fmt.Println(published.Value())}\n')
        go(baseline, 'mod', 'edit', '-droprequire=graph.example/other')
        go(baseline, 'mod', 'tidy')
        assert go(baseline, 'run', '.') == 'ordinary:2'
        passed('mvs-retained-indirect-pin')
        go(baseline, 'get', 'graph.example/leaf@v1.0.0')
        assert go(baseline, 'run', '.') == 'ordinary:1'
        passed('mvs-explicit-downgrade')

        producer = root / 'nix-provider'
        write(producer, {'go.mod': definition('graph.example/nix', [('graph.example/parent', 'v1.0.0')]),
            'lib.go': 'package nix\nimport "graph.example/parent"\nfunc Value() string{return "NIX:"+parent.Value()}\n',
            'cmd/provider/main.go': 'package main\nimport("fmt"; "graph.example/nix")\nfunc main(){fmt.Println(nix.Value())}\n'})
        go(producer, 'mod', 'tidy')
        run(tools / 'bin/go-purifier', 'lock', cwd=producer)
        flake(producer)
        assert run(build(producer) / 'bin/provider') == 'NIX:1'
        reference = 'path:' + str(producer)
        consumer = root / 'consumer'
        consumer_files = {'go.mod': definition('graph.example/purified', [('graph.example/nix', 'v0.99.0'),
            ('graph.example/published', 'v1.0.0'), ('graph.example/other', 'v1.0.0')]),
            'lib/value.go': 'package value\nimport("graph.example/nix"; "graph.example/published"; "graph.example/other")\nfunc Value() string{return nix.Value()+"|"+published.Value()+"|"+other.Value()}\n',
            'cmd/consumer/main.go': 'package main\nimport("fmt"; "graph.example/purified/lib")\nfunc main(){fmt.Println(value.Value())}\n'}
        write(consumer, consumer_files)
        go(consumer, 'mod', 'tidy')
        assert go(consumer, 'run', './cmd/consumer') == 'REGULAR|ordinary:2|2'
        original = (consumer / 'go.mod').read_bytes()
        run(tools / 'bin/go-purifier', 'lock', '--input', reference, cwd=consumer)
        data = json.loads((consumer / 'go-purifier.lock.json').read_text())
        assert len(data['projectInputs']) == 1
        assert not any(path.startswith('graph.example/nix/') for path in data['files']), 'provided module entered download lock'
        assert (consumer / 'go.mod').read_bytes() == original
        passed('nix-input-lock')
        want = 'NIX:2|ordinary:2|2'
        assert go(consumer, 'run', './cmd/consumer', environment=env) == want
        passed('nix-input-runtime')
        assert go(consumer, 'list', '-f', '{{.Dir}}', 'graph.example/nix', environment=env).startswith('/nix/store/')
        passed('nix-input-origin')
        flake(consumer)
        assert run(build(consumer) / 'bin/consumer') == want
        passed('pure-consumer')
        repository = build(consumer, '#checks.' + system + '.repository')
        locked_proxy = build(consumer, '#checks.' + system + '.proxy')
        version = catalog(repository, 'graph.example/purified')
        downstream_env = dict(plain, GOPROXY='file://' + str(repository) + ',file://' + str(locked_proxy),
                              GOMODCACHE=str(root / 'ordinary-module-cache'))
        ordinary = root / 'ordinary'
        ordinary_files = {'go.mod': definition('graph.example/bridge', [('graph.example/purified', version),
            ('graph.example/leaf', 'v1.0.0')]),
            'bridge.go': 'package bridge\nimport "graph.example/purified/lib"\nfunc Value()string{return value.Value()}\n',
            'cmd/bridge/main.go': 'package main\nimport("fmt";"graph.example/bridge")\nfunc main(){fmt.Println(bridge.Value())}\n'}
        write(ordinary, ordinary_files)
        go(ordinary, 'mod', 'tidy', environment=downstream_env)
        assert go(ordinary, 'run', './cmd/bridge', environment=downstream_env) == want
        assert selected(ordinary, 'graph.example/leaf', environment=downstream_env)['Version'] == 'v1.1.0'
        passed('ordinary-consumer')
        stage(repository)
        stage(locked_proxy)
        go(ordinary, 'mod', 'edit', '-replace=graph.example/leaf=../fork', environment=downstream_env)
        assert go(ordinary, 'run', './cmd/bridge', environment=downstream_env) == 'NIX:fork|ordinary:fork|fork'
        passed('ordinary-middle-root-replace')
        publish('graph.example/bridge', 'v1.0.0', {name: path.read_text() for path in ordinary.rglob('*')
            if path.is_file() and (path.suffix == '.go' or path.name == 'go.mod')
            for name in [str(path.relative_to(ordinary))]})
        passed('ordinary-middle-publication')
        returning = root / 'returning'
        write(returning, {'go.mod': definition('graph.example/returning', [('graph.example/bridge', 'v1.0.0')]),
            'lib/value.go': 'package value\nimport "graph.example/bridge"\nfunc Value()string{return bridge.Value()}\n',
            'cmd/returning/main.go': 'package main\nimport("fmt";"graph.example/returning/lib")\nfunc main(){fmt.Println(value.Value())}\n'})
        returning_env = dict(env, GOPROXY=plain['GOPROXY'])
        go(returning, 'mod', 'tidy', environment=dict(returning_env, GO_PURIFIER='0'))
        run(tools / 'bin/go-purifier', 'lock', '--input', reference, cwd=returning, environment=returning_env)
        assert go(returning, 'run', './cmd/returning', environment=returning_env) == want
        assert go(returning, 'list', '-f', '{{.Dir}}', 'graph.example/nix', environment=returning_env).startswith('/nix/store/')
        passed('return-purifier')
        flake(returning)
        assert run(build(returning) / 'bin/returning') == want
        passed('return-pure-build')
        final_repository = build(returning, '#checks.' + system + '.repository')
        final_proxy = build(returning, '#checks.' + system + '.proxy')
        final_version = catalog(final_repository, 'graph.example/returning')
        final = root / 'final'
        write(final, {'go.mod': definition('graph.example/final', [('graph.example/returning', final_version)]),
            'main.go': 'package main\nimport("fmt";"graph.example/returning/lib")\nfunc main(){fmt.Println(value.Value())}\n'})
        stage(final_repository)
        stage(final_proxy)
        final_env = dict(plain, GOPROXY='file://' + str(final_repository) + ',file://' + str(final_proxy) + ',' + plain['GOPROXY'],
                         GOMODCACHE=str(root / 'final-module-cache'))
        go(final, 'mod', 'tidy', environment=final_env)
        assert go(final, 'run', '.', environment=final_env) == want
        passed('final-ordinary-consumer')
        go(final, 'get', 'graph.example/nix@v0.99.0', environment=final_env)
        observed = go(final, 'run', '.', environment=final_env)
        assert observed == 'REGULAR|ordinary:2|2', observed
        report['results']['ordinary-explicit-native-override'] = {
            'status': 'blocked', 'observed': observed,
            'reason': 'go.mod cannot express an exact hard source constraint; a higher ordinary requirement wins MVS. Strict ordinary-consumer source preservation is not established.'}
        save()
        print('BLOCKED go.graph.strict-ordinary-source-preservation: higher native version wins MVS', flush=True)
        rebound = dict(final_env, GO_PURIFIER='1')
        run(tools / 'bin/go-purifier', 'lock', '--input', reference, cwd=final, environment=rebound)
        assert go(final, 'run', '.', environment=rebound) == want
        assert go(final, 'list', '-f', '{{.Dir}}', 'graph.example/nix', environment=rebound).startswith('/nix/store/')
        data = json.loads((final / 'go-purifier.lock.json').read_text())
        assert not any(path.startswith('graph.example/nix/') for path in data['files'])
        passed('purifier-rebind-after-ordinary-override')
    finally:
        server.shutdown()
        server.server_close()
    return int(not report['supportedCasesPassed'] or (args.require_strict_publication and not report['selectedCasesPassed']))


if __name__ == '__main__':
    raise SystemExit(main())
