"""erlang.mk's incremental ESCRIPT_ZIP command, with canonical ZIP metadata."""
from pathlib import Path
import sys
import zipfile

target = Path(sys.argv[1])
files = {}
if target.exists():
    with zipfile.ZipFile(target) as old:
        files.update((name, old.read(name)) for name in old.namelist())
for arg in sys.argv[2:]:
    path = Path(arg)
    if path.is_file():
        files[path.as_posix()] = path.read_bytes()
target.parent.mkdir(parents=True, exist_ok=True)
with zipfile.ZipFile(target, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as out:
    for name, data in sorted(files.items()):
        info = zipfile.ZipInfo(name, (1980, 1, 1, 0, 0, 0))
        info.create_system = 3
        info.external_attr = 0o100644 << 16
        info.compress_type = zipfile.ZIP_DEFLATED
        out.writestr(info, data, compresslevel=9)
