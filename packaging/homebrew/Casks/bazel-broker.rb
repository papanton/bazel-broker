# Homebrew cask for the Bazel Broker menu-bar app.
# Copy this into your tap repo (homebrew-tap/Casks/) and fill in OWNER + sha256.
# Install:  brew install --cask papanton/tap/bazel-broker
cask "bazel-broker" do
  version "0.1.1"
  sha256 "dc20c06d0e44ebc50f7dc1d5466eb5e0dab5efa73de40a0335ab8386f83cf67b"

  url "https://github.com/papanton/bazel-broker/releases/download/v#{version}/BrokerMenuBar-#{version}.zip"
  name "Bazel Broker"
  desc "Local control + observability for Bazel iOS builds across worktrees"
  homepage "https://github.com/papanton/bazel-broker"

  # Ad-hoc/unsigned is fine: brew strips the quarantine bit on install, so the app
  # launches without a Gatekeeper prompt. (Drop this once you notarize.)
  app "BrokerMenuBar.app"

  # Launch it right after install so it appears in the menu bar in one step. The
  # app then bootstraps the broker daemon as a LaunchAgent (persists across logins).
  postflight do
    system_command "/usr/bin/open", args: ["-a", "#{appdir}/BrokerMenuBar.app"]
  end

  # Only quit the app on uninstall/upgrade. Homebrew runs `uninstall` on EVERY
  # upgrade/reinstall too, so booting out the daemon here would stop the running
  # broker on each upgrade. The daemon is a per-user LaunchAgent the app installs
  # itself and is meant to outlive the app, so its teardown lives in `zap`
  # (`brew uninstall --zap`) instead.
  uninstall quit: "com.bazelbroker.menubar"

  zap launchctl: "com.bazelbroker.broker",
      trash:     [
        "~/Library/Application Support/BazelBroker",
        "~/Library/LaunchAgents/com.bazelbroker.broker.plist",
        "~/.config/bazel-broker",
        "~/.local/state/bazel-broker",
      ]
end
