# zstdir

Compress and extract directories with [zstd](https://facebook.github.io/zstd/) in one step.

```sh
zstdir photos              # photos/ -> photos.tar.zst
zstdir photos.tar.zst      # photos.tar.zst -> photos/
```

zstd compresses single files, so a directory normally needs tar as well:
`tar -I 'zstd -T0' -cf photos.tar.zst photos`. zstdir does both in one step, and
picks the mode from its argument: directories are compressed, zstd archives
are extracted.

The archives are plain `.tar.zst`. Anything that reads those reads zstdir's
output (`tar --zstd -xf`, `zstd -dc | tar -x`, bsdtar), and zstdir extracts
archives those tools make.

## Install

**Homebrew** (macOS, and Linux with Homebrew):

```sh
brew install arjkb/tap/zstdir
```

**From source** (Go 1.26 or later), on macOS or Linux:

```sh
go install github.com/arjkb/zstdir@latest
ln -s zstdir "$(go env GOPATH)/bin/unzstdir"   # optional alias
```

## Usage

```
zstdir [options] DIR...          compress each DIR into DIR.tar.zst
zstdir [options] ARCHIVE...      extract each archive into a directory
zstdir -d [options] [ARCHIVE|-]  extract; "-" or no argument reads stdin
unzstdir [options] ARCHIVE...    same as zstdir -d
```

| Option | |
|---|---|
| `-d`, `--decompress` | force extraction; with no argument, read stdin |
| `-o`, `--output PATH` | archive name when compressing, directory name when extracting |
| `-c`, `--stdout` | write the archive to stdout |
| `-f`, `--force` | overwrite an existing archive; allow terminal I/O |
| `-v`, `--verbose` | list entries on stderr; `-vv` adds a summary |
| `-q`, `--quiet` | suppress warnings |
| `-L`, `--level N` | compression level 1–22 (default 3) |
| `-T`, `--threads N` | threads; 0 = one per CPU, up to 8 (default) |

Examples:

```sh
zstdir -o backup.tar.zst photos         # name the archive
zstdir -o restored backup.tar.zst       # name the extracted directory
zstdir -L 19 -vv photos                 # smaller archive, with a summary
zstdir -c project | ssh host zstdir -d  # copy a directory to another host
zstdir src docs assets                  # one archive per directory
```

See `man zstdir` for the full reference.

## Behavior

It follows the usual Unix conventions:

- Quiet unless something goes wrong. Messages go to stderr, so stdout can carry an archive.
- Exit status is 0 on success, 1 on error, 2 on a usage error, and 130 when interrupted. With several inputs, a failure doesn't stop the rest.
- Outputs go next to their inputs, like `gzip` and `zstd`.
- Existing archives are only overwritten with `-f`. Existing directories are never replaced.
- Work happens in a temporary file or directory that is renamed into place only on success, so an error or Ctrl-C leaves nothing half-written.

When extracting:

- **Placement:** an archive's single top-level directory becomes the output directory. An archive with several top-level entries is extracted *into* a new directory instead of scattering them into the current one.
- **Path safety:** entries that would escape the output directory are rejected. That covers `..`, absolute paths and symlink tricks, and is enforced by Go's `os.Root`.
- **Metadata:**
  - permission bits and modification times are restored;
  - ownership, setuid/setgid bits, extended attributes and ACLs are not;
  - the `._*` AppleDouble files that macOS `tar` adds are skipped.

When compressing:

- Symlinks are stored as links.
- Devices, sockets and FIFOs are skipped with a warning.
- Hard links are stored as separate files.

## Performance

zstdir uses goroutines for:

- reading files ahead of the tar writer;
- compressing 4 MiB chunks of the stream in parallel, as independent zstd frames. This is the approach `pzstd` uses, and any zstd decoder reads the frames as one stream;
- writing extracted files several at a time.

Measured on an Apple M3 Pro, warm cache, level 3, against `tar` piped through `zstd -T0`:

| | zstdir | tar + zstd |
|---|---|---|
| Go source tree, 12k files, 156 MB: compress | 0.18 s | 4.6 s |
| Go source tree: extract | 0.51 s | 1.2 s |
| 6 large files, 970 MB: compress | 0.39 s | 0.40 s |
| 6 large files: extract | 0.35 s | 0.63 s |

Splitting the stream into chunks costs up to about 2% in compression ratio. Use
`-T 1` for a single-frame archive.

## Development

```sh
go test -race ./...
go build
```

`internal/archive` holds the tar+zstd logic and `main.go` the CLI. Some tests
compare against the system `tar` and `zstd` and are skipped if those are
missing. See [RELEASING.md](RELEASING.md) for how releases are made.

## License

[GPL-3.0](LICENSE)
