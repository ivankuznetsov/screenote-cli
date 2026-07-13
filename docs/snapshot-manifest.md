# Snapshot manifest

`screenote snapshot` publishes a complete capture run from PNG or JPEG files already produced by an agent or automation. The CLI does not launch a browser or capture pages itself.

## Example

```json
{
  "version": 1,
  "git_commit": "7f3a1c9",
  "taken_at": "2026-07-10T14:30:00Z",
  "images": [
    {
      "page": "Homepage",
      "title": "Homepage",
      "file": "captures/home-desktop.png",
      "viewport": "desktop"
    },
    {
      "page": "Homepage",
      "title": "Homepage",
      "file": "captures/home-mobile.jpg",
      "viewport": "mobile"
    }
  ]
}
```

Run it with an authenticated, project-selected CLI:

```sh
screenote --project 7 snapshot --manifest snapshot.json
```

Use `--wait 5m` to override the default two-minute processing wait. The maximum wait is 30 minutes. A timeout is a resumable failure: rerun the unchanged manifest.

## Fields

| Field | Rules |
| --- | --- |
| `version` | Required integer; version 1 is supported. |
| `git_commit` | Required 7-40 character hexadecimal Git commit. |
| `taken_at` | Required ISO 8601 timestamp with `Z` or an explicit numeric offset. |
| `images` | Required array containing 1-100 entries. |
| `images[].page` | Required page name, at most 255 characters. |
| `images[].title` | Optional logical screenshot title; defaults to `page`. Every viewport variant of that screenshot must use the identical title. Do not append `desktop`, `tablet`, or `mobile`. |
| `images[].file` | Required path relative to the manifest, contained within its directory. The filename may identify the viewport (for example, `home-mobile.png`). |
| `images[].viewport` | Required `desktop`, `tablet`, or `mobile`. |

Entries with the same page and title become viewport variants of one Screenote screenshot. Use one logical title for all of those entries: `"Benchmark overview"` for both desktop and mobile, never `"Benchmark overview — desktop"` and `"Benchmark overview — mobile"`. Viewport identity belongs only in `viewport` and, when useful, `file`. When separate titles on the same page look like viewport-suffixed versions of one logical title, the CLI rejects the manifest so a capture mistake cannot silently create separate screenshot cards. A lone logical title may still end in a viewport word.

A page/title group may contain each viewport at most once. Every file must be a non-empty, readable PNG or JPEG no larger than 20 MB; type is detected from bytes rather than the extension.

## Identity and resume

The CLI computes one deterministic manifest identity from normalized metadata, entry order, opaque relative-file-reference hashes, and image content SHA-256 values. Readable local paths never leave the machine.

Rerunning an unchanged manifest returns the same server graph and skips attached images. Changing the commit, timestamp, order, page, title, viewport, relative file reference, or image bytes creates a different capture identity and never mixes with partial work from the earlier run.

The CLI rechecks every image immediately before upload. If a file changed after preflight, the command stops before sending that entry.

The version-1 length-prefixed digest vectors are published in [`testdata/contracts/snapshot-digests-v1.json`](../testdata/contracts/snapshot-digests-v1.json). Other implementations should execute these vectors rather than copying digest literals into language-specific tests.

## Machine-readable output

Snapshot progress is JSON Lines on stdout: every line is one complete JSON object. Fields documented below are required for that event. IDs and `manifest_entry` are JSON numbers; all other values are strings. `manifest_entry` is the zero-based index in the normalized manifest.

| Event | Required fields | Semantics |
| --- | --- | --- |
| `snapshot_prepared` | `event`, `operation`, `snapshot_id`, `manifest_digest`, `state` | `operation` is `created` or `resumed`; `state` is the aggregate snapshot state. |
| `image_skipped` | `event`, `operation`, `snapshot_id`, `manifest_entry`, `image_id`, `viewport` | `operation` is `already_uploaded`; the attached non-failed image did not need another PUT. |
| `image_uploaded` | `event`, `operation`, `snapshot_id`, `manifest_entry`, `image_id`, `viewport`, `state` | `operation` is `uploaded`, `already_uploaded`, or `processing_retried`; `state` describes this image. |
| `snapshot_state` | `event`, `snapshot_id`, `state` | Emitted when the aggregate state changes during upload or polling. |
| `snapshot_ready` | `event`, `snapshot_id`, `state`, `review_url` | The terminal success event; `state` is `ready`. |

Snapshot states are `awaiting_upload`, `processing`, `failed`, or `ready`. Image states use the same values. A successful command emits exactly one final `snapshot_ready` event. Progress events may precede a failure, so consumers must use the process exit code and must not interpret an earlier progress line as completion.

Failures write exactly one JSON object to stderr using required `code` and `error` string fields. `operation` is included when a workflow stage had started, `manifest_entry` when one zero-based entry failed, and `snapshot_id` once server preparation succeeded. Output and errors never contain bearer credentials or readable local image paths.
