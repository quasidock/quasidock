r"""Move OTP's subprocess pipes into the Windows AppContainer LOCAL namespace.

OTP 29.1.3 sys.c create_pipe uses \\.\pipe\erlang44_<pid>_<counter>.
Replace only its fixed prefix, preserving the string length, format arguments,
PE layout and all executable instructions. Refuse an unexpected upstream DLL.
"""
from pathlib import Path
import sys

old = b"\\\\.\\pipe\\erlang44_"
new = b"\\\\.\\pipe\\LOCAL\\er_"
assert len(old) == len(new)
vm = Path(sys.argv[1]) / "beam.smp.dll"
data = vm.read_bytes()
if data.count(old) != 1 or new in data:
    raise SystemExit("OTP VM does not contain exactly one expected subprocess pipe prefix")
vm.write_bytes(data.replace(old, new, 1))
