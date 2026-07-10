# Screenote CLI

The public command-line client for [Screenote](https://screenote.ai), a visual feedback workspace for screenshots and annotations.

## Install

Install the latest tagged release with Go:

```sh
go install github.com/ivankuznetsov/screenote-cli/cmd/screenote@latest
```

GitHub Release binaries and Homebrew installation will be available with the first public release.

## Authenticate

Interactive OAuth login:

```sh
screenote --base-url https://screenote.ai login
screenote logout
```

If a browser cannot open, the CLI writes a JSON object containing `authorization_url` to stderr so you can open it manually.

CI and agents can use a pre-provisioned OAuth bearer token:

```sh
screenote config set --base-url https://screenote.ai --token "$SCREENOTE_TOKEN"
screenote config
```

`screenote config` reports whether a token is set and where it came from, but never prints the token.

Configuration precedence is:

1. Flags: `--token`, `--base-url`, `--project`
2. Environment: `SCREENOTE_TOKEN`, `SCREENOTE_BASE_URL`, `SCREENOTE_PROJECT`
3. Config file: `~/.config/screenote/config.toml`
4. Stored credentials from `screenote login`

Ordinary commands never prompt or open a browser. Project-scoped commands require `--project`, `SCREENOTE_PROJECT`, or config `project`.

## Commands

```sh
screenote project list
screenote --project 7 page list
screenote --project 7 screenshot create --title "Homepage" --file screenshot.png
cat screenshot.png | screenote --project 7 screenshot create --title "Homepage"
screenote --project 7 screenshot list --status ready --limit 25
screenote --project 7 annotation list --screenshot 123 --status open
screenote --project 7 annotation get --annotation 456
screenote --project 7 comment add --annotation 456 --body "Fix pushed in abc123"
```

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
