# Asset Command

The `asset` command works on a single asset of the server, designated by its ID.

## Syntax

```bash
immich-go asset list    [filters] [options]
immich-go asset show    --asset=<asset-id> [options]
immich-go asset clone   --asset=<asset-id> [options] <file>
immich-go asset replace --asset=<asset-id> [options] <file>
```

## Sub-commands

| Sub-command | Description |
| ----------- | ----------- |
| `list`      | List the assets matching a set of filters, optionally as a CSV |
| `show`      | Print the metadata the server holds for an asset |
| `clone`     | Upload a file and give it the metadata of an existing asset, leaving the source untouched |
| `replace`   | The same, then move the source asset to the trash |

## Purpose

`clone` and `replace` are for a file reworked outside of Immich that should take the place of the original:

- re-encoding a video with another codec (H.264 → H.265)
- cropping a 3D/stereoscopic video down to a single eye
- any other transformation done with `ffmpeg`, an editor, or a script

Immich removed the replace endpoint (`PUT /assets/{id}/original`) in the v3 series. The supported sequence, which these sub-commands implement, is:

1. upload the new file — the server gives it a brand new ID
2. `PUT /assets/copy` — the server carries over what it can
3. patch by hand what the copy leaves behind
4. `replace` only: move the source asset to the trash

Start with `clone --stack`: both assets end up side by side in the web interface, which makes the result easy to check before trusting `replace`.

## Required Options

| Option          | Required        | Description                |
| --------------- | :-------------: | -------------------------- |
| `-s, --server`  |        Y        | Immich server URL          |
| `-k, --api-key` |        Y        | Your API key               |
| `--asset`       | show, clone, replace | ID of the asset to work on |

`clone` and `replace` need an API key with the `asset.copy` permission, on top of the usual upload and delete ones.

## Selecting assets with `list`

`list` takes the filters of `upload from-immich`, with the same names and meaning:
`--from-tags`, `--from-albums`, `--from-make`, `--from-model`, `--from-country`,
`--from-state`, `--from-city`, `--from-archived`, `--from-favorite`, `--from-trash`,
`--from-no-album`, `--from-minimal-rating`, `--from-partners`, `--date-range` and
`--include-type`.

The report goes to the terminal, sorted by capture date. `--export=<file.csv>` also
writes it as a CSV. The command refuses to overwrite an existing export, because it
is meant to be filled in by hand afterwards.

| Column                        | Description |
| ----------------------------- | ----------- |
| `id`                          | Asset ID, what `clone` and `replace` take as `--asset` |
| `name`, `type`                | Original file name, `IMAGE` or `VIDEO` |
| `capture_date`                | The date shown in the properties panel, with the offset it was shot at |
| `timeline_date`               | The date the asset is grouped on in the timeline |
| `width`, `height`, `size`     | Dimensions and file size in bytes |
| `checksum`                    | Lets a second run tell which rows are already done |
| `server_path`                 | Where the file sits in the server's storage |
| `new_file`                    | Empty, to be filled with the path of the converted file |

The two dates are exported separately on purpose: they disagree whenever the capture
date was edited after the server extracted the metadata of the file, and that is
worth spotting before running a batch.

`server_path` is the path as the **server** sees it, which inside a container looks
like `/usr/src/app/upload/...`. If the Immich folder is mounted here, rewrite it with
`--server-path-prefix` and `--local-path-prefix`, and `ffmpeg` can read the column
directly. The two go together, and a path outside the given prefix — an external
library — is reported unchanged.

## Behavior Options

| Option       | Applies to      | Default         | Description                                           |
| ------------ | --------------- | --------------- | ----------------------------------------------------- |
| `--dry-run`  | all             | `false`         | Report what would be done without changing the server |
| `--filename` | clone, replace  | Local file name | Original file name to give to the server              |
| `--stack`    | clone           | `false`         | Stack the new asset with the source one               |

## What is carried over

| Metadata                                           | Carried over by |
| -------------------------------------------------- | --------------- |
| Albums, stack, shared links, sidecar, favorite flag | the server, through `/assets/copy` |
| Capture date                                        | immich-go, through an uploaded XMP sidecar |
| Description, rating, GPS                            | immich-go, through `/assets/{id}` |
| Tags                                                | immich-go, through `/tags/assets` |
| Codec, resolution, duration, file size              | the server, extracted from the new file |
| Faces and people                                    | **not carried over** — the server runs its recognition again |

### Why the capture date needs a sidecar

Immich keeps the date twice. `asset_exif.dateTimeOriginal` is what the properties panel shows, and `asset.localDateTime` is what the timeline groups on — the same wall clock, stored as if it were UTC.

`localDateTime` is written in exactly two places: when the asset is created, and when the server extracts the metadata of the file. **No API recomputes it.** Setting the capture date through `PUT /assets/{id}` or the bulk `PUT /assets` only writes `asset_exif`, so on its own it moves the date shown in the properties panel and leaves the asset where it was on the timeline.

A file coming out of a transcoder carries the date of the transcoding, or a bogus one, or none — and the metadata extraction runs asynchronously, after the upload. Left alone it would overwrite `localDateTime` with whatever it read from the new file.

So immich-go uploads a small XMP sidecar holding the capture date of the source asset. The extraction explicitly prefers the dates of a sidecar and drops the ones embedded in the media, which makes it compute the right `localDateTime` the first time. The capture date is also set through `/assets/{id}` afterwards, which locks it against any later extraction.

If the date still looks wrong, check it with `immich-go asset show`: it prints the capture date and the timeline date separately, so a disagreement between the two is visible.

## Safety

- `replace` moves the source asset to the **trash**, not away, and only once every other step has succeeded.
- If the new file is byte-identical to an asset already on the server, the command stops without changing anything.
- Should a step fail midway, both assets are left on the server and their IDs are reported, so nothing is lost.

## Examples

```bash
# Select the stereoscopic videos and write a conversion plan
immich-go asset list --server=http://localhost:2283 --api-key=your-key \
  --from-tags=3d --include-type=VIDEO \
  --server-path-prefix=/usr/src/app/upload --local-path-prefix=/mnt/tank/immich \
  --export=./work/plan.csv

# What does the server hold for this asset?
immich-go asset show --server=http://localhost:2283 --api-key=your-key --asset=6f3a1b2c-...

# Crop a stereoscopic video to its left eye, upload it next to the original and stack the two
ffmpeg -i stereo.mp4 -vf "crop=iw/2:ih:0:0" -c:a copy left-eye.mp4
immich-go asset clone --server=http://localhost:2283 --api-key=your-key \
  --asset=6f3a1b2c-... --stack left-eye.mp4

# Happy with the result: do it for real
immich-go asset replace --server=http://localhost:2283 --api-key=your-key \
  --asset=6f3a1b2c-... left-eye.mp4

# Re-encode to H.265, keeping the name the asset has on the server
ffmpeg -i input.mp4 -c:v libx265 -crf 28 -c:a copy output.mp4
immich-go asset replace --server=http://localhost:2283 --api-key=your-key \
  --asset=6f3a1b2c-... --filename=input.mp4 --dry-run output.mp4
```

## Related

- [Upload Command](upload.md) — `--overwrite` replaces a server asset when the local file has the same name and capture date, and is larger
