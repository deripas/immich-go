# Replace Command

The `replace` command swaps the file of an existing asset for a local file, while keeping the metadata the server holds for it.

## Syntax

```bash
immich-go replace --asset=<asset-id> [options] <file>
```

## Purpose

Useful when a file has been reworked outside of Immich and the result should take the place of the original:

- re-encoding a video with another codec (H.264 → H.265)
- cropping a 3D/stereoscopic video down to a single eye
- any other transformation done with `ffmpeg`, an editor, or a script

Immich removed the replace endpoint (`PUT /assets/{id}/original`) in the v3 series. The supported sequence, which this command implements, is:

1. upload the new file — the server gives it a brand new ID
2. `PUT /assets/copy` — the server carries over what it can
3. patch by hand what the copy leaves behind
4. move the replaced asset to the trash

## Required Options

| Option          | Required | Description                  |
| --------------- | :------: | ---------------------------- |
| `-s, --server`  |    Y     | Immich server URL            |
| `-k, --api-key` |    Y     | Your API key                 |
| `--asset`       |    Y     | ID of the asset to replace   |

The API key needs the `asset.copy` permission, on top of the usual upload and delete ones.

## Behavior Options

| Option       | Default            | Description                                          |
| ------------ | ------------------ | ---------------------------------------------------- |
| `--dry-run`  | `false`            | Report what would be done without changing the server |
| `--filename` | Local file name    | Original file name to give to the server              |

## What is carried over

| Metadata                                             | Carried over by |
| ---------------------------------------------------- | --------------- |
| Albums, stack, shared links, sidecar, favorite flag   | the server, through `/assets/copy` |
| Capture date, description, rating, GPS, visibility    | immich-go, through `/assets/{id}` |
| Tags                                                  | immich-go, through `/tags/assets` |
| Codec, resolution, duration, file size                | the server, extracted from the new file |
| Faces and people                                      | **not carried over** — the server runs its recognition again |

The capture date deserves a note: a transcoder output doesn't carry the date of the original shot, and a date fixed by hand in Immich lives on the server only. The command therefore takes the capture date from the asset being replaced and sends it along with the upload, rather than letting the server read it from the new file.

## Safety

- The replaced asset is moved to the **trash**, not deleted permanently, and only once every other step has succeeded.
- If the new file is byte-identical to an asset already on the server, the command stops without changing anything.
- Should a step fail midway, both assets are left on the server and their IDs are reported, so nothing is lost.

## Examples

```bash
# See what would happen, without touching the server
immich-go replace --server=http://localhost:2283 --api-key=your-key \
  --asset=6f3a1b2c-... --dry-run left-eye.mp4

# Replace a stereoscopic video by its cropped left eye
ffmpeg -i stereo.mp4 -vf "crop=iw/2:ih:0:0" -c:a copy left-eye.mp4
immich-go replace --server=http://localhost:2283 --api-key=your-key \
  --asset=6f3a1b2c-... left-eye.mp4

# Replace a video by an H.265 re-encode, keeping the name it has on the server
ffmpeg -i input.mp4 -c:v libx265 -crf 28 -c:a copy output.mp4
immich-go replace --server=http://localhost:2283 --api-key=your-key \
  --asset=6f3a1b2c-... --filename=input.mp4 output.mp4
```

## Related

- [Upload Command](upload.md) — `--overwrite` replaces a server asset when the local file has the same name and capture date, and is larger
