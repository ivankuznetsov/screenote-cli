# Screenote CLI

The public command-line client for [Screenote](https://screenote.ai), a visual feedback workspace for screenshots and annotations.

## Install

On macOS, install the latest release with Homebrew:

```sh
brew install ivankuznetsov/tap/screenote
```

On macOS or Linux, install the checksum-verified release binary directly:

```sh
curl -fsSL https://screenote.ai/install.sh | sh
```

On Omarchy or another Arch-based system, you can also install the package through the AUR:

```sh
omarchy pkg aur add screenote-cli-git
```

The package provides `/usr/bin/screenote` and follows the latest CLI `main`
branch. Its reviewed `PKGBUILD` is maintained in
[`packaging/aur/screenote-cli-git`](packaging/aur/screenote-cli-git).

For source development, Go 1.26 or newer can build or install the latest source directly:

```sh
go install github.com/ivankuznetsov/screenote-cli/cmd/screenote@latest
```

Tagged GitHub releases provide prebuilt macOS and Linux binaries for AMD64 and ARM64 with SHA-256 checksums.

## Update

Check whether a newer stable release is available without changing the installation:

```sh
screenote update --check
```

Install the latest release:

```sh
screenote update
```

Homebrew installs are upgraded through Homebrew. Direct installs download the
published release installer to a temporary file, verify its GitHub-published
SHA-256 digest, pin it to the selected release, and reinstall into the current
binary directory. Known package-managed locations (Homebrew, `/usr/bin`, Nix,
Snap, and MacPorts) remain owned by their package manager so the CLI does not
overwrite managed files.

After a successful interactive command, the CLI checks for a newer release at
most once every 24 hours and prints an update suggestion to stderr at most once
per 24 hours. The check has a two-second deadline, is skipped for non-terminal
and CI invocations, and never changes the command's exit status. Set
`SCREENOTE_NO_UPDATE_CHECK=1` to disable automatic checks. Explicit
`screenote update` and `screenote update --check` commands still work.

## Authenticate

Use OAuth login from a machine with a browser:

```sh
screenote login
screenote logout
```

If a browser cannot open, the CLI writes a JSON object containing `authorization_url` to stderr so you can open it manually.

For SSH, tmux, containers, and other headless sessions, use OAuth device authorization instead. The CLI prints a short code and URL, waits while you approve the request in any browser, and then stores the same refreshable OAuth credentials as interactive login:

```sh
screenote login --device
```

The device-login prompt is JSON on stderr, so agents can read it without exposing OAuth access or refresh credentials:

```json
{"event":"device_authorization","authorization_url":"https://screenote.ai/oauth/device?user_code=ABCDE-FGHIJ","verification_uri":"https://screenote.ai/oauth/device","user_code":"ABCDE-FGHIJ","expires_in":600,"interval":5}
```

OAuth credentials are stored in `~/.config/screenote/config.toml` with file mode `0600` and refreshed automatically. Use `screenote logout` to remove them. The CLI defaults to `https://screenote.ai`; self-hosted server and project configuration can still come from `--base-url` / `--project`, `SCREENOTE_BASE_URL` / `SCREENOTE_PROJECT`, or the config file.

Ordinary commands never prompt or open a browser. Project-scoped commands require `--project`, `SCREENOTE_PROJECT`, or config `project`.

## Commands

```sh
screenote project list
screenote project create --name "Website review"
screenote --project 7 page list
screenote --project 7 screenshot create --title "Homepage" --file screenshot.png
cat screenshot.png | screenote --project 7 screenshot create --title "Homepage"
screenote --project 7 screenshot list --status ready --limit 25
screenote --project 7 annotation list --screenshot 123 --status open
screenote --project 7 annotation get --annotation 456
screenote --project 7 annotation get --annotation 456 --crop-file annotation-456.png
screenote --project 7 annotation get --annotation 456 --attachments-dir references
screenote --project 7 annotation get --annotation 456 --attachments-dir references --crop-file annotation-456.png
screenote --project 7 annotation resolve --annotation 456 --comment "Fixed in abc123"
screenote --project 7 comment add --annotation 456 --body "Fix pushed in abc123"
screenote --project 7 comment add --annotation 456 --body "Change this element" --image reference.png
cat reference.webp | screenote --project 7 comment add --annotation 456 --body "Match this treatment" --image -
screenote update --check
screenote update
```

`annotation get --crop-file PATH` decodes the annotation crop to a private local PNG (mode `0600`). Its JSON output includes `crop_file` and omits `cropped_image_base64`; without the flag, the API response is printed unchanged.

`comment add --image PATH|-` posts one PNG, JPEG, or WebP image together with
the required comment body. Images are limited to 20 MiB. The CLI spools the
input privately before sending it and makes one safe same-request retry when
delivery is ambiguous. If the result is still unknown, it returns
`comment_result_unknown`; manually rerunning the command may create another
comment. Image mode requires a server that exposes the `image-comments-v1`
contract. Older servers return `image_comments_unsupported`, and the CLI never
falls back to creating a text-only comment.

`annotation get --attachments-dir DIR` downloads every root and reply
attachment to private, deterministic `attachment-<id>.<ext>` files without
overwriting existing files. The transformed JSON preserves the thread shape,
replaces expiring media URLs with absolute `local_path` values, and can be
combined with `--crop-file` to produce one JSON document. A missing attachment
directory is created with mode `0700`; an existing directory must not be a
symlink or be group/world-writable.

Publish a browser-free multi-page capture from image files produced by your agent or automation:

```sh
screenote --project 7 snapshot --manifest snapshot.json
```

The command validates and hashes the complete manifest locally before making a request, uploads images sequentially, resumes unchanged partial work without duplicates, waits for processing, and returns a Screenote review URL. It emits JSON Lines progress to stdout; other commands retain their single-JSON-document output. See [the snapshot manifest reference](docs/snapshot-manifest.md).

Successful commands write JSON to stdout. Errors write JSON to stderr:

```json
{"code":"missing_project","error":"project is required; set --project, SCREENOTE_PROJECT, or config project"}
```

| Exit code | Meaning |
| --- | --- |
| 0 | OK |
| 1 | Generic error |
| 2 | Usage or configuration error |
| 3 | Authentication or authorization error |
| 4 | Not found |
| 5 | Rate limited |

## Development

```sh
go test ./...
go vet ./...
go run ./cmd/screenote --help
```

## License

[MIT](LICENSE)
