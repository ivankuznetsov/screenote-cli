# Screenote CLI

The public command-line client for [Screenote](https://screenote.ai), a visual feedback workspace for screenshots and annotations.

## Install

On Omarchy or another Arch-based system, install the package through the AUR:

```sh
omarchy pkg aur add screenote-cli-git
```

The package provides `/usr/bin/screenote` and follows the latest CLI `main`
branch. Its reviewed `PKGBUILD` is maintained in
[`packaging/aur/screenote-cli-git`](packaging/aur/screenote-cli-git).

For source development, Go 1.26 or newer can install the latest version
directly:

```sh
go install github.com/ivankuznetsov/screenote-cli/cmd/screenote@latest
```

GitHub Release binaries and Homebrew installation will be available with the first public release.

## Authenticate

Use OAuth login from a machine with a browser:

```sh
screenote --base-url https://screenote.ai login
screenote logout
```

If a browser cannot open, the CLI writes a JSON object containing `authorization_url` to stderr so you can open it manually.

For SSH, tmux, containers, and other headless sessions, use OAuth device authorization instead. The CLI prints a short code and URL, waits while you approve the request in any browser, and then stores the same refreshable OAuth credentials as interactive login:

```sh
screenote --base-url https://screenote.ai login --device
```

The device-login prompt is JSON on stderr, so agents can read it without exposing OAuth access or refresh credentials:

```json
{"event":"device_authorization","authorization_url":"https://screenote.ai/oauth/device?user_code=ABCDE-FGHIJ","verification_uri":"https://screenote.ai/oauth/device","user_code":"ABCDE-FGHIJ","expires_in":600,"interval":5}
```

OAuth credentials are stored in `~/.config/screenote/config.toml` with file mode `0600` and refreshed automatically. Use `screenote logout` to remove them. Server and project configuration can still come from `--base-url` / `--project`, `SCREENOTE_BASE_URL` / `SCREENOTE_PROJECT`, or the config file.

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
screenote --project 7 annotation resolve --annotation 456 --comment "Fixed in abc123"
screenote --project 7 comment add --annotation 456 --body "Fix pushed in abc123"
```

`annotation get --crop-file PATH` decodes the annotation crop to a private local PNG (mode `0600`). Its JSON output includes `crop_file` and omits `cropped_image_base64`; without the flag, the API response is printed unchanged.

Publish a browser-free multi-page capture from image files produced by your agent or automation:

```sh
screenote --project 7 snapshot --manifest snapshot.json
```

The command validates and hashes the complete manifest locally before making a request, uploads images sequentially, resumes unchanged partial work without duplicates, waits for processing, and returns a Screenote review URL. It emits JSON Lines progress to stdout; other commands retain their single-JSON-document output. See [the snapshot manifest reference](docs/snapshot-manifest.md).

Successful commands write JSON to stdout. Errors write JSON to stderr:

```json
{"code":"missing_base_url","error":"base URL is required; set --base-url, SCREENOTE_BASE_URL, or config base_url"}
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
