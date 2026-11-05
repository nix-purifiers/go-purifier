#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
"""Collect bounded lifecycle controls; any abnormal process outcome fails the run."""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

JAVA = r'''import java.lang.foreign.*;
public class ForeignLifecycleControl {
  static void mark(String value) {
    System.err.println("LIFECYCLE-STAGE " + value);
    System.err.flush();
  }
  static void exercise(String library, boolean invoke, Arena arena) throws Throwable {
    mark("library-load:start");
    var lookup = SymbolLookup.libraryLookup(library, arena);
    mark("library-load:done");
    if (invoke) {
      mark("symbol-find:start");
      var symbol = lookup.find("answer").orElseThrow();
      mark("symbol-find:done");
      mark("downcall-handle:start");
      var call = Linker.nativeLinker().downcallHandle(symbol, FunctionDescriptor.of(ValueLayout.JAVA_INT));
      mark("downcall-handle:done");
      mark("downcall:start");
      if ((int) call.invokeExact() != 42) throw new AssertionError("wrong answer");
      mark("downcall:done");
    }
  }
  public static void main(String[] args) throws Throwable {
    mark("main:start");
    if (args[2].equals("confined")) {
      try {
        try (var arena = Arena.ofConfined()) {
          try { exercise(args[0], args[1].equals("call"), arena); }
          finally { mark("arena-close:next"); }
        }
      } finally { mark("arena-resource-scope:exit"); }
    } else if (args[2].equals("process")) {
      // A separate diagnostic comparison, not a replacement for the confined case.
      exercise(args[0], args[1].equals("call"), Arena.global());
      mark("process-arena:retained");
    } else throw new IllegalArgumentException("unknown lifetime");
    mark("main:return");
  }
}
'''


class Interrupted(Exception):
    def __init__(self, signum): self.signum = signum


def stop_owned(process, grace=3):
    if process.poll() is not None: return False
    try: os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        process.wait()
        return False
    try: process.wait(timeout=grace)
    except subprocess.TimeoutExpired:
        try: os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError: pass
        process.wait()
        return True
    return False


def observe(command, directory, name, jcmd=None, deadline=45, trace_after=8, attach_timeout=8):
    """Every signalled/attached PID is a live process started here in its own group."""
    log = directory/(name+'.log')
    dump = directory/(name+'-threads.log')
    start = time.monotonic(); trace_attempted = False; timed_out = False; forced_kill = False; quit_sent = False
    with log.open('w') as stream:
        stream.write(json.dumps([str(x) for x in command])+'\n'); stream.flush()
        process = subprocess.Popen([str(x) for x in command], cwd=directory,
            stdout=stream, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            try: process.wait(timeout=trace_after if jcmd else deadline)
            except subprocess.TimeoutExpired:
                if jcmd and process.poll() is None:
                    trace_attempted = True
                    print('LIFECYCLE-CONTROL '+name+':thread-dump:start',flush=True)
                    with dump.open('w') as output:
                        attach = subprocess.Popen([str(jcmd), str(process.pid), 'Thread.print', '-l'],
                            stdout=output,stderr=subprocess.STDOUT,start_new_session=True)
                        try:
                            try: attach.wait(timeout=attach_timeout)
                            except subprocess.TimeoutExpired: stop_owned(attach,1)
                        finally: stop_owned(attach,1)
                        output.write('\nattach exit code: '+str(attach.returncode)+'\n')
                    if attach.returncode != 0 and process.poll() is None:
                        print('LIFECYCLE-CONTROL '+name+':QUIT:sent',flush=True)
                        try:
                            os.kill(process.pid,signal.SIGQUIT)
                            quit_sent = True
                        except ProcessLookupError: pass
                remaining = max(0,deadline-(time.monotonic()-start))
                try: process.wait(timeout=remaining)
                except subprocess.TimeoutExpired:
                    timed_out = True; forced_kill = stop_owned(process)
        finally:
            forced_kill = stop_owned(process) or forced_kill
    row=dict(command=[str(x) for x in command],pid=process.pid,exitCode=process.returncode,
        seconds=round(time.monotonic()-start,3),deadlineSeconds=deadline,
        threadDumpAttempted=trace_attempted,deadlineExceeded=timed_out,forcedKill=forced_kill,
        quitSent=quit_sent,
        output=log.read_text(errors='replace'),threadDump=dump.read_text(errors='replace') if dump.exists() else None)
    # An intervention or timeout is diagnostic evidence, never a clean pass.
    row['cleanExit'] = row['exitCode']==0 and not trace_attempted and not timed_out
    return row


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ['java','javac','jcmd','c-library','go-library','producer-catalog','output']:
        parser.add_argument('--'+name,required=True,type=Path)
    parser.add_argument('--java-major',required=True)
    parser.add_argument('--sdk-description',required=True)
    parser.add_argument('--include-unsupported-confined-go',action='store_true',
        help='also exercise unsupported confined Go unload; abnormal outcomes remain failures')
    args=parser.parse_args();args.output.mkdir(parents=True,exist_ok=True)
    directory=args.output.resolve()
    producer=json.loads(args.producer_catalog.read_text())
    if Path(producer['libraries']['proof']).resolve()!=args.go_library.resolve():
        raise ValueError('producer catalog does not identify the Go library being loaded')
    if producer['targets']['buildMode']!='c-shared' or not producer['targets']['go']:
        raise ValueError('producer catalog lacks the selected Go SDK/build mode')
    if producer['targets'].get('runtimeLifetime')!='process':
        raise ValueError('Go producer must declare process runtime lifetime')
    report=dict(scope='lifecycle controls; unsupported confined Go is opt-in and retains failure status',
        sdkDescription=args.sdk_description+'; producer Go '+producer['targets']['go'],
        producerCatalog=producer,includeUnsupportedConfinedGo=args.include_unsupported_confined_go,cases=[])
    def interrupted(signum,frame): raise Interrupted(signum)
    signal.signal(signal.SIGTERM,interrupted);signal.signal(signal.SIGINT,interrupted)
    exit_code=1
    try:
        (directory/'ForeignLifecycleControl.java').write_text(JAVA)
        report['javaVersion']=observe([args.java,'-version'],directory,'java-version',deadline=15)
        report['compile']=observe([args.javac,'--enable-preview','--release',args.java_major,
            'ForeignLifecycleControl.java'],directory,'compile',deadline=30)
        if not report['javaVersion']['cleanExit'] or not report['compile']['cleanExit']:
            return 1
        for kind,library in [('c',args.c_library),('go',args.go_library)]:
            for mode in ['load','call']:
                for lifetime in ['confined','process']:
                    if kind=='go' and lifetime=='confined' and not args.include_unsupported_confined_go:
                        continue
                    name=kind+'-'+mode+'-'+lifetime
                    print('LIFECYCLE-CONTROL '+name+':start',flush=True)
                    row=observe([args.java,'--enable-preview','--enable-native-access=ALL-UNNAMED',
                        'ForeignLifecycleControl',library,mode,lifetime],directory,name,jcmd=args.jcmd)
                    row.update(libraryKind=kind,library=str(library),operation=mode,lifetime=lifetime)
                    report['cases'].append(row)
                    print('LIFECYCLE-CONTROL '+name+':exit:'+str(row['exitCode']),flush=True)
                    print(row['output'],flush=True)
                    if row['threadDump']: print(row['threadDump'],flush=True)
        exit_code=0 if all(row['cleanExit'] for row in report['cases']) else 1
        return exit_code
    except Interrupted as error:
        report['interruptedBySignal']=error.signum
        return 128+error.signum
    finally:
        report['cleanRun']=exit_code==0
        (directory/'report.json').write_text(json.dumps(report,indent=2)+'\n')
        print('LIFECYCLE-REPORT '+json.dumps(report,separators=(',',':')),flush=True)


if __name__ == '__main__': raise SystemExit(main())
