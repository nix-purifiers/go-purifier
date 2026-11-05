#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
"""Exercise owned-process deadline/trace handling in the actual Nix runtime."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ['controller','java','javac','jcmd','output']:
        parser.add_argument('--'+name,required=True,type=Path)
    args=parser.parse_args();args.output.mkdir(parents=True,exist_ok=True)
    spec=importlib.util.spec_from_file_location('controls',args.controller)
    controls=importlib.util.module_from_spec(spec);spec.loader.exec_module(controls)
    with tempfile.TemporaryDirectory(prefix='ffi-controller-check-') as temporary:
        root=Path(temporary)
        (root/'Hold.java').write_text('public class Hold { public static void main(String[] a) throws Exception { System.err.println("OWNED-JAVA-PID:"+ProcessHandle.current().pid()); Thread.sleep(60000); } }')
        subprocess.run([str(args.javac),'Hold.java'],cwd=root,check=True,timeout=20)
        held=controls.observe([args.java,'Hold'],root,'held-java',jcmd=args.jcmd,
            deadline=12,trace_after=1,attach_timeout=8)
        pid_line=next(l for l in held['output'].splitlines() if l.startswith('OWNED-JAVA-PID:'))
        assert int(pid_line.split(':')[1])==held['pid'],'JVM PID is not the owned subprocess PID'
        assert held['deadlineExceeded'] and held['exitCode']!=0 and not held['cleanExit']
        assert held['threadDumpAttempted']
        trace_available='Hold.main' in (held['threadDump'] or '')+held['output']
        ignored=controls.observe([sys.executable,'-c',
            'import os,signal,time; signal.signal(signal.SIGTERM,signal.SIG_IGN); print("OWNED-PYTHON-PID:"+str(os.getpid()),flush=True); time.sleep(60)'],
            root,'ignores-term',deadline=0.3)
        pid_line=next(l for l in ignored['output'].splitlines() if l.startswith('OWNED-PYTHON-PID:'))
        assert int(pid_line.split(':')[1])==ignored['pid'],'Python PID is not the owned subprocess PID'
        assert ignored['forcedKill'] and ignored['exitCode']==-9 and not ignored['cleanExit']
        assert ignored['seconds']<6
        for row in [held,ignored]:
            try: os.waitpid(row['pid'],os.WNOHANG)
            except ChildProcessError: pass
            else: raise AssertionError('owned subprocess was not reaped')
        report=dict(scope='controller failure handling, not SDK compatibility',javaHang=held,
            ignoredTerm=ignored,jvmStackAvailable=trace_available)
        (args.output/'report.json').write_text(json.dumps(report,indent=2)+'\n')
        print('CONTROLLER-CHECK '+json.dumps(dict(ownedPidsVerified=True,timeoutsFailed=True,
            childrenReaped=True,jvmStackAvailable=trace_available)),flush=True)
        if not trace_available:
            print('Thread capture unavailable here; retain failed attach/QUIT evidence without inferring JVM state.',flush=True)


if __name__=='__main__': main()
