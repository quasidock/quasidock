"""Honor codegen's PYTHONPATH when using Python's isolated Windows package."""
import os
from pathlib import Path
import runpy
import sys

script = sys.argv.pop(1)
sys.path[:0] = [str(Path(script).parent)] + os.environ.get("PYTHONPATH", "").split(os.pathsep)
runpy.run_path(script, run_name="__main__")
