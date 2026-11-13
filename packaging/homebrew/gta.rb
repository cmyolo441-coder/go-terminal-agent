# Homebrew formula template for gta.
#
# The @...@ placeholders are filled from a release's checksums.txt by
#   make homebrew-formula VERSION=1.2.3
# which writes dist/gta.rb. The release workflow attaches that file to each
# GitHub release; copy it into a tap repository (Formula/gta.rb) to publish:
#   brew install <owner>/<tap>/gta
class Gta < Formula
  desc "Terminal AI coding agent"
  homepage "https://github.com/cmyolo441-coder/go-terminal-agent"
  version "@VERSION@"

  on_macos do
    on_arm do
      url "https://github.com/cmyolo441-coder/go-terminal-agent/releases/download/v@VERSION@/gta_@VERSION@_darwin_arm64.tar.gz"
      sha256 "@SHA256_DARWIN_ARM64@"
    end
    on_intel do
      url "https://github.com/cmyolo441-coder/go-terminal-agent/releases/download/v@VERSION@/gta_@VERSION@_darwin_amd64.tar.gz"
      sha256 "@SHA256_DARWIN_AMD64@"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/cmyolo441-coder/go-terminal-agent/releases/download/v@VERSION@/gta_@VERSION@_linux_arm64.tar.gz"
      sha256 "@SHA256_LINUX_ARM64@"
    end
    on_intel do
      url "https://github.com/cmyolo441-coder/go-terminal-agent/releases/download/v@VERSION@/gta_@VERSION@_linux_amd64.tar.gz"
      sha256 "@SHA256_LINUX_AMD64@"
    end
  end

  def install
    bin.install "gta"
  end

  test do
    assert_equal version.to_s, shell_output("#{bin}/gta version --short").strip
    assert_match "os/arch:", shell_output("#{bin}/gta version")
  end
end
