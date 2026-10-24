# Releasing zstdir

A release is a git tag, and Homebrew is how it reaches users. The formula
in the `arjkb/homebrew-tap` repository builds zstdir from the source tarball
GitHub generates for each tag. That tap's CI builds **bottles** (prebuilt
binaries), so users don't compile anything or need Go.

The version number comes only from the tag. The formula passes it to the
build with `-ldflags "-X main.version=..."`, and no file in this repository
needs bumping.

## One-time setup

1. Push this repository to `github.com/arjkb/zstdir`.
2. Create the tap. `brew tap-new` generates a repository whose GitHub
   Actions build and publish bottles:

   ```sh
   brew tap-new arjkb/homebrew-tap
   gh repo create arjkb/homebrew-tap --public --push \
     --source "$(brew --repository arjkb/homebrew-tap)"
   ```

3. In the tap repository's settings, go to **Actions → General → Workflow
   permissions** and select **Read and write permissions**. The publish
   workflow needs this to upload bottles and push the formula update.

Users then install with `brew install arjkb/tap/zstdir`. Homebrew maps
`arjkb/tap` to the `arjkb/homebrew-tap` repository.

## Each release

1. Make sure `main` is green in CI. Update the date (`.Dd`) in
   `man/zstdir.1` if the man page changed.
2. Tag and push:

   ```sh
   git tag -a v0.1.0 -m "zstdir 0.1.0"
   git push origin v0.1.0
   ```

   Optionally, create a GitHub release for the tag with release notes
   (`gh release create v0.1.0 --generate-notes`). Homebrew doesn't need one.
3. Get the checksum of the source tarball the formula builds from:

   ```sh
   curl -fsSL https://github.com/arjkb/zstdir/archive/refs/tags/v0.1.0.tar.gz | shasum -a 256
   ```

4. Update the formula in the tap, on a branch:
   - **First release:** copy `packaging/homebrew/zstdir.rb` to
     `Formula/zstdir.rb` in the tap, then set `url` and `sha256`.
   - **Later releases:** change `url` and `sha256` in `Formula/zstdir.rb`.
     Leave the `bottle do` block alone; it is rewritten when bottles are
     published.

   Check it locally:

   ```sh
   brew install --build-from-source arjkb/tap/zstdir
   brew test arjkb/tap/zstdir
   brew audit --strict --online arjkb/tap/zstdir   # add --new for the first release
   ```

5. Open a pull request in the tap with the formula change. Its CI
   (`brew test-bot`) builds and tests bottles.
6. When the checks pass, add the **`pr-pull`** label to the pull request.
   The publish workflow then:
   - uploads the bottles to a release in the tap;
   - pushes the formula with its `bottle do` block to `main`;
   - closes the pull request.

`brew upgrade` will now pick up the new version.
