# Durable CPA customization maintenance

Last verified: 2026-08-25

This document is the operating contract for keeping the Cursor quota integration, the
maintained Copilot quota bridge, and the custom management page across CLIProxyAPI upgrades.
It deliberately uses CPA's native update paths instead of a second installer.

## Maintained components

| Component | Maintained source | Release contract |
| --- | --- | --- |
| Cursor plugin | [yaminyassin/cliproxy-cursor-plugin](https://github.com/yaminyassin/cliproxy-cursor-plugin) | Numeric tag such as `v0.2.0`, platform zip, `checksums.txt` |
| Copilot plugin | [yaminyassin/cliproxyapi-copilot-plugin](https://github.com/yaminyassin/cliproxyapi-copilot-plugin) | Numeric tag such as `v0.3.5`, platform zip, `checksums.txt` |
| Management Center | [yaminyassin/Cli-Proxy-API-Management-Center](https://github.com/yaminyassin/Cli-Proxy-API-Management-Center) | Published `management.html`; optional `checksums.txt` for human verification |
| Custom plugin registry | [registry.json](../registry.json) | CPA schema version 1; both maintained plugin repositories |

The Cursor quota RPC itself is private and may change independently. Its field mapping and
revalidation procedure stay in
[cursor-quota-management-research.md](cursor-quota-management-research.md).

## Why this persists

CPA v7.2.141 implements the storage and verification behavior we need:

1. The plugin store downloads the exact GitHub release selected in the management portal,
   verifies its entry in `checksums.txt`, and rejects invalid zip layouts. See CPA
   [`install.go`](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.141/internal/pluginstore/install.go)
   and the official
   [plugin store release contract](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements).
2. CPA writes store installations as versioned files under
   `<plugins.dir>/<goos>/<goarch>/<plugin-id>-v<version>.<extension>`. It also records the
   source URL, release tag, and version under the plugin's `store` config. On restart, that
   exact version wins over an older unversioned file.
3. The plugin directory and auth directory are configured outside the Homebrew formula, so
   a normal formula upgrade does not own those files.
4. CPA's panel updater reads the latest published `management.html` from
   `remote-management.panel-github-repository`, verifies GitHub's SHA-256 asset digest when
   present, and writes it atomically. See CPA
   [`updater.go`](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.141/internal/managementasset/updater.go).

The durable boundary is therefore:

- Homebrew owns the CPA executable.
- CPA config owns the selected plugin source and version.
- CPA's plugin store owns the versioned plugin files.
- The maintained Management Center release owns `management.html`.
- Cursor and GitHub credentials remain in CPA's auth directory and never enter these repos.

## One-time migration for the maintained CPA instance

Do this only after all three GitHub releases are published and their Actions runs are green.

1. Make a local backup of the CPA config, auth directory, plugin directory, and current
   `management.html`. Store it outside the Homebrew prefix and keep it private because the
   config and auth files contain credentials.
2. Add the registry URL without removing other configured sources:

   ```yaml
   plugins:
     enabled: true
     store-sources:
       - "https://raw.githubusercontent.com/yaminyassin/cliproxy-cursor-plugin/main/registry.json"
   ```

3. Point the panel updater at the maintained fork and enable verified updates:

   ```yaml
   remote-management:
     disable-auto-update-panel: false
     panel-github-repository: "https://github.com/yaminyassin/Cli-Proxy-API-Management-Center"
   ```

4. Restart CPA once so it reads the new store source and panel settings.
5. In **Plugin Store**, install the required Cursor release from the custom source. Install
   the Copilot release from the custom source, not the duplicate official entry.
6. Restart CPA again. Native libraries can remain loaded after an in-place file update, so a
   full process restart is the reliable activation boundary.

CPA will add a `store` block under each plugin config. Keep that block when editing plugin
settings. It is the version and source lock used on future starts.

## Release procedure

### Cursor

1. Update code, tests, the default `version` value, and `registry.json` if the displayed
   fallback version changes.
2. Run `go test ./...`, `./scripts/validate-registry.sh`, and
   `make package VERSION=<version>`.
3. Push a dotted numeric tag such as `v0.2.1`.
4. Confirm the release contains:

   ```text
   cursor_<version>_darwin_arm64.zip
   cursor_<version>_linux_amd64.zip
   checksums.txt
   ```

### Copilot

1. Update code, tests, and `internal/provider.PluginVersion`.
2. Run `go test ./...`. Run the native Darwin package check on macOS and leave the
   Bookworm Linux package check to CI when Docker is unavailable locally.
3. Push a dotted numeric tag such as `v0.3.6`.
4. Confirm the release contains Darwin ARM64 and Linux AMD64 zips plus one combined
   `checksums.txt`.

### Management Center

1. Rebase the maintained changes onto the desired upstream Management Center release.
2. Run `bun run verify`.
3. Push a maintained release tag, for example `v1.22.6-yamin.2`.
4. Confirm the release is published, is not marked as a prerelease, and contains
   `management.html`. CPA's `/releases/latest` request ignores draft and prerelease builds.

Do not update the registry for every GitHub-release plugin version. CPA treats the latest
published release as the current store version. The registry `version` field is only a
display fallback when GitHub cannot be queried.

## CPA upgrade procedure

1. Read the CPA release notes and compare plugin ABI or Management API changes against the
   Cursor research record.
2. Back up the same four local surfaces listed in the migration section.
3. Upgrade CPA through Homebrew.
4. Confirm the config still contains the custom registry URL, both plugin `store` locks, and
   the maintained panel repository.
5. Restart CPA and verify the registered versions for `cursor` and
   `cliproxyapi-copilot`.
6. Open the management page and validate both quota cards with real credentials. A green
   build or successful plugin registration does not prove the private Cursor usage RPC still
   works.

If the plugins fail to register against a new CPA build, keep the new CPA upgrade out of the
daily-driver path until the plugins are rebuilt and verified against that CPA SDK version.

## Rollback

- **Plugin:** choose the prior published version in Plugin Store, install it from the same
  custom source, and restart CPA. The new store manifest selects that versioned file; the
  other version can remain on disk until cleanup is deliberate.
- **Management Center:** the panel updater follows the fork's latest published release. The
  safest rollback is a forward release built from the last known-good commit with a new tag.
  For an emergency local pin, set `disable-auto-update-panel: true`, restore the backed-up
  `management.html`, and restart CPA.
- **CPA executable:** Homebrew rollback is outside this customization contract. Preserve the
  pre-upgrade binary or formula separately if a same-minute binary rollback is required.

Never commit backups, management keys, API keys, Cursor tokens, Copilot tokens, or auth
files. Use `[REDACTED_SECRET]` in diagnostics and documentation.

## Validation checklist

- GitHub release asset names match CPA's exact `<id>_<version>_<goos>_<goarch>.zip` rule.
- Each zip has one root-level dynamic library named for the plugin ID.
- `checksums.txt` covers every release zip.
- Registry entries point at the maintained repositories.
- CPA config retains the custom registry URL and store manifests.
- CPA logs show both plugins registered at the selected versions after a full restart.
- Cursor and Copilot quota cards load without exposing access or refresh tokens to the
  browser response.
- The Cursor private RPC and response mapping pass the living revalidation runbook.
