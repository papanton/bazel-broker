# Releasing

Artifacts are universal (arm64 + x86_64): the menu-bar app (bundling the daemon) and
the `brokerctl` CLI. Releases are cut **locally** for now (the hosted CI runner's Xcode
may lag the local one); the `release` workflow is `workflow_dispatch` and used once CI
Xcode is pinned.

## Cut a release (ad-hoc — works today, no Apple account)

```sh
make dist VERSION=0.1.3
gh release create v0.1.3 dist/*.zip dist/*.tar.gz --title v0.1.3 --generate-notes
```

Then bump the tap (`papanton/homebrew-tap`) — `version` + `sha256` (printed by `make dist`)
in `Casks/bazel-broker.rb` and `Formula/brokerctl.rb` — and push.

Ad-hoc apps install fine via the cask (Homebrew strips quarantine) but macOS may still
show a Gatekeeper alert / remove the app on some Macs. Notarize to fix that everywhere.

## Sign + notarize (clean download on any Mac)

Requires an **Apple Developer Program** membership and a **Developer ID Application**
certificate. No App Store, no sandbox — only Hardened Runtime (already verified to keep
all functionality: cgo/libproc discovery, `launchctl`/script spawning, kill, the daemon).

One-time, create an app-specific password at <https://appleid.apple.com> (for `notarytool`).

### Locally
```sh
export CODESIGN_ID="Developer ID Application: Your Name (TEAMID)"
# notarytool auth — either a stored profile…
xcrun notarytool store-credentials NOTARY_PROFILE_NAME \
  --apple-id "you@example.com" --team-id "TEAMID" --password "app-specific-pw"
export NOTARY_PROFILE="NOTARY_PROFILE_NAME"
# …or inline:
#   export APPLE_ID=you@example.com APPLE_TEAM_ID=TEAMID NOTARY_PASSWORD=app-specific-pw

make dist VERSION=0.1.3      # signs (Hardened Runtime) + notarizes + staples the app
```
`dist.sh` signs the app, the bundled `broker`, and the CLI, then `notarytool submit --wait`
and `stapler staple`s the app. It aborts if notarization is rejected (won't ship un-notarized).

### Via GitHub Actions (`release` workflow)
Add these repo **secrets**, then run the workflow (Actions → release → Run, enter the version):

| Secret | What |
|---|---|
| `MACOS_CERT_P12_BASE64` | `base64 -i DeveloperID.p12` of the exported cert |
| `MACOS_CERT_PASSWORD` | the `.p12` export password |
| `KEYCHAIN_PASSWORD` | any throwaway password for the temp build keychain |
| `CODESIGN_ID` | `Developer ID Application: NAME (TEAMID)` |
| `APPLE_ID` | Apple ID email |
| `APPLE_TEAM_ID` | Developer Team ID |
| `NOTARY_PASSWORD` | app-specific password |

If the secrets are absent the workflow builds ad-hoc (no signing/notarization). Once the
cert is in place, also re-enable the tag trigger in `release.yml` to release on `git tag v*`.

## After any release
Update the tap, then users get it via `brew update && brew upgrade`. A notarized release
also lets you drop the quarantine-stripping caveats from the cask comments.
