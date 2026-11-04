# SPDX-License-Identifier: MIT
"""Actual pre-module native resolution, capture, offline build and consumers."""
from concurrent.futures import ThreadPoolExecutor
import functools
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading

tools = Path(sys.argv[1]).resolve()
flake = Path(__file__).resolve().parents[1]
expected_version = sys.argv[2] if len(sys.argv) > 2 else 'go1.0.3'


def command(args, cwd, env, capture=False):
    return subprocess.run(args, cwd=cwd, env=env, check=True, text=True,
                          stdout=subprocess.PIPE if capture else None).stdout


with tempfile.TemporaryDirectory(prefix='go-gopath-native-') as temporary:
    base = Path(temporary).resolve()
    env = dict(os.environ, HOME=str(base/'home'), GOPATH=str(base/'workspace'),
               LC_ALL='C', CGO_ENABLED='0', PATH=str(tools/'bin')+os.pathsep+os.environ['PATH'])
    Path(env['HOME']).mkdir()
    server_root = base/'server'
    for name, function in [('greeting', 'func Answer() int { return 42 }'),
                           ('testonly', 'func Check() bool { return true }')]:
        work = base/('working-'+name)
        work.mkdir()
        (work/'library.go').write_text('package '+name+'\n'+function+'\n')
        for args in [['git','init','--initial-branch=master'], ['git','config','user.name','Fixture'],
                     ['git','config','user.email','fixture@example.invalid'],
                     ['git','add','.'], ['git','commit','-m','native dependency']]:
            command(args, work, env)
        bare = server_root/'purifier-fixture'/name
        bare.parent.mkdir(parents=True, exist_ok=True)
        command(['git','clone','--bare',str(work),str(bare)], base, env)
        command(['git','--git-dir',str(bare),'update-server-info'], base, env)

    class Handler(SimpleHTTPRequestHandler):
        def log_message(self, *args):
            pass

    server = ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(Handler, directory=str(server_root)))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        url = 'http://127.0.0.1:'+str(server.server_port)+'/'
        env.update(GIT_CONFIG_COUNT='2', GIT_CONFIG_KEY_0='url.'+url+'.insteadOf',
                   GIT_CONFIG_VALUE_0='https://github.com/',
                   GIT_CONFIG_KEY_1='url.'+url+'.insteadOf', GIT_CONFIG_VALUE_1='git://github.com/')
        root = Path(env['GOPATH'])/'src/example.org/legacy'
        (root/'cmd/app').mkdir(parents=True)
        (root/'library.go').write_text('package legacy\nimport "github.com/purifier-fixture/greeting"\nfunc Answer() int { return greeting.Answer() }\n')
        (root/'library_test.go').write_text('package legacy\nimport ("testing"; "github.com/purifier-fixture/testonly")\nfunc TestAnswer(t *testing.T) { if Answer()!=42 || !testonly.Check() { t.Fatal("wrong answer") } }\n')
        (root/'cmd/app/main.go').write_text('package main\nimport ("fmt"; "runtime"; "example.org/legacy")\nfunc main(){fmt.Printf("%d on %s\\n",legacy.Answer(),runtime.Version())}\n')
        original = {p.relative_to(root): p.read_bytes() for p in root.rglob('*.go')}
        historical_get = expected_version == 'go1.0.3'
        if historical_get:
            command(['go','get','-d','github.com/purifier-fixture/greeting','github.com/purifier-fixture/testonly'], root, env)
        else:
            # New Go no longer fetches GOPATH dependencies with go get.
            # Its native Git checkouts remain valid inputs to graph capture.
            for name in ['greeting', 'testonly']:
                target = Path(env['GOPATH'])/'src/github.com/purifier-fixture'/name
                target.parent.mkdir(parents=True, exist_ok=True)
                command(['git','clone',url+'purifier-fixture/'+name,str(target)], root, env)
        command(['go-purifier','lock','--gopath'], root, env)
        lock = root/'go-purifier.lock.json'
        saved_lock = lock.read_bytes()
        observation = json.loads(saved_lock)
        assert observation['gopath']['goVersion'] .startswith('go version '+expected_version), observation['gopath']
        assert {a['path'] for a in observation['checkouts']} == {
            'src/github.com/purifier-fixture/greeting', 'src/github.com/purifier-fixture/testonly'}
        command(['go-purifier','lock','--gopath'], root, env)
        assert lock.read_bytes() == saved_lock, 'unchanged native graph must capture identically'
        dependency = Path(env['GOPATH'])/'src/github.com/purifier-fixture/greeting/library.go'
        dependency.write_text(dependency.read_text()+'\n// uncommitted source must not be captured\n')
        rejected = subprocess.run(['go-purifier','lock','--gopath'], cwd=root, env=env,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        assert rejected.returncode != 0 and 'uncommitted' in rejected.stdout, rejected.stdout
        assert lock.read_bytes() == saved_lock
        command(['git','checkout','--','library.go'], dependency.parent, env)
        damaged = json.loads(saved_lock)
        damaged['contentHash'] = 'sha256-'+'A'*43+'='
        lock.write_text(json.dumps(damaged))
        rejected = subprocess.run(['go','build','./cmd/app'], cwd=root, env=env,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        assert rejected.returncode != 0 and 'contentHash' in rejected.stdout, rejected.stdout
        lock.write_bytes(saved_lock)
        command(['go-purifier','prefetch'], root, env)
        # Remove the developer's original fetched sources. Locked builds must
        # read only the Nix checkout tree, including the test-only dependency.
        shutil.rmtree(Path(env['GOPATH'])/'src/github.com')
        offline = dict(env, HTTP_PROXY='http://127.0.0.1:9', HTTPS_PROXY='http://127.0.0.1:9',
                       ALL_PROXY='http://127.0.0.1:9', GIT_CONFIG_COUNT='0')
        with ThreadPoolExecutor(max_workers=8) as pool:
            list(pool.map(lambda _: command(['go','list','./...'], root, offline, True), range(24)))
        command(['go','test','./...'], root, offline)
        command(['go','build','-o','application','./cmd/app'], root, offline)
        assert command([str(root/'application')], root, offline, True).strip() == '42 on '+expected_version
        assert lock.read_bytes() == saved_lock
        for path, contents in original.items():
            assert (root/path).read_bytes() == contents

        # Build the same selected SDK and graph through the public Nix builder.
        supports_trimpath = '-trimpath' in command(['go','help','build'], root, env, True)
        build_env = dict(env, GOPATH_TEST_VARY_PATHS='yes' if supports_trimpath else 'no', GOPATH_TEST_ROOT=str(root), GOPATH_TEST_FLAKE=str(flake), GOPATH_TEST_SDK=str(tools/'nix-support/project-go-sdk.nix'))
        expression = '''let
          f = builtins.getFlake ("path:" + builtins.getEnv "GOPATH_TEST_FLAKE");
          system = builtins.currentSystem;
          pkgs = import f.inputs.nixpkgs { inherit system; overlays = [ f.overlays.default ]; };
          sdk = import (/. + builtins.getEnv "GOPATH_TEST_SDK");
          project = (pkgs.go-purifier.buildGoPathProject.override { go = sdk; }) {
            src = /. + builtins.getEnv "GOPATH_TEST_ROOT"; version = "v0.1.0";
          };
          variant = project.all.overrideAttrs (_: {
            preBuild = ''mkdir -p "$TMPDIR/another-build-root"; export TMPDIR="$TMPDIR/another-build-root"'';
          });
        in [ project.all project.release project.repository ] ++
          pkgs.lib.optional (builtins.getEnv "GOPATH_TEST_VARY_PATHS" == "yes") variant'''
        built = json.loads(command(['nix','build','--impure','--no-link','--json','--expr',expression], root, build_env, True))
        paths = [Path(output) for item in built for output in item['outputs'].values()]
        binaries = [p/'bin/app' for p in paths if (p/'bin/app').exists()]
        if supports_trimpath:
            assert len(binaries) == 2
            assert binaries[0].read_bytes() == binaries[1].read_bytes(), 'Go embedded the varying build directory'
        pure = binaries[0].parent.parent
        release = next(p for p in paths if (p/'SHA256SUMS').exists())
        repository = next(p for p in paths if (p/'src/example.org/legacy').exists())
        assert command([str(pure/'bin/app')], root, offline, True).strip() == '42 on '+expected_version

        # Ordinary downstream compilation from the prepared native GOPATH tree.
        ordinary = base/'ordinary'
        shutil.copytree(repository, ordinary, symlinks=False)
        for path in [ordinary, *ordinary.rglob('*')]:
            path.chmod(path.stat().st_mode | (0o700 if path.is_dir() else 0o600))
        consumer = ordinary/'src/example.org/consumer'
        consumer.mkdir(parents=True)
        (consumer/'main.go').write_text('package main\nimport ("fmt"; "example.org/legacy")\nfunc main(){fmt.Println(legacy.Answer())}\n')
        ordinary_env = dict(offline, GO_PURIFIER='0', GOPATH=str(ordinary), GO111MODULE='off')
        command(['go','build','-o','consumer','example.org/consumer'], consumer, ordinary_env)
        assert command([str(consumer/'consumer')], consumer, ordinary_env, True).strip() == '42'

        for args in [['git','init'], ['git','config','user.name','Fixture'],
                     ['git','config','user.email','fixture@example.invalid'],
                     ['git','add','library.go','library_test.go','cmd','go-purifier.lock.json'],
                     ['git','commit','-m','native GOPATH project']]:
            command(args, root, env)
        command(['go-purifier','publish','v0.1.0','--dry-run'], root, env)
        system = command(['nix','eval','--impure','--raw','--expr','builtins.currentSystem'], root, env, True).strip()
        archive = 'app-v0.1.0-'+system+'.tar.gz'
        assert (root/'dist/v0.1.0'/archive).read_bytes() == (release/archive).read_bytes()
        extracted = base/'release-consumer'
        extracted.mkdir()
        with tarfile.open(root/'dist/v0.1.0'/archive) as tar:
            tar.extractall(extracted, filter='data')
        assert command([str(extracted/'app')], extracted, offline, True).strip() == '42 on '+expected_version
        work = base/'working-greeting'
        (work/'library.go').write_text('package greeting\nfunc Answer() int { return 84 }\n')
        command(['git','add','.'], work, env)
        command(['git','commit','-m','next dependency revision'], work, env)
        command(['git','push',str(server_root/'purifier-fixture/greeting'),'master'], work, env)
        command(['git','--git-dir',str(server_root/'purifier-fixture/greeting'),'update-server-info'], base, env)
        if historical_get:
            command(['go','get','-u','-d','github.com/purifier-fixture/greeting','github.com/purifier-fixture/testonly'], root, env)
        else:
            for name in ['greeting', 'testonly']:
                target = Path(env['GOPATH'])/'src/github.com/purifier-fixture'/name
                target.parent.mkdir(parents=True, exist_ok=True)
                command(['git','clone',url+'purifier-fixture/'+name,str(target)], root, env)
        command(['go-purifier','lock','--gopath'], root, env)
        assert lock.read_bytes() != saved_lock, 'new native Git revision must update the capture'
        command(['go-purifier','prefetch'], root, env)
        command(['go','build','-o','updated','./cmd/app'], root, offline)
        assert command([str(root/'updated')], root, offline, True).strip() == '84 on '+expected_version
        print('PASS native dependency revision update invalidates the old GOPATH package cache', flush=True)
        print('PASS native '+expected_version+(' Go get' if historical_get else ' Git checkout')+'/capture/prefetch/offline build/test, pure build, ordinary source consumer, byte-equal release and ordinary artifact execution')
    finally:
        server.shutdown()
        server.server_close()
        thread.join()
