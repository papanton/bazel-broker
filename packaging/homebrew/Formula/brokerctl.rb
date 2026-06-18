# Homebrew formula for the brokerctl CLI (prebuilt universal binary).
# Copy into your tap repo (homebrew-tap/Formula/) and fill in OWNER + sha256.
# Install:  brew install papanton/tap/brokerctl
class Brokerctl < Formula
  desc "CLI for the Bazel Broker daemon (ls / watch / kill / drain / profile)"
  homepage "https://github.com/papanton/bazel-broker"
  version "0.1.1"
  url "https://github.com/papanton/bazel-broker/releases/download/v#{version}/brokerctl-#{version}.tar.gz"
  sha256 "5ccd5ce4cb7e87caf8bd38a9bfbe9d336b1c7d22df30aa17c5874445bd3a02f2"

  def install
    bin.install "brokerctl"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/brokerctl version")
  end
end
