"""Trusted offline builder for the example's exclusively owned dependency path.

SPDX-License-Identifier: Apache-2.0
"""
from pathlib import Path
import shutil
import subprocess
import sys


def main():
    target = Path(".deps")
    if target.is_symlink():
        raise ValueError("dependency directory must not be a symlink")
    if target.exists():
        shutil.rmtree(target)  # A failed cleanup must abort the installation.
    subprocess.run([
        sys.executable, "-m", "pip", "install", "--no-index", "--find-links=/opt/wheels",
        "--require-hashes", "--no-compile", "--no-cache-dir", "--target=.deps",
        "-r", "requirements.lock",
    ], check=True)


if __name__ == "__main__":
    main()
