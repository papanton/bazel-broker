# Homebrew formula for the brokerctl CLI (prebuilt universal binary).
# Copy into your tap repo (homebrew-tap/Formula/) and fill in OWNER + sha256.
# Install:  brew install papanton/tap/brokerctl
class Brokerctl < Formula
  desc "CLI for the Bazel Broker daemon (ls / watch / kill / drain / profile)"
  homepage "https://github.com/papanton/bazel-broker"
  version "0.1.2"
  url "https://github.com/papanton/bazel-broker/releases/download/v#{version}/brokerctl-#{version}.tar.gz"
  sha256 "e97fa04535e1f3b647ca4c758d886538cc1eeb869e141c31f5bb28b5afd06554"

  def install
    bin.install "brokerctl"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/brokerctl version")
  end
end
