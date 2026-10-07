#!/usr/bin/env python3
"""Generate the Claimy Homebrew formula from archives and their checksums."""
import hashlib
import pathlib
import re
import sys

root = pathlib.Path(__file__).resolve().parent.parent
dist = root / "dist"
version = sys.argv[1] if len(sys.argv) > 1 else "0.0.0"
if version.startswith("v"):
    version = version[1:]
if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
    raise SystemExit(f"error: version must be MAJOR.MINOR.PATCH: {version}")
repo = "beeemT/claimy"
archives = sorted(dist.glob(f"claimy_{version}_*.tar.gz"))
expected = {f"claimy_{version}_{os}_{arch}.tar.gz" for os in ("darwin", "linux") for arch in ("arm64", "amd64")}
if {p.name for p in archives} != expected:
    raise SystemExit(f"error: expected exactly four archives for {version} in {dist}")

checksums = {}
for archive in archives:
    checksums[archive.name] = hashlib.sha256(archive.read_bytes()).hexdigest()
(dist / "checksums.txt").write_text("".join(f"{checksums[name]}  {name}\n" for name in sorted(checksums)), encoding="utf-8")

def sha(os, arch):
    return checksums[f"claimy_{version}_{os}_{arch}.tar.gz"]

formula = f'''class Claimy < Formula
  desc "CLI for advisory environment claims"
  homepage "https://github.com/{repo}"
  version "{version}"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/{repo}/releases/download/v#{{version}}/claimy_#{{version}}_darwin_arm64.tar.gz"
      sha256 "{sha('darwin', 'arm64')}"
    end
    on_intel do
      url "https://github.com/{repo}/releases/download/v#{{version}}/claimy_#{{version}}_darwin_amd64.tar.gz"
      sha256 "{sha('darwin', 'amd64')}"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/{repo}/releases/download/v#{{version}}/claimy_#{{version}}_linux_arm64.tar.gz"
      sha256 "{sha('linux', 'arm64')}"
    end
    on_intel do
      url "https://github.com/{repo}/releases/download/v#{{version}}/claimy_#{{version}}_linux_amd64.tar.gz"
      sha256 "{sha('linux', 'amd64')}"
    end
  end

  def install
    bin.install "claimy"
    pkgshare.install "SKILL.md"
  end

  test do
    shell_output "#{{bin}}/claimy acquire --group test --environments invalid --request-id test 2>&1", 2
  end
end
'''
(dist / "claimy.rb").write_text(formula, encoding="utf-8")
print(f"wrote {dist / 'claimy.rb'} and {dist / 'checksums.txt'}")
