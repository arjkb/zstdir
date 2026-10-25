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
   ```

   This creates the tap locally, in `$(brew --repository arjkb/homebrew-tap)`,
   wherever it is run from. Publish it with `gh`:

   ```sh
   gh repo create arjkb/homebrew-tap --public --push \
     --source "$(brew --repository arjkb/homebrew-tap)"
   ```

   or, without `gh`, create an empty public `homebrew-tap` repository on
   GitHub and push to it:

   ```sh
   cd "$(brew --repository arjkb/homebrew-tap)"
   git remote add origin git@github.com:arjkb/homebrew-tap.git
   git push -u origin main
   ```

3. In the tap repository's **Settings → Actions → General → Workflow
   permissions**, select **Read and write permissions**. The publish
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

   ```sh
   cd "$(brew --repository arjkb/homebrew-tap)"
   git checkout main && git pull
   git checkout -b zstdir-X.Y.Z
   ```

   - **First release:** copy `packaging/homebrew/zstdir.rb` to
     `Formula/zstdir.rb` in the tap, drop the two template comment lines at
     the top, then set `url` and `sha256`.
   - **Later releases:** change `url` and `sha256` in `Formula/zstdir.rb`.
     Leave the `bottle do` block alone; it is rewritten when bottles are
     published.

   `sha256` is only the 64-character hash `shasum` prints, not the `-`
   after it. `brew style arjkb/tap/zstdir` checks the formula without
   building it. A full local check is optional, since the pull request's CI
   does the same, and it installs Homebrew's own Go as a build dependency
   (`brew autoremove` removes it afterwards):

   ```sh
   brew install --build-from-source arjkb/tap/zstdir
   brew test arjkb/tap/zstdir
   brew audit --strict --online arjkb/tap/zstdir   # add --new for the first release
   ```

5. Commit as `zstdir X.Y.Z` (`zstdir X.Y.Z (new formula)` for the first),
   push the branch and open a pull request in the tap. Its CI
   (`brew test-bot`) builds and tests bottles for macOS and Linux.
6. When the checks pass, publish: in the tap's **Actions** tab, select
   **brew pr-pull**, click **Run workflow**, and enter the pull request
   number. (No label is needed; the workflow only runs by hand.) It:
   - uploads the bottles to a release in the tap;
   - pushes the formula with its `bottle do` block to `main`;
   - closes the pull request. GitHub shows it as "closed with unmerged
     commits" because the commits were rewritten; that is expected.
7. Delete the pull request's branch on GitHub, and update the local tap:

   ```sh
   cd "$(brew --repository arjkb/homebrew-tap)"
   git checkout main && git pull
   git branch -D zstdir-X.Y.Z
   ```

`brew upgrade` will now pick up the new version.
