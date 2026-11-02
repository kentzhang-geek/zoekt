import argparse
import ctypes
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
from ctypes import wintypes

p = argparse.ArgumentParser()
p.add_argument('binary')
p.add_argument('label')
p.add_argument('--profile', action='store_true')
a = p.parse_args()
artifacts = Path('doc/benchmarks/indexing-implementation-2026-11-02').resolve()
out = Path('C:/Users/64223/.codex/visualizations/2026/11/02/01a19a51-b794-7bb0-9afe-49bdda626102') / 'ue-index-benchmark' / a.label
if out.exists():
    raise SystemExit(f'refusing existing output: {out}')
out.parent.mkdir(parents=True, exist_ok=True)
args = [str(Path(a.binary).resolve()), '-index_dir', str(out)]
if a.profile:
    args += ['-cpu_profile', str(artifacts / f'{a.label}.pprof')]
args += ['update', 'UE']
env = os.environ.copy()
env['GOMAXPROCS'] = '20'
start = time.perf_counter()
class MemoryCounters(ctypes.Structure):
    _fields_ = [('cb', wintypes.DWORD), ('PageFaultCount', wintypes.DWORD)] + [
        (name, ctypes.c_size_t) for name in ['PeakWorkingSetSize', 'WorkingSetSize',
        'QuotaPeakPagedPoolUsage', 'QuotaPagedPoolUsage', 'QuotaPeakNonPagedPoolUsage',
        'QuotaNonPagedPoolUsage', 'PagefileUsage', 'PeakPagefileUsage', 'PrivateUsage']]
get_memory = ctypes.windll.psapi.GetProcessMemoryInfo
get_memory.argtypes = [wintypes.HANDLE, ctypes.POINTER(MemoryCounters), wintypes.DWORD]
get_memory.restype = wintypes.BOOL
peak_ws = peak_private = 0
with (artifacts / f'{a.label}.log').open('w', encoding='utf-8') as log:
    process = subprocess.Popen(args, stdout=log, stderr=subprocess.STDOUT, env=env,
                               creationflags=subprocess.CREATE_NO_WINDOW)
    while True:
        memory = MemoryCounters()
        memory.cb = ctypes.sizeof(memory)
        if get_memory(int(process._handle), ctypes.byref(memory), memory.cb):
            peak_ws = max(peak_ws, memory.PeakWorkingSetSize)
            peak_private = max(peak_private, memory.PrivateUsage)
        try:
            code = process.wait(timeout=0.25)
            break
        except subprocess.TimeoutExpired:
            pass
wall = time.perf_counter() - start
times = [wintypes.FILETIME() for _ in range(4)]
get_times = ctypes.windll.kernel32.GetProcessTimes
get_times.argtypes = [wintypes.HANDLE] + [ctypes.POINTER(wintypes.FILETIME)] * 4
get_times.restype = wintypes.BOOL
if not get_times(int(process._handle), *(ctypes.byref(t) for t in times)):
    raise ctypes.WinError()
cpu = sum((t.dwHighDateTime << 32) | t.dwLowDateTime for t in times[2:]) / 1e7
result = dict(args=args, output=str(out), wall_s=wall, cpu_s=cpu,
              average_cores=cpu / wall, exit_code=code, gomaxprocs=20,
              peak_working_set_bytes=peak_ws, sampled_peak_private_bytes=peak_private,
              cache='uncontrolled; no system cache reset', output_drive=out.drive)
(artifacts / f'{a.label}.json').write_text(json.dumps(result, indent=2), encoding='utf-8')
print(json.dumps(result))
raise SystemExit(code)
